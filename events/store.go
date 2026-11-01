// Package events keeps the events table of the daemon (OVERSIRE-377): one row for each change in the work.
// The home daemon holds the full table. Each other daemon keeps a local copy and sends its rows to the home (Outbox).
package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oversire/cli/wire"
	_ "modernc.org/sqlite"
)

const (
	// DefaultLimit and MaxLimit bound one page of rows.
	DefaultLimit = 500
	MaxLimit     = 1000
	// MaxAppend is the most drafts in one POST /v1/events.
	MaxAppend = 500
)

var (
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]*:.+$`)
	kindPattern = regexp.MustCompile(`^[a-z]+(\.[a-z_]+)+$`)
	keyPattern  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]*$`)
)

// Store is the events table in SQLite (<data>/events.db).
type Store struct {
	db    *sql.DB
	clock func() time.Time
	mu    sync.Mutex // one write at a time, so that seq grows with the commit order

	queue func() bool // true: new rows also go to the outbox

	subMu   sync.Mutex
	subs    map[int]func([]wire.Event)
	nextSub int
}

// Open opens or makes the table at path.
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
		CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
			at INTEGER NOT NULL, seen_at INTEGER NOT NULL, kind TEXT NOT NULL, item TEXT, parent_item TEXT, project TEXT,
			repo TEXT, machine TEXT, provider TEXT, session TEXT, corr TEXT, actor_type TEXT NOT NULL, actor TEXT NOT NULL,
			on_behalf TEXT, via TEXT NOT NULL, summary TEXT NOT NULL, payload TEXT, private INTEGER NOT NULL DEFAULT 0);
		CREATE INDEX IF NOT EXISTS events_item ON events(item, at);
		CREATE INDEX IF NOT EXISTS events_parent ON events(parent_item, at);
		CREATE INDEX IF NOT EXISTS events_session ON events(session, at);
		CREATE INDEX IF NOT EXISTS events_kind ON events(kind, at);
		CREATE TABLE IF NOT EXISTS item_links (item TEXT PRIMARY KEY, parent TEXT, updated_at INTEGER NOT NULL);
		CREATE INDEX IF NOT EXISTS item_links_parent ON item_links(parent);
		CREATE TABLE IF NOT EXISTS outbox (seq INTEGER PRIMARY KEY);
		CREATE TABLE IF NOT EXISTS hints (seq INTEGER PRIMARY KEY, body TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("events: open %s: %w", path, err)
	}
	return &Store{db: db, clock: clock, subs: map[int]func([]wire.Event){}}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Normalize checks a draft and fits it to the limits: the summary on one line of at most 200 code points,
// and no payload over 4 KB. It does not check the kind against the catalog: the set is open.
func Normalize(d wire.EventDraft) (wire.EventDraft, error) {
	switch {
	case !idPattern.MatchString(d.ID):
		return d, fmt.Errorf("id %q is not <source>:<native id>", d.ID)
	case !kindPattern.MatchString(d.Kind):
		return d, fmt.Errorf("kind %q is not domain.verb", d.Kind)
	case d.At.IsZero():
		return d, fmt.Errorf("%s: no at", d.ID)
	case d.ActorType != wire.ActorHuman && d.ActorType != wire.ActorAgent && d.ActorType != wire.ActorSystem && d.ActorType != wire.ActorProvider:
		return d, fmt.Errorf("%s: actor type %q", d.ID, d.ActorType)
	case d.Actor == "" || d.Via == "":
		return d, fmt.Errorf("%s: no actor or via", d.ID)
	}
	d.Item = upperKey(d.Item)
	d.ParentItem = upperKey(d.ParentItem)
	if d.Item == d.ParentItem {
		d.ParentItem = ""
	}
	d.Summary = Clip(d.Summary)
	if len(d.Payload) > 0 {
		var b bytes.Buffer
		if json.Compact(&b, d.Payload) != nil || b.Len() == 0 || b.Bytes()[0] != '{' {
			return d, fmt.Errorf("%s: payload is not an object", d.ID)
		}
		d.Payload = b.Bytes()
		if len(d.Payload) > wire.MaxEventPayload {
			d.Payload = json.RawMessage(`{}`)
		}
	}
	d.At = d.At.UTC().Truncate(time.Millisecond)
	return d, nil
}

func upperKey(k string) string {
	k = strings.ToUpper(strings.TrimSpace(k))
	if !keyPattern.MatchString(k) {
		return ""
	}
	return k
}

// Clip gives s on one line, with at most MaxEventSummary code points.
func Clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= wire.MaxEventSummary {
		return s
	}
	r := []rune(s)
	return string(r[:wire.MaxEventSummary-1]) + "…"
}

// QueueWhen sets the rule that sends new rows to the outbox (Outbox.Member).
func (s *Store) QueueWhen(fn func() bool) {
	s.mu.Lock()
	s.queue = fn
	s.mu.Unlock()
}

// Append writes the new drafts in one transaction and gives the rows that were new. A repeat id is a no-op.
// A draft with no parent gets it from item_links. On a member that is not home, the new rows also go to the outbox.
func (s *Store) Append(ctx context.Context, drafts []wire.EventDraft) ([]wire.Event, error) {
	s.mu.Lock()
	queue := s.queue != nil && s.queue()
	s.mu.Unlock()
	return s.add(ctx, drafts, queue)
}

