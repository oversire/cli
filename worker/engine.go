package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

// The page size of a read.
const (
	DefaultLimit = 200
	MaxLimit     = 1000
)

// maxRequest is the length of the open request in the header, in characters.
const maxRequest = 200

// Sessions is the session book (sessions.Book).
type Sessions interface {
	Get(id string) (wire.DaemonSession, bool)
	ByKey(runtime, key string) (wire.DaemonSession, bool)
	Tool(id string) (tool, input string)
}

// Chats opens the turns of a session (runtime.Registry).
type Chats interface {
	Chat(ctx context.Context, s wire.DaemonSession) (term.TurnSource, error)
}

// Differ reads the repos of a worktree group (review.Git).
type Differ interface {
	GroupRepos(ctx context.Context, root, key string) ([]wire.ReviewRepo, bool, error)
}

// Kind is the kind of an Error. The routes give it as the HTTP status.
type Kind string

const (
	NotFound Kind = "notFound"
	Invalid  Kind = "invalid"
	Conflict Kind = "conflict"
)

// Error is a refused call.
type Error struct {
	Kind Kind
	Text string
}

func (e *Error) Error() string { return e.Text }

func refuse(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Text: fmt.Sprintf(format, a...)}
}

// Header is the state of a worker at one read.
type Header struct {
	Session string             `json:"session"`
	Key     string             `json:"key,omitempty"`
	Harness string             `json:"harness,omitempty"`
	Model   string             `json:"model,omitempty"`
	State   wire.SessionPhase  `json:"state"`
	Reason  wire.SessionReason `json:"reason,omitempty"`
	Since   time.Time          `json:"since"`
	First   int                `json:"first"` // the first and the last seq of the turns of the read; 0 with no turns
	Last    int                `json:"last"`
	Tokens  int64              `json:"tokens,omitempty"`
	Request string             `json:"request,omitempty"` // the open permission or question
	Repos   []wire.ReviewRepo  `json:"repos,omitempty"`
}

// Read is the answer of GET /v1/workers/{id}/read. Next is the cursor to commit after the reader shows the turns.
type Read struct {
	Header Header             `json:"header"`
	Turns  []wire.SessionTurn `json:"turns"`
	Next   int                `json:"next"`
}

// Cursor is the body of PUT /v1/workers/{id}/cursors/{reader}.
type Cursor struct {
	Seq int `json:"seq"`
}

// Engine reads the turns of worker sessions for each reader and keeps the cursor of each reader.
type Engine struct {
	Log *slog.Logger

	store    *Store
	sessions Sessions
	chats    Chats
	diff     Differ
	now      func() time.Time

	mu sync.Mutex // the records
}

func NewEngine(store *Store, sessions Sessions, chats Chats, diff Differ, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{Log: slog.Default(), store: store, sessions: sessions, chats: chats, diff: diff, now: now}
}

// Resolve finds the session of a ref: a session id, or an item key. A stopped session is found
// through the store, because a stop clears its key in the book.
func (e *Engine) Resolve(ref string) (wire.DaemonSession, error) {
	if s, ok := e.sessions.Get(ref); ok {
		return s, nil
	}
	key := "work:" + strings.ToUpper(ref)
	if s, ok := e.sessions.ByKey(wire.RuntimeOversire, key); ok {
		return s, nil
	}
	e.mu.Lock()
	rec, ok, err := e.store.ByKey(key)
	e.mu.Unlock()
	if err != nil {
		return wire.DaemonSession{}, err
	}
	if ok {
		if s, ok := e.sessions.Get(rec.Session); ok {
			return s, nil
		}
	}
	return wire.DaemonSession{}, refuse(NotFound, "There is no worker session %s.", ref)
}

// session resolves ref and checks that the session is on the oversire runtime.
func (e *Engine) session(ref string) (wire.DaemonSession, error) {
	s, err := e.Resolve(ref)
	if err != nil {
		return s, err
	}
	if s.Runtime != wire.RuntimeOversire {
		return s, refuse(Conflict, "The session %s runs on the %s runtime. A worker runs on the oversire runtime.", s.ID, s.Runtime)
	}
	return s, nil
}

