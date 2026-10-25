package orcawatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
	calls  int
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, fakeTimer{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			kept = append(kept, t)
		}
	}
	c.timers = kept
	c.mu.Unlock()
}

// settle waits until the watcher makes no more timers, which means it waits in its select.
func (c *fakeClock) settle(t *testing.T) {
	t.Helper()
	last := -1
	for range 200 {
		c.mu.Lock()
		n := c.calls
		c.mu.Unlock()
		if n == last {
			return
		}
		last = n
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("the watcher does not settle")
}

// step moves the clock in steps of one Beat, so that no step looks like a wake.
func (c *fakeClock) step(t *testing.T, d time.Duration) {
	for d > 0 {
		s := min(d, Beat)
		c.advance(s)
		c.settle(t)
		d -= s
	}
}

type fakeLister struct {
	mu        sync.Mutex
	cards     []orca.WorktreeCard
	terms     []orca.Terminal
	reachable bool
	err       error
	polls     int
	checks    int
}

func (f *fakeLister) Worktrees(context.Context) ([]orca.WorktreeCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.cards, f.err
}

func (f *fakeLister) Terminals(context.Context) ([]orca.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terms, nil
}

func (f *fakeLister) Reachable(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.reachable
}

func (f *fakeLister) count() (polls, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls, f.checks
}

func (f *fakeLister) set(cards []orca.WorktreeCard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = cards
}

func recorded(t *testing.T) ([]orca.WorktreeCard, []orca.Terminal) {
	t.Helper()
	var ps struct {
		Result struct {
			Worktrees []orca.WorktreeCard `json:"worktrees"`
		} `json:"result"`
	}
	var tl struct {
		Result struct {
			Terminals []orca.Terminal `json:"terminals"`
		} `json:"result"`
	}
	for name, v := range map[string]any{"testdata/worktree-ps.json": &ps, "testdata/terminal-list.json": &tl} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	return ps.Result.Worktrees, tl.Result.Terminals
}

func idle(state string) []orca.WorktreeCard {
	return []orca.WorktreeCard{{ID: "w1", Repo: "app", Path: "/p", Agents: []orca.AgentCard{{PaneKey: "t:l", State: state, AgentType: "claude"}}}}
}

func start(t *testing.T, l *fakeLister) (*Watcher, *fakeClock, *sessions.Book, func()) {
	t.Helper()
	c := newClock()
	book := sessions.NewBook("mbp", "", nil, c.Now)
	w := NewWatcher(l, book, c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	c.settle(t)
	return w, c, book, func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestStartPollAndHint(t *testing.T) {
	l := &fakeLister{reachable: true, cards: idle("done")}
	w, c, _, stop := start(t, l)
	defer stop()
	if p, _ := l.count(); p != 1 || w.Health() != HealthOK {
		t.Fatalf("start: %d polls, %s", p, w.Health())
	}
	w.Hint()
	c.settle(t)
	c.step(t, MinGap)
	if p, _ := l.count(); p != 2 {
		t.Fatalf("hint: %d polls", p)
	}
}

func TestHintsCoalesce(t *testing.T) {
	l := &fakeLister{reachable: true, cards: idle("done")}
	w, c, _, stop := start(t, l)
	defer stop()
	for range 10 {
		w.Hint()
		c.advance(10 * time.Millisecond)
	}
	c.settle(t)
	c.step(t, 3*time.Second)
	if p, _ := l.count(); p-1 > 2 {
		t.Fatalf("10 hints gave %d polls", p-1)
	}
}

func TestBaselineFollowsStates(t *testing.T) {
	l := &fakeLister{reachable: true, cards: idle("working")}
	_, c, _, stop := start(t, l)
	defer stop()
	c.step(t, Busy-Beat)
	if p, _ := l.count(); p != 1 {
		t.Fatalf("busy, early: %d", p)
	}
	l.set(idle("done"))
	c.step(t, Beat)
	if p, _ := l.count(); p != 2 {
		t.Fatalf("busy interval: %d", p)
	}
	c.step(t, Busy+Beat)
	if p, _ := l.count(); p != 2 {
		t.Fatalf("idle must wait longer: %d", p)
	}
	c.step(t, Idle-Busy-Beat)
	if p, _ := l.count(); p != 3 {
		t.Fatalf("idle interval: %d", p)
	}
}

func TestWakePolls(t *testing.T) {
	l := &fakeLister{reachable: true, cards: idle("done")}
	_, c, _, stop := start(t, l)
	defer stop()
	c.advance(time.Minute)
	c.settle(t)
	if p, _ := l.count(); p != 2 {
		t.Fatalf("wake: %d polls", p)
	}
}

func TestUnreachableBacksOff(t *testing.T) {
	l := &fakeLister{reachable: false}
	w, c, _, stop := start(t, l)
	defer stop()
	if p, ch := l.count(); p != 0 || ch != 1 || w.Health() != HealthUnavailable {
		t.Fatalf("polls %d checks %d %s", p, ch, w.Health())
	}
	c.step(t, Retry-Beat)
	if _, ch := l.count(); ch != 1 {
		t.Fatalf("early retry: %d", ch)
	}
	c.step(t, Beat)
	if _, ch := l.count(); ch != 2 {
		t.Fatalf("retry: %d", ch)
	}
}

func TestErrorHealth(t *testing.T) {
	l := &fakeLister{reachable: true, err: errors.New("orca worktree ps: bad")}
	w, _, _, stop := start(t, l)
	defer stop()
	if w.Health() != "error: orca worktree ps: bad" {
		t.Fatalf("%s", w.Health())
	}
}

func TestApplyRecorded(t *testing.T) {
	cards, terms := recorded(t)
	c := newClock()
	book := sessions.NewBook("mbp", "", nil, c.Now)
	w := NewWatcher(&fakeLister{}, book, c)
	if !w.apply(cards, terms) {
		t.Fatal("the data has working agents")
	}
	all := book.All()
	agents, archived := 0, ""
	for _, card := range cards {
		if card.IsArchived {
			archived = card.Agents[0].PaneKey
			continue
		}
		agents += len(card.Agents)
	}
	if len(all) != agents || archived == "" {
		t.Fatalf("%d sessions, %d agents", len(all), agents)
	}
	if _, ok := book.Get(archived); ok {
		t.Fatal("an archived worktree gives a session")
	}
	seen := map[string]bool{}
	for _, s := range all {
		if s.Runtime != wire.RuntimeOrca || s.Workspace == "" || s.Cwd == "" || s.Harness != "claude" || s.Sources[0] != "orca" {
			t.Fatalf("%+v", s)
		}
		switch {
		case s.State == wire.PhaseWorking && s.Reason == wire.ReasonMonitoring:
			seen["monitoring"] = true
		case s.State == wire.PhaseWorking:
			seen["working"] = true
		case s.State == wire.PhaseWaiting:
			seen["waiting"] = true
			if tool, _ := book.Tool(s.ID); tool != "AskUserQuestion" {
				t.Fatalf("tool %q", tool)
			}
		case s.State == wire.PhaseDone:
			seen["done"] = true
		}
		if s.Terminal == "" {
			seen["no terminal"] = true
		}
	}
	for _, k := range []string{"monitoring", "working", "waiting", "done", "no terminal"} {
		if !seen[k] {
			t.Errorf("no session with %s", k)
		}
	}
	got, _ := book.Get("d9534963-3f90-46f5-83c0-eea61ed16847:c812e779-8d8e-4f95-905f-40e8c893d014")
	if got.Terminal == "" || got.Repo != "oversire-workspace" || got.LastMessage == "" || got.Prompt == "" {
		t.Fatalf("%+v", got)
	}

	// A pane that is gone ends its session.
	c.advance(time.Second)
	w.apply(cards[:1], terms)
	if n := len(book.All()); n != len(cards[0].Agents) {
		t.Fatalf("%d sessions after the panes went", n)
	}
}

func TestBlocked(t *testing.T) {
	c := newClock()
	book := sessions.NewBook("mbp", "", nil, c.Now)
	w := NewWatcher(&fakeLister{}, book, c)
	w.apply(idle("blocked"), nil)
	if s, _ := book.Get("t:l"); s.State != wire.PhaseWaiting || s.Reason != wire.ReasonPermission {
		t.Fatalf("%+v", s)
	}
}

// The book decides between hook and Orca patches: a newer hook wins, and Orca needs OrcaLag over the last hook.
func TestBookPrecedence(t *testing.T) {
	c := newClock()
	book := sessions.NewBook("mbp", "", nil, c.Now)
	w := NewWatcher(&fakeLister{}, book, c)
	w.apply(idle("working"), nil)
	c.advance(time.Second)
	waiting, question := wire.PhaseWaiting, wire.ReasonQuestion
	book.Apply(sessions.Patch{ID: "t:l", Source: sessions.SourceHook, At: c.Now(), State: &waiting, Reason: &question})

	c.advance(time.Second)
	w.apply(idle("working"), nil)
	if s, _ := book.Get("t:l"); s.State != wire.PhaseWaiting || s.Reason != wire.ReasonQuestion {
		t.Fatalf("Orca within the lag: %+v", s)
	}
	c.advance(sessions.OrcaLag)
	w.apply(idle("waiting"), nil)
	if s, _ := book.Get("t:l"); s.Reason != wire.ReasonQuestion {
		t.Fatalf("Orca waiting must keep the hook reason: %+v", s)
	}
	w.apply(idle("working"), nil)
	if s, _ := book.Get("t:l"); s.State != wire.PhaseWorking {
		t.Fatalf("Orca after the lag: %+v", s)
	}
}

// Orca went to monitoring 3 minutes after the turn, on the recap of Claude, with no shell (OVERSIRE-335).
func TestStaleMonitoring(t *testing.T) {
	var a orca.AgentCard
	card := `{"paneKey":"t:l","state":"working","workingMode":"monitoring","agentType":"claude","toolName":"Bash",
		"mainAgent":{"state":"done","stateStartedAt":1792925979532},"stateStartedAt":1792926167685,"updatedAt":1792926167685}`
	if err := json.Unmarshal([]byte(card), &a); err != nil {
		t.Fatal(err)
	}
	c := newClock()
	book := sessions.NewBook("mbp", "", nil, c.Now)
	w := NewWatcher(&fakeLister{}, book, c)
	done, end := wire.PhaseDone, wire.ReasonTurnEnd
	book.Apply(sessions.Patch{ID: "t:l", Source: sessions.SourceHook, At: c.Now(), State: &done, Reason: &end})
	c.advance(sessions.OrcaLag + time.Second)
	if w.apply([]orca.WorktreeCard{{ID: "w1", Repo: "app", Path: "/p", Agents: []orca.AgentCard{a}}}, nil) {
		t.Fatal("a stale monitoring card is not busy")
	}
	if s, _ := book.Get("t:l"); s.State != wire.PhaseDone || s.Reason != wire.ReasonTurnEnd {
		t.Fatalf("%+v", s)
	}
}
