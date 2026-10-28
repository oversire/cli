package sessions

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

type memLog struct {
	mu     sync.Mutex
	events []wire.ProviderEvent
}

func (m *memLog) Append(e wire.ProviderEvent) {
	m.mu.Lock()
	m.events = append(m.events, e)
	m.mu.Unlock()
}

func (m *memLog) actions() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s []string
	for _, e := range m.events {
		s = append(s, e.Action)
	}
	return strings.Join(s, ",")
}

var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func at(d time.Duration) time.Time { return t0.Add(d) }

func orca(id string, d time.Duration, st wire.SessionPhase) Patch {
	return Patch{ID: id, Source: SourceOrca, At: at(d), Runtime: ptr(wire.RuntimeOrca), Harness: ptr("claude"), Cwd: ptr("/src/app"), State: &st}
}

func hook(id string, d time.Duration, st wire.SessionPhase) Patch {
	return Patch{ID: id, Source: SourceHook, At: at(d), State: &st}
}

func newBook(path string) (*Book, *memLog) {
	l := &memLog{}
	return NewBook("mini", path, l, func() time.Time { return t0 }), l
}

func TestPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		patches []Patch
		state   wire.SessionPhase
		reason  wire.SessionReason
	}{
		{"hook then lagging orca", []Patch{orca("s", 0, wire.PhaseWorking), hook("s", time.Second, wire.PhaseWorking), orca("s", 3*time.Second, wire.PhaseWaiting)}, wire.PhaseWorking, ""},
		{"orca 6 s after hook", []Patch{orca("s", 0, wire.PhaseWorking), hook("s", time.Second, wire.PhaseWorking), orca("s", 7*time.Second, wire.PhaseWaiting)}, wire.PhaseWaiting, ""},
		{"hook newer than orca", []Patch{orca("s", 0, wire.PhaseWaiting), hook("s", time.Second, wire.PhaseWorking)}, wire.PhaseWorking, ""},
		{"hook older than orca", []Patch{orca("s", 2*time.Second, wire.PhaseWaiting), hook("s", time.Second, wire.PhaseWorking)}, wire.PhaseWaiting, ""},
		{"monitoring from orca", []Patch{func() Patch {
			p := orca("s", 0, wire.PhaseWorking)
			p.Reason = ptr(wire.ReasonMonitoring)
			return p
		}()}, wire.PhaseWorking, wire.ReasonMonitoring},
		{"no monitoring from hook", []Patch{orca("s", 0, wire.PhaseWorking), {ID: "s", Source: SourceHook, At: at(time.Second), Reason: ptr(wire.ReasonMonitoring)}}, wire.PhaseWorking, ""},
		{"wire owns its session", []Patch{
			{ID: "s", Source: SourceWire, At: at(0), Runtime: ptr(wire.RuntimeOversire), State: ptr(wire.PhaseWorking)},
			hook("s", time.Second, wire.PhaseDone),
		}, wire.PhaseWorking, ""},
		{"done with a question", []Patch{{ID: "s", Source: SourceHook, At: at(0), Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseDone), LastMessage: ptr("Fixed.\n\nShould I push?")}}, wire.PhaseDone, wire.ReasonQuestion},
		{"done without a question", []Patch{{ID: "s", Source: SourceHook, At: at(0), Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseDone), LastMessage: ptr("Pushed.")}}, wire.PhaseDone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := newBook("")
			for _, p := range c.patches {
				b.Apply(p)
			}
			s, ok := b.Get("s")
			if !ok || s.State != c.state || s.Reason != c.reason {
				t.Fatalf("got %s/%s, want %s/%s", s.State, s.Reason, c.state, c.reason)
			}
		})
	}
}

