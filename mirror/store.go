// Package mirror keeps the mirror feed of the home daemon (OVERSIRE-525). The app that holds the automations lease
// (the source) puts the provider data in it. Each client app reads it with a cursor. The daemon does not read the bodies.
package mirror

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
	_ "modernc.org/sqlite"
)

const (
	// PageLimit is the most docs in one page.
	PageLimit = 500
	// MaxDocs, MaxDocBytes and MaxBatchBytes are the limits of one put.
	MaxDocs       = 500
	MaxDocBytes   = 1 << 20
	MaxBatchBytes = 8 << 20
	// TombstoneTTL is the time that a deleted doc stays in the feed. A reader that is older has to read all again.
	TombstoneTTL = 7 * 24 * time.Hour
	// ReaderTTL is the time that a reader that is not seen stays in the head.
	ReaderTTL = 30 * 24 * time.Hour

	pageBytes = 8 << 20
)

var (
	ErrEpoch    = errors.New("the epoch is not the epoch of the mirror")
	ErrTooLarge = errors.New("the batch is too large")
	ErrBadDoc   = errors.New("the doc is not valid")
)

// Source is the sender of the last accepted put. App is put.Source, the app device that holds the lease.
// Device, Name and Machine come from the caller of the request. A loopback caller has no device.
type Source struct {
	App, Device, Name, Machine string
	Loopback                   bool
}

// Store is the mirror feed in SQLite.
type Store struct {
	db    *sql.DB
	clock func() time.Time
	mu    sync.Mutex // one write at a time, so that a batch is atomic and seq grows
	Log   *slog.Logger
}

// Open opens or makes the feed at path (<data>/mirror.db). A new database gets a random epoch.
func Open(path string, clock func() time.Time) (*Store, error) {
	if clock == nil {
		clock = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS docs (kind TEXT NOT NULL, id TEXT NOT NULL, v INTEGER NOT NULL, hash TEXT NOT NULL,
			seq INTEGER NOT NULL, at INTEGER NOT NULL, stored INTEGER NOT NULL, deleted INTEGER NOT NULL, body TEXT,
			PRIMARY KEY (kind, id));
		CREATE INDEX IF NOT EXISTS docs_seq ON docs(seq);
		CREATE TABLE IF NOT EXISTS meta (id INTEGER PRIMARY KEY CHECK (id = 1), epoch TEXT NOT NULL, head INTEGER NOT NULL,
			floor INTEGER NOT NULL, ready INTEGER NOT NULL, source TEXT NOT NULL, source_name TEXT NOT NULL,
			source_device TEXT NOT NULL, source_caller TEXT NOT NULL, source_machine TEXT NOT NULL, source_loopback INTEGER NOT NULL,
			app_version TEXT NOT NULL, published_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS readers (machine TEXT NOT NULL, device TEXT NOT NULL, name TEXT NOT NULL,
			head INTEGER NOT NULL, seen_at INTEGER NOT NULL, PRIMARY KEY (machine, device));`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("mirror: open %s: %w", path, err)
	}
	epoch, err := newEpoch()
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (id, epoch, head, floor, ready, source, source_name, source_device, source_caller,
		source_machine, source_loopback, app_version, published_at) VALUES (1, ?, 0, 0, 0, '', '', '', '', '', 0, '', 0)`, epoch); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, clock: clock, Log: slog.Default()}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newEpoch() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type meta struct {
	epoch                      string
	head, floor                int64
	ready                      bool
	source, sourceName         string
	sourceDevice, sourceCaller string
	sourceMachine, appVersion  string
	sourceLoopback             bool
	publishedAt                int64
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readMeta(q queryer) (m meta, err error) {
	err = q.QueryRow(`SELECT epoch, head, floor, ready, source, source_name, source_device, source_caller, source_machine,
		source_loopback, app_version, published_at FROM meta WHERE id = 1`).Scan(&m.epoch, &m.head, &m.floor, &m.ready, &m.source,
		&m.sourceName, &m.sourceDevice, &m.sourceCaller, &m.sourceMachine, &m.sourceLoopback, &m.appVersion, &m.publishedAt)
	return m, err
}

func (s *Store) fail(what string, err error) {
	if s.Log != nil {
		s.Log.Error("mirror: "+what, "err", err)
	}
}

// check applies the limits and the rules of the contract. It reads no state.
func check(p wire.MirrorPut) error {
	if len(p.Docs) > MaxDocs {
		return fmt.Errorf("%w: more than %d docs", ErrTooLarge, MaxDocs)
	}
	total := 0
	for i, d := range p.Docs {
		if len(d.Body) > MaxDocBytes {
			return fmt.Errorf("%w: docs[%d] is over %d bytes", ErrTooLarge, i, MaxDocBytes)
		}
		if total += len(d.Body); total > MaxBatchBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxBatchBytes)
		}
	}
	for i, d := range p.Docs {
		switch {
		case !d.Kind.Valid():
			return fmt.Errorf("%w: docs[%d].kind %q", ErrBadDoc, i, d.Kind)
		case d.ID == "":
			return fmt.Errorf("%w: docs[%d].id is empty", ErrBadDoc, i)
		case d.V < 1:
			return fmt.Errorf("%w: docs[%d].v is below 1", ErrBadDoc, i)
		case d.Deleted && len(d.Body) > 0:
			return fmt.Errorf("%w: docs[%d] is deleted and has a body", ErrBadDoc, i)
		case !d.Deleted && !isObject(d.Body):
			return fmt.Errorf("%w: docs[%d].body is not an object", ErrBadDoc, i)
		}
	}
	return nil
}

func isObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// Put writes the docs that changed, in one transaction. The daemon sets hash and seq. A doc with the same hash and v is a no-op.
// The source is recorded for each accepted put, also when no doc changed.
func (s *Store) Put(p wire.MirrorPut, src Source) (wire.MirrorPutResult, error) {
	if err := check(p); err != nil {
		return wire.MirrorPutResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return wire.MirrorPutResult{}, err
	}
	defer tx.Rollback()
	m, err := readMeta(tx)
	if err != nil {
		return wire.MirrorPutResult{}, err
	}
	if p.Epoch != m.epoch {
		return wire.MirrorPutResult{}, ErrEpoch
	}
	now := s.clock().UnixMilli()
	var applied, same int
	for _, d := range p.Docs {
		var body any // NULL for a tombstone
		hash := ""
		if !d.Deleted {
			var b bytes.Buffer
			if err := json.Compact(&b, d.Body); err != nil {
				return wire.MirrorPutResult{}, fmt.Errorf("%w: %s/%s: %v", ErrBadDoc, d.Kind, d.ID, err)
			}
			sum := sha256.Sum256(b.Bytes())
			hash, body = hex.EncodeToString(sum[:]), b.String()
		}
		var curHash string
		var curV int
		var curDeleted bool
		err := tx.QueryRow("SELECT hash, v, deleted FROM docs WHERE kind = ? AND id = ?", string(d.Kind), d.ID).Scan(&curHash, &curV, &curDeleted)
		found := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return wire.MirrorPutResult{}, err
		}
		if d.Deleted && (!found || curDeleted) || !d.Deleted && found && !curDeleted && curHash == hash && curV == d.V {
			same++
			continue
		}
		m.head++
		if _, err := tx.Exec(`INSERT INTO docs (kind, id, v, hash, seq, at, stored, deleted, body) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(kind, id) DO UPDATE SET v = excluded.v, hash = excluded.hash, seq = excluded.seq, at = excluded.at,
				stored = excluded.stored, deleted = excluded.deleted, body = excluded.body`,
			string(d.Kind), d.ID, d.V, hash, m.head, d.At.UnixMilli(), now, d.Deleted, body); err != nil {
			return wire.MirrorPutResult{}, err
		}
		applied++
	}
	// A new source has to send all its docs and say so.
	m.ready = p.Ready || m.ready && m.source == p.Source
	if _, err := tx.Exec(`UPDATE meta SET head = ?, ready = ?, source = ?, source_name = ?, source_device = ?, source_caller = ?,
		source_machine = ?, source_loopback = ?, app_version = ?, published_at = ? WHERE id = 1`,
		m.head, m.ready, p.Source, p.SourceName, src.Device, src.Name, src.Machine, src.Loopback, p.AppVersion, now); err != nil {
		return wire.MirrorPutResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return wire.MirrorPutResult{}, err
	}
	return wire.MirrorPutResult{Head: s.Head(), Applied: applied, Same: same}, nil
}

