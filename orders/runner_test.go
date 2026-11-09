package orders

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// slowRuntimes launches one new session for each call, after a pause, so that two orders overlap.
type slowRuntimes struct {
	mu    sync.Mutex
	calls int
	last  wire.WorkOrder
}

func (f *slowRuntimes) Has(id string) bool { return id == "oversire" || id == wire.RuntimeOrca }
func (f *slowRuntimes) Do(_ context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	f.mu.Lock()
	f.last = o
	f.mu.Unlock()
	if _, ok := o.Payload.(wire.StopPayload); ok {
		return &wire.WorkOrderResult{}, nil
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/enhance"}, ID: fmt.Sprintf("s-%d", n)}
	return &wire.WorkOrderResult{Session: &s, PaneKey: s.ID}, nil
}

func (f *slowRuntimes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func keyed(id, key string) wire.WorkOrder {
	o := order(id, "claude", now.Add(time.Hour))
	p := o.Payload.(wire.LaunchRequest)
	p.Key = key
	o.Payload = p
	return o
}

func keyRig() (*Runner, *slowRuntimes, *sessions.Book) {
	rts := &slowRuntimes{}
	book := sessions.NewBook("mbp", "", nil, func() time.Time { return now })
	r := NewRunner(rts, fakeAllow{agent: "claude"}, &memJournal{res: map[string]wire.Result{}, sent: map[string]bool{}}, func() time.Time { return now })
	return r.WithKeys(book), rts, book
}

func trusted() context.Context {
	return server.WithCaller(context.Background(), server.Caller{Loopback: true})
}

func TestKeyedOrdersStartOneSession(t *testing.T) {
	r, rts, book := keyRig()
	var wg sync.WaitGroup
	res := make([]wire.Result, 2)
	for i := range res {
		wg.Go(func() { res[i], _ = r.Run(trusted(), keyed(fmt.Sprintf("wo-%d", i), "enhance:OVERSIRE-317"), Hooks{}) })
	}
	wg.Wait()
	if rts.count() != 1 {
		t.Fatalf("launches = %d, want 1", rts.count())
	}
	for _, x := range res {
		if x.State != wire.ResultDone || x.Result.Session == nil || x.Result.Session.ID != "s-1" || x.Result.PaneKey != "s-1" {
			t.Fatalf("result = %+v", x)
		}
	}
	if s, ok := book.ByKey("oversire", "enhance:OVERSIRE-317"); !ok || s.ID != "s-1" || s.Workspace != "app/enhance" || s.Terminal != "" {
		t.Fatalf("book = %+v %v", s, ok)
	}
}

func TestKeyedOrderAfterTheEndLaunchesAgain(t *testing.T) {
	r, rts, book := keyRig()
	r.Run(trusted(), keyed("wo-1", "enhance:K-1"), Hooks{})
	book.Apply(sessions.Patch{ID: "s-1", Source: sessions.SourceWire, End: true})
	res, _ := r.Run(trusted(), keyed("wo-2", "enhance:K-1"), Hooks{})
	if rts.count() != 2 || res.Result.Session.ID != "s-2" {
		t.Fatalf("launches = %d, result = %+v", rts.count(), res.Result)
	}
}

func TestUnkeyedOrdersAlwaysLaunch(t *testing.T) {
	r, rts, _ := keyRig()
	r.Run(trusted(), order("wo-1", "claude", now.Add(time.Hour)), Hooks{})
	r.Run(trusted(), order("wo-2", "claude", now.Add(time.Hour)), Hooks{})
	if rts.count() != 2 {
		t.Fatalf("launches = %d, want 2", rts.count())
	}
}

func TestKeyedOrderChecks(t *testing.T) {
	r, rts, _ := keyRig()
	r.Run(trusted(), keyed("wo-1", "enhance:K-1"), Hooks{})

	// A hub order with an agent that the allow list does not have learns nothing of the session.
	o := keyed("wo-2", "enhance:K-1")
	p := o.Payload.(wire.LaunchRequest)
	p.Agent = "codex"
	o.Payload = p
	res, _ := r.Run(context.Background(), o, Hooks{})
	if res.State != wire.ResultFailed || res.Result.Session != nil {
		t.Fatalf("refused order = %+v", res.Result)
	}
	res, _ = r.Run(trusted(), keyed("wo-3", "Enhance K-1"), Hooks{})
	if res.State != wire.ResultFailed || res.Result.Error != `The start key "Enhance K-1" is not valid.` {
		t.Fatalf("bad key = %+v", res.Result)
	}
	if rts.count() != 1 {
		t.Fatalf("launches = %d, want 1", rts.count())
	}
}

func TestKeyedOrderGivesTheTerminal(t *testing.T) {
	r, rts, book := keyRig()
	book.Apply(sessions.Patch{ID: "tab-1:leaf-1", Source: sessions.SourceOrca, Runtime: ptr(wire.RuntimeOrca), Workspace: ptr("wt-1"),
		Terminal: ptr("term_1"), Key: ptr("enhance:K-1")})
	res, _ := r.Run(trusted(), onOrca(keyed("wo-1", "enhance:K-1")), Hooks{})
	want := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: "local"}, ID: "wt-1"}, ID: "term_1"}
	if rts.count() != 0 || *res.Result.Session != want || res.Result.PaneKey != "tab-1:leaf-1" || !res.Result.Reused {
		t.Fatalf("launches = %d, result = %+v", rts.count(), res.Result)
	}
}