func TestEvents(t *testing.T) {
	b, l := newBook("")
	b.Apply(orca("s", 0, wire.PhaseWorking))
	if l.actions() != "" {
		t.Fatal("sent before the window ends")
	}
	b.Flush(at(Coalesce))
	if l.actions() != "working" {
		t.Fatalf("got %q", l.actions())
	}
	e := l.events[0]
	if e.ID != "daemon-mini-s-working-"+strconv.FormatInt(t0.UnixMilli(), 10) || e.Provider != "orca" || e.Account.ID != "mini" || e.Kind != "session" ||
		e.Summary.Title != "app" || e.Summary.Actor != "claude" || !strings.Contains(string(e.Subject.Ref), `"id":"/src/app"`) {
		t.Fatalf("event %+v %s", e, e.Subject.Ref)
	}

	// The last message alone sends no event.
	b.Apply(Patch{ID: "s", Source: SourceOrca, At: at(time.Second), LastMessage: ptr("hi")})
	b.Flush(at(2 * time.Second))
	if l.actions() != "working" {
		t.Fatalf("got %q", l.actions())
	}
	// Coalescing: waiting then working then done within 300 ms gives only done.
	b.Apply(orca("s", 3*time.Second, wire.PhaseWaiting))
	b.Apply(orca("s", 3*time.Second+100*time.Millisecond, wire.PhaseWorking))
	b.Apply(orca("s", 3*time.Second+200*time.Millisecond, wire.PhaseDone))
	b.Flush(at(4 * time.Second))
	if l.actions() != "working,done" {
		t.Fatalf("got %q", l.actions())
	}
	// A flap back to the sent state within the window sends nothing.
	b.Apply(orca("s", 5*time.Second, wire.PhaseWorking))
	b.Apply(orca("s", 5*time.Second+100*time.Millisecond, wire.PhaseDone))
	b.Flush(at(6 * time.Second))
	if l.actions() != "working,done" {
		t.Fatalf("got %q", l.actions())
	}
	// A new question with the same state.
	b.Apply(Patch{ID: "s", Source: SourceHook, At: at(7 * time.Second), Question: ptr("q1")})
	b.Flush(at(8 * time.Second))
	if l.actions() != "working,done,question" {
		t.Fatalf("got %q", l.actions())
	}
	// Orca no longer lists the pane.
	b.Apply(Patch{ID: "s", Source: SourceOrca, At: at(9 * time.Second), End: true})
	if l.actions() != "working,done,question,removed" {
		t.Fatalf("got %q", l.actions())
	}
	if _, ok := b.Get("s"); ok {
		t.Fatal("not removed")
	}
}

func TestStableIDs(t *testing.T) {
	ids := func() string {
		b, l := newBook("")
		b.Apply(orca("s", 0, wire.PhaseWorking))
		b.Flush(at(time.Second))
		return l.events[0].ID
	}
	if a, c := ids(), ids(); a != c {
		t.Fatalf("%s != %s", a, c)
	}
}

func TestExpiry(t *testing.T) {
	b, l := newBook("")
	b.Apply(orca("done", 0, wire.PhaseDone))
	b.Apply(Patch{ID: "cli", Source: SourceHook, At: at(0), Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseWorking)})
	b.Apply(Patch{ID: "cli", Source: SourceHook, At: at(time.Minute), End: true})
	b.Apply(orca("live", 0, wire.PhaseWorking))
	b.Flush(at(2 * time.Minute))

	b.Prune(at(10 * time.Minute))
	if len(b.All()) != 3 {
		t.Fatal("pruned too early")
	}
	b.Prune(at(11 * time.Minute))
	if _, ok := b.Get("cli"); ok || len(b.All()) != 2 {
		t.Fatal("cli session stays after its end")
	}
	b.Prune(at(DoneTTL))
	if _, ok := b.Get("done"); ok || len(b.All()) != 1 {
		t.Fatal("done session stays")
	}
	if !strings.HasSuffix(l.actions(), "removed,removed") {
		t.Fatalf("got %q", l.actions())
	}
}

func TestOrcaKeepsDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	b, l := newBook(path)
	b.Apply(orca("s", 0, wire.PhaseDone))
	b.Apply(orca("s", DoneTTL, wire.PhaseDone))
	b.Prune(at(DoneTTL + time.Hour))
	if _, ok := b.Get("s"); !ok {
		t.Fatal("pruned while Orca reports it")
	}
	if err := b.Save(true); err != nil {
		t.Fatal(err)
	}
	c := NewBook("mini", path, nil, func() time.Time { return at(3 * DoneTTL) })
	c.Prune(at(3 * DoneTTL))
	if _, ok := c.Get("s"); !ok {
		t.Fatal("pruned at load, before the first Orca poll")
	}
	b.Prune(at(2 * DoneTTL))
	if _, ok := b.Get("s"); ok || !strings.HasSuffix(l.actions(), "removed") {
		t.Fatalf("stays after Orca stops to report it: %q", l.actions())
	}
}

