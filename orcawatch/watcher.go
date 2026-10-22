// Package orcawatch keeps the session book in line with the local Orca. It polls `orca worktree ps` and
// `orca terminal list`, on a hint from a hook, on a baseline interval and after the Mac wakes.
// It runs no terminal read and no terminal wait: each wait holds one of the long-poll slots of Orca.
package orcawatch

import (
	"context"
	"sync"
	"time"

	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

const (
	// MinGap is the shortest time between two polls.
	MinGap = time.Second
	// Busy is the baseline interval while an Orca session works or waits. Idle is the interval else.
	Busy = 20 * time.Second
	Idle = 120 * time.Second
	// Retry is the wait when Orca is not reachable.
	Retry = 60 * time.Second
	// Beat is the interval of the wake check. A wall clock jump of more than WakeJump over Beat is a wake.
	Beat     = 5 * time.Second
	WakeJump = 30 * time.Second
	// PollTimeout limits each poll.
	PollTimeout = 15 * time.Second
)

// Health values.
const (
	HealthOK          = "ok"
	HealthUnavailable = "unavailable"
)

// Lister reads the local Orca (orca.Orca).
type Lister interface {
	Worktrees(ctx context.Context) ([]orca.WorktreeCard, error) // worktree ps
	Terminals(ctx context.Context) ([]orca.Terminal, error)     // terminal list
	Reachable(ctx context.Context) bool                         // status
}

// Clock gives the time and the timers. Now must give the wall clock, so that a sleep of the Mac shows as a jump.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now().Round(0) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type Watcher struct {
	list  Lister
	book  *sessions.Book
	clock Clock
	hint  chan struct{}
	known map[string]bool // panes that Orca gave, to end them when they go

	mu     sync.Mutex
	health string
	busy   bool
}

// NewWatcher uses the real clock when clock is nil.
func NewWatcher(list Lister, book *sessions.Book, clock Clock) *Watcher {
	if clock == nil {
		clock = realClock{}
	}
	return &Watcher{list: list, book: book, clock: clock, hint: make(chan struct{}, 1), known: map[string]bool{}, health: HealthUnavailable}
}

// Hint asks for a poll. It does not block. Hints in a row give one queued poll.
func (w *Watcher) Hint() {
	select {
	case w.hint <- struct{}{}:
	default:
	}
}

// Health gives "ok", "unavailable" or "error: <reason>".
func (w *Watcher) Health() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.health
}

// Run polls at the start, then on hints, on the baseline interval and after a wake, until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	for _, s := range w.book.All() {
		if s.Runtime == wire.RuntimeOrca {
			w.known[s.ID] = true
		}
	}
	var last time.Time
	beat := w.clock.Now()
	due := true
	for {
		if due {
			if !last.IsZero() {
				if gap := MinGap - w.clock.Now().Sub(last); gap > 0 {
					select {
					case <-ctx.Done():
						return nil
					case <-w.clock.After(gap):
					}
				}
			}
			// This poll also serves the hints that came before it.
			select {
			case <-w.hint:
			default:
			}
			w.poll(ctx)
			last, due = w.clock.Now(), false
		}
		wait := w.interval() - w.clock.Now().Sub(last)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-w.hint:
			due = true
		case <-w.clock.After(wait):
			due = true
		case <-w.clock.After(Beat):
			now := w.clock.Now()
			if now.Sub(beat) > Beat+WakeJump {
				due = true
			}
			beat = now
		}
	}
}

func (w *Watcher) interval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.health == HealthUnavailable:
		return Retry
	case w.busy:
		return Busy
	}
	return Idle
}

func (w *Watcher) setHealth(h string, busy bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.health, w.busy = h, busy
}

// poll reads Orca once and applies a patch for each agent. After a failure it checks whether Orca runs at all.
func (w *Watcher) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, PollTimeout)
	defer cancel()
	if w.Health() == HealthUnavailable && !w.list.Reachable(ctx) {
		return
	}
	cards, err := w.list.Worktrees(ctx)
	var terms []orca.Terminal
	if err == nil {
		terms, err = w.list.Terminals(ctx)
	}
	if err != nil {
		if ctx.Err() == nil && !w.list.Reachable(ctx) {
			w.setHealth(HealthUnavailable, false)
		} else {
			w.setHealth("error: "+err.Error(), false)
		}
		return
	}
	w.setHealth(HealthOK, w.apply(cards, terms))
}

// apply gives one patch for each agent, and End for each pane that is gone. It tells whether an agent works or waits.
func (w *Watcher) apply(cards []orca.WorktreeCard, terms []orca.Terminal) (busy bool) {
	at := w.clock.Now()
	handles := map[string]orca.Terminal{}
	for _, t := range terms {
		if old, ok := handles[t.PaneKey()]; !ok || old.Orphaned && !t.Orphaned {
			handles[t.PaneKey()] = t
		}
	}
	seen := map[string]bool{}
	for _, c := range cards {
		if c.IsArchived {
			continue
		}
		for _, a := range c.Agents {
			p, ok := w.patch(c, a, handles[a.PaneKey].Handle, at)
			if !ok {
				continue
			}
			seen[a.PaneKey] = true
			w.known[a.PaneKey] = true
			busy = busy || a.State == "working" || a.State == "waiting" || a.State == "blocked"
			w.book.Apply(p)
		}
	}
	for id := range w.known {
		if !seen[id] {
			w.book.Apply(sessions.Patch{ID: id, Source: sessions.SourceOrca, At: at, End: true})
			delete(w.known, id)
		}
	}
	return busy
}

func (w *Watcher) patch(c orca.WorktreeCard, a orca.AgentCard, terminal string, at time.Time) (sessions.Patch, bool) {
	if a.PaneKey == "" {
		return sessions.Patch{}, false
	}
	var state wire.SessionPhase
	var reason *wire.SessionReason
	switch a.State {
	case "working":
		state = wire.PhaseWorking
		r := wire.SessionReason("")
		if a.WorkingMode == "monitoring" {
			r = wire.ReasonMonitoring
		}
		reason = &r
	case "waiting":
		state = wire.PhaseWaiting
	case "blocked":
		state = wire.PhaseWaiting
		r := wire.ReasonPermission
		reason = &r
	case "done":
		state = wire.PhaseDone
		if a.Interrupted {
			r := wire.ReasonInterrupted
			reason = &r
		}
	default:
		return sessions.Patch{}, false
	}
	str := func(s string) *string { return &s }
	p := sessions.Patch{
		ID: a.PaneKey, Source: sessions.SourceOrca, At: at,
		Runtime: str(wire.RuntimeOrca), Workspace: str(c.ID), Cwd: str(c.Path), Repo: str(c.Repo), Branch: str(c.BranchName()),
		Tool: str(a.ToolName), ToolInput: str(a.ToolInput),
	}
	if a.StateStartedAt > 0 {
		p.Since = time.UnixMilli(a.StateStartedAt)
	}
	if a.AgentType != "" {
		p.Harness = str(a.AgentType)
	}
	if terminal != "" {
		p.Terminal = &terminal
	}
	if a.Prompt != "" {
		p.Prompt = str(a.Prompt)
	}
	if a.LastAssistantMessage != "" {
		p.LastMessage = str(a.LastAssistantMessage)
	}
	// Orca gives no reason for waiting and done. The same state keeps the reason that a hook gave.
	if cur, ok := w.book.Get(a.PaneKey); ok && cur.State == state && reason == nil {
		return p, true
	}
	p.State, p.Reason = &state, reason
	return p, true
}
