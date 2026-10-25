// Package store keeps the synced store of the group on the home daemon. It uses the same API and rules as the cloud hub
// (cloud/src/store.ts): last write wins for each collection and id, by HLC, and a delete is a tombstone for 30 days.
package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/oversire/cli/wire"
	_ "modernc.org/sqlite"
)

const (
	// TombstoneTTL is the time that a delete stays in the store.
	TombstoneTTL = 30 * 24 * time.Hour
	// MaxOps is the most ops in one POST.
	MaxOps = 100
	// PageLimit is the most ops in one pull page.
	PageLimit = 500

	maxRecordBytes = 32 * 1024
)

// Synced are the collections that sync. A collection in recordShapes also has a record rule.
var Synced = []string{
	"queue-lanes", "queue-triggers", "ignored-failures", "pins", "snoozes", "notes", "agent-links", "memory", "jobs", "suggestions",
	"item-areas", "work-orders", "billing", "runners", "runner-sessions", "planner-settings", "area-catalog", "area-claims",
	"area-candidates", "changed-paths", "ai-outputs",
}

// HubOnly are the synced collections that only the cloud hub writes. A daemon refuses them.
var HubOnly = []string{"billing", "work-orders", "runners", "runner-sessions", "area-claims"}

// DecodeOps reads the body of POST /v1/store/ops. A bad op rejects the whole batch.
func DecodeOps(body []byte) ([]wire.StoreOp, error) {
	var in struct {
		Ops []json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Ops == nil || len(in.Ops) > MaxOps {
		return nil, fmt.Errorf("ops must be an array of at most %d", MaxOps)
	}
	ops := make([]wire.StoreOp, 0, len(in.Ops))
	for i, raw := range in.Ops {
		var op wire.StoreOp
		if err := wire.Decode(raw, &op); err != nil {
			return nil, fmt.Errorf("ops[%d]: %v", i, err)
		}
		if err := check(op, fmt.Sprintf("ops[%d]", i)); err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// check applies the rules of the contract and of the store to one op.
func check(op wire.StoreOp, path string) error {
	switch {
	case op.Schema < 1:
		return fail(path+".schema", "not an integer >= 1")
	case op.Collection == "":
		return fail(path+".collection", "empty")
	case op.ID == "":
		return fail(path+".id", "empty")
	case op.Op != wire.StorePut && op.Op != wire.StoreDelete:
		return fail(path+".op", "not one of put, delete")
	case !hlcPattern.MatchString(op.HLC):
		return fail(path+".hlc", "not a hybrid clock")
	case op.Device == "":
		return fail(path+".device", "empty")
	case !slices.Contains(wire.StoreSchemasAccepted, op.Schema):
		return fmt.Errorf("unknown store schema %d", op.Schema)
	case !slices.Contains(Synced, op.Collection):
		return fmt.Errorf("%s: %s does not sync", path, op.Collection)
	case slices.Contains(HubOnly, op.Collection):
		return fmt.Errorf("%s: only the hub writes %s", path, op.Collection)
	}
	if op.Record == nil {
		if op.Op == wire.StorePut {
			return fmt.Errorf("%s: a put needs a record", path)
		}
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(op.Record))
	d.UseNumber()
	var rec any
	if err := d.Decode(&rec); err != nil {
		return fail(path+".record", "not an object")
	}
	if _, ok := rec.(map[string]any); !ok {
		return fail(path+".record", "not an object")
	}
	if op.Op == wire.StoreDelete {
		return fmt.Errorf("%s: a delete has no record", path)
	}
	if utf16Len(op.Record) > maxRecordBytes {
		return fmt.Errorf("%s: record too large", path)
	}
	if s, ok := recordShapes[op.Collection]; ok {
		return s(rec, path+".record")
	}
	return nil
}

// utf16Len is the length of the compact JSON in UTF-16 units, like JSON.stringify(record).length.
func utf16Len(raw json.RawMessage) int {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return len(raw)
	}
	n := 0
	for _, r := range b.String() {
		n += utf16.RuneLen(r)
	}
	return n
}

// OpLog is the op log of the store in SQLite. It has the tables of the cloud hub: ops, records and store_meta.
type OpLog struct {
	db    *sql.DB
	clock func() time.Time
	mu    sync.Mutex // one write at a time, so that a batch is atomic and seq grows
}

// Open opens or makes the op log at path (<data>/store.db).
func Open(path string, clock func() time.Time) (*OpLog, error) {
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
		CREATE TABLE IF NOT EXISTS ops (seq INTEGER PRIMARY KEY AUTOINCREMENT, collection TEXT NOT NULL, id TEXT NOT NULL,
			hlc TEXT NOT NULL, device TEXT NOT NULL, op TEXT NOT NULL, record TEXT, at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS records (collection TEXT NOT NULL, id TEXT NOT NULL, seq INTEGER NOT NULL, hlc TEXT NOT NULL,
			device TEXT NOT NULL, op TEXT NOT NULL, record TEXT, at INTEGER NOT NULL, schema INTEGER NOT NULL,
			PRIMARY KEY (collection, id));
		CREATE INDEX IF NOT EXISTS records_seq ON records(seq);
		CREATE TABLE IF NOT EXISTS store_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &OpLog{db: db, clock: clock}, nil
}

// Close closes the database.
func (l *OpLog) Close() error { return l.db.Close() }

// Apply writes the ops that are newer than the stored record, in one transaction. The answer holds the accepted ops.
// It does not check the ops: DecodeOps does that.
func (l *OpLog) Apply(ops []wire.StoreOp) (wire.StoreApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock().UnixMilli()
	tx, err := l.db.Begin()
	if err != nil {
		return wire.StoreApplyResult{}, err
	}
	defer tx.Rollback()
	accepted := []wire.StoreOp{}
	for _, op := range ops {
		var cur string
		err := tx.QueryRow("SELECT hlc FROM records WHERE collection = ? AND id = ?", op.Collection, op.ID).Scan(&cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return wire.StoreApplyResult{}, err
		}
		if err == nil && cur >= op.HLC {
			continue
		}
		var record any // NULL for a delete
		if op.Record != nil {
			var b bytes.Buffer
			if err := json.Compact(&b, op.Record); err != nil {
				return wire.StoreApplyResult{}, fmt.Errorf("store: record of %s/%s: %w", op.Collection, op.ID, err)
			}
			record = b.String()
		}
		var seq int
		if err := tx.QueryRow("INSERT INTO ops (collection, id, hlc, device, op, record, at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING seq",
			op.Collection, op.ID, op.HLC, op.Device, string(op.Op), record, now).Scan(&seq); err != nil {
			return wire.StoreApplyResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO records (collection, id, seq, hlc, device, op, record, at, schema) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(collection, id) DO UPDATE SET seq = excluded.seq, hlc = excluded.hlc, device = excluded.device,
				op = excluded.op, record = excluded.record, at = excluded.at, schema = excluded.schema`,
			op.Collection, op.ID, seq, op.HLC, op.Device, string(op.Op), record, now, op.Schema); err != nil {
			return wire.StoreApplyResult{}, err
		}
		if err := setMeta(tx, "head", seq); err != nil {
			return wire.StoreApplyResult{}, err
		}
		accepted = append(accepted, op)
	}
	if err := tx.Commit(); err != nil {
		return wire.StoreApplyResult{}, err
	}
	return wire.StoreApplyResult{Accepted: accepted, Head: l.Head()}, nil
}

// Head is the last seq. It does not go back when the log is pruned.
func (l *OpLog) Head() int {
	n, _ := meta(l.db, "head")
	return n
}

// Pull gives the newest op of each record that changed after after. A client behind a pruned tombstone gets a reset:
// all records in one page, and a record that is not in it is gone.
func (l *OpLog) Pull(after, limit int) (wire.StorePull, error) {
	if limit <= 0 {
		limit = PageLimit
	}
	pruned, err := meta(l.db, "pruned_through")
	if err != nil {
		return wire.StorePull{}, err
	}
	reset := after > 0 && after < pruned
	var rows *sql.Rows
	const cols = "SELECT seq, collection, id, hlc, device, op, record, schema FROM records"
	if reset {
		rows, err = l.db.Query(cols + " ORDER BY seq")
	} else {
		rows, err = l.db.Query(cols+" WHERE seq > ? ORDER BY seq LIMIT ?", after, limit+1)
	}
	if err != nil {
		return wire.StorePull{}, err
	}
	defer rows.Close()
	ops, seqs := []wire.StoreOp{}, []int{}
	for rows.Next() {
		var op wire.StoreOp
		var seq int
		var record sql.NullString
		if err := rows.Scan(&seq, &op.Collection, &op.ID, &op.HLC, &op.Device, &op.Op, &record, &op.Schema); err != nil {
			return wire.StorePull{}, err
		}
		if record.Valid {
			op.Record = json.RawMessage(record.String)
		}
		ops, seqs = append(ops, op), append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return wire.StorePull{}, err
	}
	more := !reset && len(ops) > limit
	if more {
		return wire.StorePull{Ops: ops[:limit], Cursor: seqs[limit-1], More: true}, nil
	}
	return wire.StorePull{Ops: ops, Cursor: max(after, l.Head()), Reset: reset}, nil
}

// Prune drops tombstones and log rows older than TombstoneTTL.
func (l *OpLog) Prune(now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-TombstoneTTL).UnixMilli()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var top sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(seq) FROM records WHERE op = 'delete' AND at < ?", cutoff).Scan(&top); err != nil {
		return err
	}
	if top.Valid {
		if _, err := tx.Exec("DELETE FROM records WHERE op = 'delete' AND at < ?", cutoff); err != nil {
			return err
		}
		cur, err := meta(tx, "pruned_through")
		if err != nil {
			return err
		}
		if err := setMeta(tx, "pruned_through", max(int(top.Int64), cur)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM ops WHERE at < ? AND seq NOT IN (SELECT seq FROM records)", cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// querier is *sql.DB or *sql.Tx.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

func meta(q querier, key string) (int, error) {
	var v int
	err := q.QueryRow("SELECT value FROM store_meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func setMeta(q querier, key string, value int) error {
	_, err := q.Exec("INSERT INTO store_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}