// Read gives the turns of the session after the cursor of the reader, at most limit. 0 is the default limit.
func (e *Engine) Read(ctx context.Context, ref, reader string, limit int) (Read, error) {
	if reader == "" {
		return Read{}, refuse(Invalid, "Give the reader.")
	}
	if limit < 0 || limit > MaxLimit {
		return Read{}, refuse(Invalid, "The limit is 1 to %d.", MaxLimit)
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	ses, err := e.session(ref)
	if err != nil {
		return Read{}, err
	}
	rec, err := e.remember(ses)
	if err != nil {
		return Read{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := e.chats.Chat(ctx, ses)
	if errors.Is(err, term.ErrNoView) {
		return Read{}, refuse(Conflict, "The session %s has no turns.", ses.ID)
	}
	if err != nil {
		return Read{}, err
	}
	cursor := rec.Cursors[reader]
	history, _, err := src.Turns(ctx, cursor)
	if err != nil {
		return Read{}, err
	}
	turns := history[:min(limit, len(history))]
	out := Read{Header: e.header(ctx, ses, rec, turns), Turns: turns, Next: next(turns, cursor)}
	if out.Turns == nil {
		out.Turns = []wire.SessionTurn{}
	}
	return out, nil
}

// next is the cursor after the turns. It stops before the first running tool, because a tool turn
// changes in place with the same seq.
func next(turns []wire.SessionTurn, cursor int) int {
	n := cursor
	for _, t := range turns {
		if t.Tool != nil && t.Tool.Status == wire.ToolRunning {
			return max(n, t.Seq-1)
		}
		n = max(n, t.Seq)
	}
	return n
}

// remember loads the record of the session and keeps its start key.
func (e *Engine) remember(ses wire.DaemonSession) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, err := e.store.Load(ses.ID)
	if err != nil {
		return Record{}, err
	}
	if ses.Key != "" && rec.Key != ses.Key {
		rec.Session, rec.Key, rec.UpdatedAt = ses.ID, ses.Key, e.now().UTC()
		if err := e.store.Save(rec); err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

// Commit sets the cursor of the reader. The reader runs it after it shows the turns.
func (e *Engine) Commit(ref, reader string, seq int) error {
	if reader == "" {
		return refuse(Invalid, "Give the reader.")
	}
	if seq < 0 {
		return refuse(Invalid, "The seq is 0 or more.")
	}
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, err := e.store.Load(ses.ID)
	if err != nil {
		return err
	}
	rec.Session = ses.ID
	if ses.Key != "" {
		rec.Key = ses.Key
	}
	if rec.Cursors == nil {
		rec.Cursors = map[string]int{}
	}
	rec.Cursors[reader] = seq
	rec.UpdatedAt = e.now().UTC()
	return e.store.Save(rec)
}

func (e *Engine) header(ctx context.Context, ses wire.DaemonSession, rec Record, turns []wire.SessionTurn) Header {
	h := Header{Session: ses.ID, Key: ses.Key, Harness: ses.Harness, Model: ses.Model, State: ses.State, Reason: ses.Reason, Since: ses.StateSince}
	if h.Key == "" {
		h.Key = rec.Key
	}
	if len(turns) > 0 {
		h.First, h.Last = turns[0].Seq, turns[len(turns)-1].Seq
	}
	switch ses.Reason {
	case wire.ReasonPermission:
		tool, input := e.sessions.Tool(ses.ID)
		h.Request = clip(strings.TrimSpace("permission "+tool+" "+input), maxRequest)
	case wire.ReasonQuestion:
		h.Request = strings.TrimSpace("question " + ses.Question)
	}
	if root, name, ok := ticket.GroupOf(ses.Repo); ok {
		repos, found, err := e.diff.GroupRepos(ctx, root, name)
		if err != nil {
			e.Log.Warn("worker group repos", "session", ses.ID, "err", err)
		}
		if found {
			h.Repos = repos
		}
	}
	return h
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