// Head gives the state of the feed. SourceOnline is not set: the routes know it.
func (s *Store) Head() wire.MirrorHead {
	m, err := readMeta(s.db)
	if err != nil {
		s.fail("head", err)
		return wire.MirrorHead{}
	}
	h := wire.MirrorHead{Epoch: m.epoch, Head: m.head, Floor: m.floor, Ready: m.ready, Source: m.source, SourceName: m.sourceName,
		SourceMachine: m.sourceMachine, AppVersion: m.appVersion}
	if m.publishedAt != 0 {
		t := time.UnixMilli(m.publishedAt).UTC()
		h.PublishedAt = &t
	}
	rows, err := s.db.Query("SELECT machine, device, name, head, seen_at FROM readers WHERE seen_at >= ? ORDER BY machine, device",
		s.clock().Add(-ReaderTTL).UnixMilli())
	if err != nil {
		s.fail("readers", err)
		return h
	}
	defer rows.Close()
	for rows.Next() {
		var r wire.MirrorReader
		var seen int64
		if err := rows.Scan(&r.Machine, &r.Device, &r.Name, &r.Head, &seen); err != nil {
			s.fail("readers", err)
			return h
		}
		r.SeenAt = time.UnixMilli(seen).UTC()
		h.Readers = append(h.Readers, r)
	}
	return h
}

// Source gives the sender of the last accepted put. ok is false before the first put.
func (s *Store) Source() (src Source, ok bool) {
	m, err := readMeta(s.db)
	if err != nil || m.source == "" {
		return Source{}, false
	}
	return Source{App: m.source, Device: m.sourceDevice, Name: m.sourceCaller, Machine: m.sourceMachine, Loopback: m.sourceLoopback}, true
}

// Page gives the docs with seq above after, up to limit (default and most PageLimit). A page also ends after 8 MiB of bodies.
// Reset is true when the reader has to drop its docs and read from 0: epoch is not the epoch of the feed (an empty epoch is a
// first read), or after is above the head, or after is below the floor. A reset page has no docs and next is 0.
func (s *Store) Page(after int64, limit int, epoch string) (wire.MirrorPage, error) {
	if limit <= 0 || limit > PageLimit {
		limit = PageLimit
	}
	m, err := readMeta(s.db)
	if err != nil {
		return wire.MirrorPage{}, err
	}
	after = max(after, 0)
	stale := after > m.head || after > 0 && (epoch == "" || after < m.floor)
	if epoch != "" && epoch != m.epoch || stale {
		return wire.MirrorPage{Epoch: m.epoch, Docs: []wire.MirrorDoc{}, Reset: true}, nil
	}
	rows, err := s.db.Query("SELECT kind, id, v, hash, seq, at, deleted, body FROM docs WHERE seq > ? ORDER BY seq LIMIT ?", after, limit+1)
	if err != nil {
		return wire.MirrorPage{}, err
	}
	defer rows.Close()
	p := wire.MirrorPage{Epoch: m.epoch, Docs: []wire.MirrorDoc{}}
	size := 0
	for rows.Next() {
		var d wire.MirrorDoc
		var at int64
		var body sql.NullString
		if err := rows.Scan(&d.Kind, &d.ID, &d.V, &d.Hash, &d.Seq, &at, &d.Deleted, &body); err != nil {
			return wire.MirrorPage{}, err
		}
		if len(p.Docs) == limit || len(p.Docs) > 0 && size+len(body.String) > pageBytes {
			p.More = true
			break
		}
		d.At = time.UnixMilli(at).UTC()
		if body.Valid {
			d.Body = json.RawMessage(body.String)
		}
		size += len(body.String)
		p.Docs = append(p.Docs, d)
	}
	if err := rows.Err(); err != nil {
		return wire.MirrorPage{}, err
	}
	p.Next = max(after, m.head)
	if p.More {
		p.Next = p.Docs[len(p.Docs)-1].Seq
	}
	return p, nil
}

