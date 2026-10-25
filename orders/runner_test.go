package orders

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// slowRuntimes launches one new session for each call, after a pause, so that two orders overlap.
type slowRuntimes struct {
	mu    sync.Mutex
	calls int
}

func (f *slowRuntimes) Has(id string) bool { return id == "oversire" }
func (f *slowRuntimes) Do(_ context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
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

func trusted() context.Context { return server.WithCaller(context.Background(), server.Caller{Loopback: true}) }

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
	if s, ok := book.ByKey("enhance:OVERSIRE-317"); !ok || s.ID != "s-1" || s.Workspace != "app/enhance" || s.Terminal != "" {
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
	res, _ := r.Run(trusted(), keyed("wo-1", "enhance:K-1"), Hooks{})
	want := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: "local"}, ID: "wt-1"}, ID: "term_1"}
	if rts.count() != 0 || *res.Result.Session != want || res.Result.PaneKey != "tab-1:leaf-1" {
		t.Fatalf("launches = %d, result = %+v", rts.count(), res.Result)
	}
}

func ptr[T any](v T) *T { return &v }
