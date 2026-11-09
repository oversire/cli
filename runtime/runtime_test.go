package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

type fake struct {
	read   int
	order  Order
	sent   wire.SendPayload
	budget wire.HistoryBudget
}

func (f *fake) ID() string { return "fake" }
func (f *fake) Start(_ context.Context, o Order, _ wire.LaunchRequest) (wire.SessionRef, error) {
	f.order = o
	return wire.SessionRef{ID: "s1"}, nil
}
func (f *fake) Stop(context.Context, wire.SessionRef) error { return nil }
func (f *fake) Read(_ context.Context, _ wire.SessionRef, n int) ([]string, error) {
	f.read = n
	return nil, nil
}
func (f *fake) Send(_ context.Context, p wire.SendPayload) error {
	f.sent = p
	return nil
}
func (f *fake) History(_ context.Context, _ wire.SessionRef, b wire.HistoryBudget) (wire.SessionDigest, error) {
	f.budget = b
	return wire.SessionDigest{Harness: "claude"}, nil
}

func TestRegistry(t *testing.T) {
	f := &fake{}
	r := NewRegistry(f)
	if !slices.Equal(r.IDs(), []string{"fake"}) || !r.Has("fake") || r.Has("orca") {
		t.Fatal(r.IDs())
	}
	ctx := context.Background()
	exp := time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC)
	res, err := r.Do(ctx, wire.WorkOrder{ID: "o1", ExpiresAt: exp, Runtime: "fake", Payload: wire.LaunchRequest{}})
	if err != nil || res.Session.ID != "s1" || f.order != (Order{ID: "o1", ExpiresAt: exp}) {
		t.Fatal(res, err, f.order)
	}
	answer := &wire.PermissionAnswer{ID: "p-1", Allow: true}
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.SendPayload{Permission: answer}}); err != nil || f.sent.Permission != answer {
		t.Fatal(f.sent, err)
	}
	res, err = r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.ReadPayload{Lines: 5000}})
	if err != nil || f.read != MaxReadLines || res.Lines == nil {
		t.Fatal(f.read, res, err)
	}
	res, err = r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.HistoryPayload{}})
	if e, fu, c := f.budget.Values(); err != nil || res.Digest == nil || e != wire.DefaultEarlier || fu != wire.DefaultFull || c != wire.DefaultMaxChars {
		t.Fatal(f.budget, res, err)
	}
	big := wire.NewHistoryBudget(1000, -1, 1<<30)
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.HistoryPayload{Budget: &big}}); err != nil {
		t.Fatal(err)
	}
	if e, fu, c := f.budget.Values(); e != MaxHistoryEarlier || fu != 0 || c != MaxHistoryChars {
		t.Fatal(e, fu, c)
	}
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "orca", Payload: wire.StopPayload{}}); err == nil {
		t.Fatal("an unknown runtime passed")
	}
}

// watcher is a fake runtime with Oversight.
type watcher struct {
	fake
	ref wire.SessionRef
	q   *wire.RunnerQuestion
}

func (w *watcher) ID() string { return "watcher" }
func (w *watcher) Inspect(_ context.Context, s wire.SessionRef) (Inspection, error) {
	if s.ID != "s1" {
		return Inspection{}, ErrNotFound
	}
	w.ref = s
	return Inspection{Tokens: 7}, nil
}
func (w *watcher) Escalate(_ context.Context, s wire.SessionRef, q *wire.RunnerQuestion) error {
	w.ref, w.q = s, q
	return nil
}

func TestRegistryInspectAndEscalate(t *testing.T) {
	w := &watcher{}
	r := NewRegistry(&fake{}, w)
	ctx := context.Background()
	ses := wire.DaemonSession{ID: "s1", Runtime: "watcher", Workspace: "app/work"}
	in, err := r.Inspect(ctx, ses)
	if err != nil || in.Tokens != 7 || w.ref.ID != "s1" || w.ref.Workspace.ID != "app/work" || w.ref.Workspace.Account.Provider != "watcher" {
		t.Fatalf("inspect = %+v %v, ref %+v", in, err, w.ref)
	}
	q := &wire.RunnerQuestion{ID: "esc-1"}
	if err := r.Escalate(ctx, ses, q); err != nil || w.q != q {
		t.Fatalf("escalate = %v, q %v", err, w.q)
	}
	if _, err := r.Inspect(ctx, wire.DaemonSession{ID: "s9", Runtime: "watcher"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := r.Inspect(ctx, wire.DaemonSession{ID: "s1", Runtime: "fake"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a runtime with no Oversight: %v", err)
	}
	if err := r.Escalate(ctx, wire.DaemonSession{ID: "s1", Runtime: "fake"}, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a runtime with no Oversight: %v", err)
	}
	if _, err := r.Inspect(ctx, wire.DaemonSession{ID: "s1", Runtime: "none"}); !errors.Is(err, ErrNoRuntime) {
		t.Fatalf("unknown runtime: %v", err)
	}
}