// Index gives the kind, id and hash of each live doc.
func (s *Store) Index() wire.MirrorIndex {
	m, err := readMeta(s.db)
	idx := wire.MirrorIndex{Epoch: m.epoch, Head: m.head, Docs: []wire.MirrorIndexEntry{}}
	if err != nil {
		s.fail("index", err)
		return idx
	}
	rows, err := s.db.Query("SELECT kind, id, hash FROM docs WHERE deleted = 0 ORDER BY kind, id")
	if err != nil {
		s.fail("index", err)
		return idx
	}
	defer rows.Close()
	for rows.Next() {
		var e wire.MirrorIndexEntry
		if err := rows.Scan(&e.Kind, &e.ID, &e.Hash); err != nil {
			s.fail("index", err)
			break
		}
		idx.Docs = append(idx.Docs, e)
	}
	return idx
}

// Seen records a reader, with its time as SeenAt. A negative Head keeps the head that the store has for the reader.
func (s *Store) Seen(r wire.MirrorReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	_, err := s.db.Exec(`INSERT INTO readers (machine, device, name, head, seen_at) VALUES (?, ?, ?, MAX(?, 0), ?)
		ON CONFLICT(machine, device) DO UPDATE SET name = excluded.name, seen_at = excluded.seen_at,
			head = CASE WHEN ? < 0 THEN head ELSE excluded.head END`,
		r.Machine, r.Device, r.Name, r.Head, now.UnixMilli(), r.Head)
	if err == nil {
		_, err = s.db.Exec("DELETE FROM readers WHERE seen_at < ?", now.Add(-ReaderTTL).UnixMilli())
	}
	if err != nil {
		s.fail("seen", err)
	}
}

// Prune drops the tombstones that are older than TombstoneTTL and the readers that are not seen for ReaderTTL.
// It moves the floor up to the last seq that it dropped.
func (s *Store) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cutoff := now.Add(-TombstoneTTL).UnixMilli()
	var top sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(seq) FROM docs WHERE deleted = 1 AND stored < ?", cutoff).Scan(&top); err != nil {
		return err
	}
	if top.Valid {
		if _, err := tx.Exec("DELETE FROM docs WHERE deleted = 1 AND stored < ?", cutoff); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE meta SET floor = MAX(floor, ?) WHERE id = 1", top.Int64); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM readers WHERE seen_at < ?", now.Add(-ReaderTTL).UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// Reset starts the feed again: a new epoch, no docs, no readers and no source.
func (s *Store) Reset() error {
	epoch, err := newEpoch()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM docs", "DELETE FROM readers"} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET epoch = ?, head = 0, floor = 0, ready = 0, source = '', source_name = '', source_device = '',
		source_caller = '', source_machine = '', source_loopback = 0, app_version = '', published_at = 0 WHERE id = 1`, epoch); err != nil {
		return err
	}
	return tx.Commit()
}

// TakeHome implements group.HomeTaker. The mirror pulls nothing from the old home: the source sends all its docs again
// to the new home, so the old data cannot come back.
func (s *Store) TakeHome(_ context.Context, _, _ string) (records int, offline bool, err error) {
	return 0, false, s.Reset()
}