func TestOrder(t *testing.T) {
	b, _ := newBook("")
	b.Apply(orca("a", 0, wire.PhaseWorking))
	b.Apply(orca("b", time.Second, wire.PhaseWorking))
	b.Apply(Patch{ID: "a", Source: SourceOrca, At: at(2 * time.Second), Prompt: ptr(strings.Repeat("x", wire.MaxSessionPrompt+5))})
	all := b.All()
	if all[0].ID != "a" || all[1].ID != "b" || len([]rune(all[0].Prompt)) != wire.MaxSessionPrompt {
		t.Fatalf("%+v", all)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	b, _ := newBook(path)
	b.Apply(orca("s", 0, wire.PhaseWaiting))
	b.Apply(Patch{ID: "s", Source: SourceOrca, At: at(time.Second), Repo: ptr("app"), Branch: ptr("main")})
	if err := b.Save(true); err != nil {
		t.Fatal(err)
	}
	c, _ := newBook(path)
	got, ok := c.Get("s")
	want, _ := b.Get("s")
	if !ok || got.State != want.State || got.Repo != "app" || !got.UpdatedAt.Equal(want.UpdatedAt) || got.Machine != "mini" {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(at(0)) {
		t.Fatalf("a restart keeps the start: %v", got.StartedAt)
	}
}

func TestRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	l := &memLog{}
	b := NewBook("mini", path, l, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Run(ctx) }()
	b.Apply(Patch{ID: "s", Source: SourceHook, Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseWorking)})
	deadline := time.Now().Add(2 * time.Second)
	for l.actions() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || l.actions() != "working" {
		t.Fatalf("%v %q", err, l.actions())
	}
	if _, ok := NewBook("mini", path, nil, nil).Get("s"); !ok {
		t.Fatal("not saved at stop")
	}
}

func TestSince(t *testing.T) {
	b, _ := newBook("")
	p := orca("s", time.Minute, wire.PhaseWaiting)
	p.Since = at(10 * time.Second)
	b.Apply(p)
	if s, _ := b.Get("s"); !s.StateSince.Equal(at(10 * time.Second)) {
		t.Fatalf("got %v", s.StateSince)
	}
	// The same state keeps its start. A start after At is not trusted.
	p = orca("s", 2*time.Minute, wire.PhaseWorking)
	p.Since = at(3 * time.Minute)
	b.Apply(p)
	s, _ := b.Get("s")
	if !s.StateSince.Equal(at(2 * time.Minute)) {
		t.Fatalf("got %v", s.StateSince)
	}
	// The start is the first time that the book knows, and a new state does not move it.
	if s.StartedAt == nil || !s.StartedAt.Equal(at(10*time.Second)) {
		t.Fatalf("started %v", s.StartedAt)
	}
}

func TestByKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	b, _ := newBook(path)
	b.Apply(Patch{ID: "live", Source: SourceHook, At: at(0), Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseWorking), Key: ptr("enhance:K-1")})
	b.Apply(Patch{ID: "gone", Source: SourceHook, At: at(0), Runtime: ptr(wire.RuntimeCLI), State: ptr(wire.PhaseDone), Key: ptr("enhance:K-2")})
	b.Apply(Patch{ID: "gone", Source: SourceHook, At: at(time.Second), End: true})
	if s, ok := b.ByKey(wire.RuntimeCLI, "enhance:K-1"); !ok || s.ID != "live" || s.Key != "enhance:K-1" {
		t.Fatalf("live: %+v %v", s, ok)
	}
	if _, ok := b.ByKey(wire.RuntimeCLI, "enhance:K-2"); ok {
		t.Fatal("an ended session keeps its key")
	}
	if _, ok := b.ByKey(wire.RuntimeCLI, "enhance:K-3"); ok {
		t.Fatal("a missing key gives a session")
	}
	if err := b.Save(true); err != nil {
		t.Fatal(err)
	}
	c, _ := newBook(path)
	if s, ok := c.ByKey(wire.RuntimeCLI, "enhance:K-1"); !ok || s.ID != "live" {
		t.Fatalf("after load: %+v %v", s, ok)
	}
}

func TestUnseenKeyedSessionExpires(t *testing.T) {
	b, _ := newBook("")
	b.Apply(Patch{ID: "tab-1:leaf-1", At: at(0), Runtime: ptr(wire.RuntimeOrca), Key: ptr("enhance:K-1")})
	b.Apply(Patch{ID: "tab-2:leaf-1", At: at(0), Runtime: ptr(wire.RuntimeOrca), Key: ptr("enhance:K-2")})
	b.Apply(orca("tab-2:leaf-1", time.Second, wire.PhaseWorking))
	if s, _ := b.Get("tab-1:leaf-1"); len(s.Sources) != 0 || s.State != wire.PhaseStarting {
		t.Fatalf("a key patch is no source: %+v", s)
	}
	b.Prune(at(UnseenTTL))
	if _, ok := b.ByKey(wire.RuntimeOrca, "enhance:K-1"); ok {
		t.Fatal("a session that no source reported keeps its key")
	}
	if _, ok := b.ByKey(wire.RuntimeOrca, "enhance:K-2"); !ok {
		t.Fatal("a reported session lost its key")
	}
}