func TestStopFreesTheKey(t *testing.T) {
	r, rts, book := keyRig()
	book.Apply(sessions.Patch{ID: "tab-1:leaf-1", Source: sessions.SourceOrca, Runtime: ptr(wire.RuntimeOrca), Workspace: ptr("wt-1"),
		Terminal: ptr("term_1"), Key: ptr("enhance:K-1")})
	stop := order("wo-1", "claude", now.Add(time.Hour))
	stop.Payload = wire.StopPayload{Session: wire.SessionRef{Workspace: wire.WorkspaceRef{ID: "wt-1"}, ID: "term_1"}}
	if res, _ := r.Run(trusted(), stop, Hooks{}); res.State != wire.ResultDone {
		t.Fatalf("stop = %+v", res.Result)
	}
	if _, ok := book.ByKey(wire.RuntimeOrca, "enhance:K-1"); ok {
		t.Fatal("the stopped session keeps its key")
	}
	r.Run(trusted(), onOrca(keyed("wo-2", "enhance:K-1")), Hooks{})
	if rts.count() != 1 {
		t.Fatalf("launches = %d, want 1", rts.count())
	}
}

func TestKeyedOrderOnAnotherRuntimeLaunches(t *testing.T) {
	r, rts, book := keyRig()
	first, _ := r.Run(trusted(), keyed("wo-1", "work:MCPQ-52"), Hooks{})
	if first.Result.Reused {
		t.Fatalf("a new session is reused: %+v", first.Result)
	}
	again, _ := r.Run(trusted(), keyed("wo-2", "work:MCPQ-52"), Hooks{})
	if rts.count() != 1 || !again.Result.Reused || again.Result.Session.ID != "s-1" {
		t.Fatalf("same runtime: launches = %d, result = %+v", rts.count(), again.Result)
	}
	other, _ := r.Run(trusted(), onOrca(keyed("wo-3", "work:MCPQ-52")), Hooks{})
	if rts.count() != 2 || other.Result.Reused || other.Result.Session.ID != "s-2" {
		t.Fatalf("other runtime: launches = %d, result = %+v", rts.count(), other.Result)
	}
	if s, ok := book.ByKey("oversire", "work:MCPQ-52"); !ok || s.ID != "s-1" {
		t.Fatalf("oversire key = %+v %v", s, ok)
	}
	if s, ok := book.ByKey(wire.RuntimeOrca, "work:MCPQ-52"); !ok || s.ID != "s-2" {
		t.Fatalf("orca key = %+v %v", s, ok)
	}
}

func onOrca(o wire.WorkOrder) wire.WorkOrder {
	o.Runtime = wire.RuntimeOrca
	return o
}

func ptr[T any](v T) *T { return &v }

func TestStartKeepsModelEffortAndProfile(t *testing.T) {
	r, _, book := keyRig()
	o := order("wo-1", "claude", now.Add(time.Hour))
	p := o.Payload.(wire.LaunchRequest)
	p.Model, p.Effort, p.Profile = "claude-opus-5-5", "max", "claude-max"
	o.Payload = p
	if res, _ := r.Run(trusted(), o, Hooks{}); res.State != wire.ResultDone {
		t.Fatalf("result = %+v", res.Result)
	}
	s, ok := book.Get("s-1")
	if !ok || s.Model != "claude-opus-5-5" || s.Effort != "max" || s.Profile != "claude-max" || s.Key != "" {
		t.Fatalf("book = %+v %v", s, ok)
	}
}

