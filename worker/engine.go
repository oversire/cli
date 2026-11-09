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

// The limits of Wait and Brief.
const (
	MaxWait          = 25 * time.Second // one wait call stops before the 30 s client timeout
	DefaultPoll      = 2 * time.Second
	DefaultStall     = 10 * time.Minute
	DefaultStallTool = 30 * time.Minute
	maxNote          = 500 // characters of one note in a brief
	maxFailed        = 5   // failed tools in a brief
)

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
	Log       *slog.Logger
	Poll      time.Duration // the time between two checks in Wait
	Stall     time.Duration // a working session with no turn for this time is stalled
	StallTool time.Duration // a running tool keeps off a stall for this time

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
	return &Engine{Log: slog.Default(), Poll: DefaultPoll, Stall: DefaultStall, StallTool: DefaultStallTool, store: store, sessions: sessions, chats: chats, diff: diff, now: now}
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

// WakeReason tells why Wait returned.
type WakeReason string

const (
	WakeInput      WakeReason = "input"      // the turn ended; the worker waits for input
	WakePermission WakeReason = "permission" // the worker asks for a permission
	WakeQuestion   WakeReason = "question"   // the worker asks a question
	WakeDone       WakeReason = "done"
	WakeFailed     WakeReason = "failed"
	WakeStall      WakeReason = "stall"
	WakeNone       WakeReason = "none" // the wait ended at max with no change
)

// Wake is the answer of GET /v1/workers/{id}/wait.
type Wake struct {
	Reason WakeReason `json:"reason"`
	Header Header     `json:"header"`
}

// wakeFor maps the state of a session to a wake reason. WakeNone means the worker needs no action.
func wakeFor(s wire.DaemonSession) WakeReason {
	switch s.State {
	case wire.PhaseDone:
		return WakeDone
	case wire.PhaseFailed:
		return WakeFailed
	case wire.PhaseWaiting:
		switch s.Reason {
		case wire.ReasonTurnEnd, wire.ReasonInterrupted:
			return WakeInput
		case wire.ReasonPermission:
			return WakePermission
		case wire.ReasonQuestion:
			return WakeQuestion
		}
	}
	return WakeNone
}

// Wait blocks until the worker needs an action, or until max. It checks every Poll.
func (e *Engine) Wait(ctx context.Context, ref string, max time.Duration) (Wake, error) {
	if max <= 0 || max > MaxWait {
		return Wake{}, refuse(Invalid, "The max is above 0 and at most %s.", MaxWait)
	}
	ses, err := e.session(ref)
	if err != nil {
		return Wake{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.NewTimer(max)
	defer timer.Stop()
	tick := time.NewTicker(e.Poll)
	defer tick.Stop()
	var tail lastTurn
	for {
		reason := wakeFor(ses)
		if reason == WakeNone && ses.State == wire.PhaseWorking {
			if err := tail.update(ctx, e.chats, ses); err != nil {
				return Wake{}, err
			}
			if e.stalled(ses, tail) {
				reason = WakeStall
			}
		}
		if reason != WakeNone {
			return e.wake(ctx, ses, reason)
		}
		select {
		case <-ctx.Done():
			return Wake{}, ctx.Err()
		case <-timer.C:
			return e.wake(ctx, ses, WakeNone)
		case <-tick.C:
		}
		if ses, err = e.session(ref); err != nil {
			return Wake{}, err
		}
	}
}

func (e *Engine) wake(ctx context.Context, ses wire.DaemonSession, r WakeReason) (Wake, error) {
	rec, err := e.remember(ses)
	if err != nil {
		return Wake{}, err
	}
	return Wake{Reason: r, Header: e.header(ctx, ses, rec, nil)}, nil
}

// stalled tells if a working session has had no turn for Stall. A running tool counts as
// activity for StallTool. A session with no turn counts from the start of its state.
func (e *Engine) stalled(ses wire.DaemonSession, t lastTurn) bool {
	at, limit := ses.StateSince, e.Stall
	if t.ok {
		at = t.turn.At
		if t.turn.Tool != nil && t.turn.Tool.Status == wire.ToolRunning {
			limit = e.StallTool
		}
	}
	return !at.IsZero() && e.now().Sub(at) > limit
}

// lastTurn follows the last turn of a session. A tool turn changes in place, so each update
// reads again from the turn before the last.
type lastTurn struct {
	src   term.TurnSource
	after int
	turn  wire.SessionTurn
	ok    bool
}

func (l *lastTurn) update(ctx context.Context, chats Chats, ses wire.DaemonSession) error {
	if l.src == nil {
		src, err := chats.Chat(ctx, ses)
		if errors.Is(err, term.ErrNoView) {
			return refuse(Conflict, "The session %s has no turns.", ses.ID)
		}
		if err != nil {
			return err
		}
		l.src = src
	}
	ctx, cancel := context.WithCancel(ctx) // ends the live feed of this call
	defer cancel()
	turns, _, err := l.src.Turns(ctx, l.after)
	if err != nil {
		return err
	}
	if len(turns) > 0 {
		l.turn, l.ok = turns[len(turns)-1], true
		l.after = max(l.turn.Seq-1, 0)
	}
	return nil
}

// Brief is a short summary of a run for a new overseer.
type Brief struct {
	Header Header             `json:"header"`
	Plan   string             `json:"plan"`   // the first user turn
	Notes  []wire.SessionTurn `json:"notes"`  // the later user turns of the overseer and the user, clipped
	Last   string             `json:"last"`   // the last agent text
	Failed []wire.SessionTurn `json:"failed"` // the last failed tools
}

// Brief builds the brief of a worker from all its turns.
func (e *Engine) Brief(ctx context.Context, ref string) (Brief, error) {
	ses, err := e.session(ref)
	if err != nil {
		return Brief{}, err
	}
	rec, err := e.remember(ses)
	if err != nil {
		return Brief{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src, err := e.chats.Chat(ctx, ses)
	if errors.Is(err, term.ErrNoView) {
		return Brief{}, refuse(Conflict, "The session %s has no turns.", ses.ID)
	}
	if err != nil {
		return Brief{}, err
	}
	turns, _, err := src.Turns(ctx, 0)
	if err != nil {
		return Brief{}, err
	}
	b := Brief{Header: e.header(ctx, ses, rec, turns), Notes: []wire.SessionTurn{}, Failed: []wire.SessionTurn{}}
	planned := false
	for _, t := range turns {
		switch {
		case t.Role == wire.RoleUser && !planned:
			b.Plan, planned = t.Text, true
		case t.Role == wire.RoleUser && t.Origin != wire.OriginSystem:
			t.Text = clip(t.Text, maxNote)
			b.Notes = append(b.Notes, t)
		case t.Role == wire.RoleAssistant && t.Tool == nil && strings.TrimSpace(t.Text) != "":
			b.Last = t.Text
		case t.Tool != nil && t.Tool.Status == wire.ToolFailed:
			b.Failed = append(b.Failed, t)
		}
	}
	b.Failed = b.Failed[max(0, len(b.Failed)-maxFailed):]
	return b, nil
}