func (s *Store) add(ctx context.Context, drafts []wire.EventDraft, queue bool) ([]wire.Event, error) {
	s.mu.Lock()
	now := s.clock().UTC().Truncate(time.Millisecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	defer tx.Rollback()
	out := []wire.Event{}
	for _, raw := range drafts {
		d, err := Normalize(raw)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if d.Item != "" && d.ParentItem == "" {
			var p sql.NullString
			if err := tx.QueryRowContext(ctx, "SELECT parent FROM item_links WHERE item = ?", d.Item).Scan(&p); err == nil {
				d.ParentItem = p.String
			}
		}
		var seq int64
		err = tx.QueryRowContext(ctx, `INSERT INTO events (id, at, seen_at, kind, item, parent_item, project, repo, machine, provider,
			session, corr, actor_type, actor, on_behalf, via, summary, payload, private) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING RETURNING seq`,
			d.ID, d.At.UnixMilli(), now.UnixMilli(), d.Kind, null(d.Item), null(d.ParentItem), null(d.Project), null(d.Repo),
			null(d.Machine), null(d.Provider), null(d.Session), null(d.Corr), string(d.ActorType), d.Actor, null(d.OnBehalf), d.Via,
			d.Summary, nullBytes(d.Payload), d.Private).Scan(&seq)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if queue {
			if _, err := tx.ExecContext(ctx, "INSERT INTO outbox (seq) VALUES (?)", seq); err != nil {
				s.mu.Unlock()
				return nil, err
			}
		}
		out = append(out, wire.Event{Seq: seq, SeenAt: now, EventDraft: d})
	}
	if err := tx.Commit(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	if len(out) > 0 {
		s.publish(out)
	}
	return out, nil
}

// Head is the last seq.
func (s *Store) Head(ctx context.Context) (int64, error) {
	var h sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(seq) FROM events").Scan(&h)
	return h.Int64, err
}

const columns = `seq, id, at, seen_at, kind, item, parent_item, project, repo, machine, provider, session, corr,
	actor_type, actor, on_behalf, via, summary, payload, private`

// Query gives the rows of an item after q.After, in seq order. Tree adds the rows of its sub-items:
// parent_item is the item, or item_links names the item as the parent.
func (s *Store) Query(ctx context.Context, q wire.EventQuery) (wire.EventPage, error) {
	key := upperKey(q.Item)
	if key == "" {
		return wire.EventPage{}, fmt.Errorf("item %q is not a key", q.Item)
	}
	where := "item = ?"
	args := []any{key}
	if q.Tree {
		where = "(item = ? OR parent_item = ? OR item IN (SELECT item FROM item_links WHERE parent = ?))"
		args = []any{key, key, key}
	}
	return s.page(ctx, where, args, q.Kinds, q.After, q.Limit)
}

// Tail gives all rows after the seq after, for a home move and the CLI.
func (s *Store) Tail(ctx context.Context, after int64, limit int) (wire.EventPage, error) {
	return s.page(ctx, "1 = 1", nil, nil, after, limit)
}

func (s *Store) page(ctx context.Context, where string, args []any, kinds []string, after int64, limit int) (wire.EventPage, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	if len(kinds) > 0 {
		var or []string
		for _, k := range kinds {
			if d, ok := strings.CutSuffix(k, ".*"); ok {
				or = append(or, "kind LIKE ?")
				args = append(args, d+".%")
			} else {
				or = append(or, "kind = ?")
				args = append(args, k)
			}
		}
		where += " AND (" + strings.Join(or, " OR ") + ")"
	}
	args = append(args, after, limit+1)
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM events WHERE "+where+" AND seq > ? ORDER BY seq LIMIT ?", args...)
	if err != nil {
		return wire.EventPage{}, err
	}
	defer rows.Close()
	p := wire.EventPage{Events: []wire.Event{}, Next: after}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return p, err
		}
		if len(p.Events) == limit {
			p.More = true
			break
		}
		p.Events = append(p.Events, e)
		p.Next = e.Seq
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	rows.Close() // the store has one connection
	p.Head, err = s.Head(ctx)
	return p, err
}

type scanner interface{ Scan(...any) error }

func scan(r scanner) (wire.Event, error) {
	var e wire.Event
	var at, seen int64
	var item, parent, project, repo, machine, provider, session, corr, onBehalf, payload sql.NullString
	var actorType string
	err := r.Scan(&e.Seq, &e.ID, &at, &seen, &e.Kind, &item, &parent, &project, &repo, &machine, &provider, &session, &corr,
		&actorType, &e.Actor, &onBehalf, &e.Via, &e.Summary, &payload, &e.Private)
	e.At, e.SeenAt = time.UnixMilli(at).UTC(), time.UnixMilli(seen).UTC()
	e.Item, e.ParentItem, e.Project, e.Repo = item.String, parent.String, project.String, repo.String
	e.Machine, e.Provider, e.Session, e.Corr, e.OnBehalf = machine.String, provider.String, session.String, corr.String, onBehalf.String
	e.ActorType = wire.ActorType(actorType)
	if payload.Valid {
		e.Payload = json.RawMessage(payload.String)
	}
	return e, err
}

// Subscribe calls fn with the new rows after each append. fn must not block.
func (s *Store) Subscribe(fn func([]wire.Event)) (cancel func()) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *Store) publish(rows []wire.Event) {
	s.subMu.Lock()
	fns := make([]func([]wire.Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.subMu.Unlock()
	for _, fn := range fns {
		fn(rows)
	}
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}