func TestLaunchAndStopOfTheDaemon(t *testing.T) {
	r, rts, book := keyRig()
	o := runtime.Order{ID: "runbook-rb_1.plan.1", ExpiresAt: now.Add(time.Hour)}
	req := wire.LaunchRequest{Repo: "/r", Agent: "codex", Key: "runbook:rb_1.plan.1", Profile: "codex", Prompt: "go"}
	st, err := r.Launch(context.Background(), "oversire", o, req)
	if err != nil || st.Session.ID != "s-1" || st.PaneKey != "s-1" {
		t.Fatalf("launch = %+v %v", st, err)
	}
	if s, ok := book.ByKey("oversire", req.Key); !ok || s.Profile != "codex" {
		t.Fatalf("book = %+v %v", s, ok)
	}
	if !r.journal.(*memJournal).sent[o.ID] {
		t.Fatal("the hub gets the result")
	}
	again, err := r.Launch(context.Background(), "oversire", o, req)
	if err != nil || again.Session.ID != "s-1" || rts.count() != 1 {
		t.Fatalf("the same order launched again: %+v %v", again, err)
	}
	if err := r.Stop(context.Background(), "oversire", runtime.Order{ID: "stop-1", ExpiresAt: now.Add(time.Hour)}, st.Session); err != nil {
		t.Fatal(err)
	}
	if _, ok := book.ByKey("oversire", req.Key); ok {
		t.Fatal("the stop keeps the key")
	}
	if _, err := r.Launch(context.Background(), "orca", runtime.Order{ID: "x", ExpiresAt: now.Add(-time.Hour)}, req); err == nil {
		t.Fatal("an expired order launched")
	}
	if _, err := r.Launch(context.Background(), "nope", runtime.Order{ID: "y", ExpiresAt: now.Add(time.Hour)}, req); err == nil {
		t.Fatal("an unknown runtime launched")
	}
}

func TestKeyedStartKeepsTheOverseer(t *testing.T) {
	r, _, book := keyRig()
	o := keyed("wo-1", "work:OVERSIRE-12")
	p := o.Payload.(wire.LaunchRequest)
	p.Overseer = "claude-1"
	o.Payload = p
	if res, _ := r.Run(trusted(), o, Hooks{}); res.State != wire.ResultDone {
		t.Fatalf("result = %+v", res.Result)
	}
	if s, ok := book.Get("s-1"); !ok || s.Overseer != "claude-1" {
		t.Fatalf("book = %+v %v", s, ok)
	}
}

func TestReusedStartSetsTheNewOverseer(t *testing.T) {
	r, rts, book := keyRig()
	req := wire.LaunchRequest{Repo: "/r", Agent: "codex", Key: "work:OVERSIRE-12", Prompt: "go", Overseer: "claude-1"}
	if _, err := r.Launch(context.Background(), "oversire", runtime.Order{ID: "a", ExpiresAt: now.Add(time.Hour)}, req); err != nil {
		t.Fatal(err)
	}
	req.Overseer = "claude-2"
	st, err := r.Launch(context.Background(), "oversire", runtime.Order{ID: "b", ExpiresAt: now.Add(time.Hour)}, req)
	if err != nil || !st.Reused || rts.count() != 1 {
		t.Fatalf("second start = %+v %v, launches %d", st, err, rts.count())
	}
	if s, _ := book.Get("s-1"); s.Overseer != "claude-2" {
		t.Fatalf("overseer = %q", s.Overseer)
	}
}

func TestLaunchGivesReused(t *testing.T) {
	r, _, _ := keyRig()
	req := wire.LaunchRequest{Repo: "/r", Agent: "codex", Key: "work:OVERSIRE-12", Prompt: "go"}
	first, err := r.Launch(context.Background(), "oversire", runtime.Order{ID: "a", ExpiresAt: now.Add(time.Hour)}, req)
	if err != nil || first.Reused {
		t.Fatalf("first = %+v %v", first, err)
	}
	second, err := r.Launch(context.Background(), "oversire", runtime.Order{ID: "b", ExpiresAt: now.Add(time.Hour)}, req)
	if err != nil || !second.Reused || second.Session.ID != first.Session.ID {
		t.Fatalf("second = %+v %v", second, err)
	}
}

func TestRunnerSendRunsASessionSendOrder(t *testing.T) {
	r, rts, _ := keyRig()
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/work"}, ID: "s-1"}
	p := wire.SendPayload{Session: ref, Text: "go on", Origin: wire.OriginOverseer}
	if err := r.Send(context.Background(), "oversire", runtime.Order{ID: "send-1", ExpiresAt: now.Add(time.Hour)}, p); err != nil {
		t.Fatal(err)
	}
	rts.mu.Lock()
	got, ok := rts.last.Payload.(wire.SendPayload)
	rts.mu.Unlock()
	if !ok || got.Text != "go on" || got.Origin != wire.OriginOverseer || rts.last.Runtime != "oversire" {
		t.Fatalf("last order = %+v", rts.last)
	}
	if !r.journal.(*memJournal).sent["send-1"] {
		t.Fatal("the hub gets the result")
	}
	if err := r.Send(context.Background(), "oversire", runtime.Order{ID: "send-2", ExpiresAt: now.Add(-time.Hour)}, p); err == nil {
		t.Fatal("an expired order sent")
	}
}
