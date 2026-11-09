package worker

import (
	"context"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// startedRig starts a worker on s1 at t0 with an overseer.
func startedRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	if _, _, err := r.e.Start(context.Background(), startReq()); err != nil {
		t.Fatal(err)
	}
	return r
}

func asking(at time.Time) runtime.Inspection {
	return runtime.Inspection{AskedAt: at, Session: wire.RunnerSession{State: wire.SessionWaitingPermission,
		Permission: &wire.PermissionPrompt{ID: "p-1", Tool: "Bash", Text: "rm x"}}}
}

func TestFallbackEscalatesAfter10Minutes(t *testing.T) {
	r := startedRig(t)
	ctx := context.Background()
	r.watch.set("s1", asking(t0))
	r.clock.Advance(9 * time.Minute)
	r.e.tick(ctx)
	if len(r.watch.calls()) != 0 {
		t.Fatal("escalated too early")
	}
	r.clock.Advance(time.Minute)
	r.e.tick(ctx)
	r.e.tick(ctx)
	calls := r.watch.calls()
	if len(calls) != 1 || calls[0].Q != nil {
		t.Fatalf("calls %+v", calls)
	}
	got := r.got()
	last := got[len(got)-1]
	if len(got) != 2 || last.Kind != wire.KindWorkerEscalated || last.Actor != ActorSystem || last.Session != "ov1" || last.Payload["reason"] != "fallback" {
		t.Fatalf("changes %+v", got)
	}
}

func TestFallbackSkipsAnEscalationQuestion(t *testing.T) {
	r := startedRig(t)
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput,
		Question: &wire.RunnerQuestion{ID: "esc-1"}}})
	r.clock.Advance(20 * time.Minute)
	r.e.tick(context.Background())
	if len(r.watch.calls()) != 0 {
		t.Fatalf("calls %+v", r.watch.calls())
	}
}

func TestOverseerActionDelaysTheFallback(t *testing.T) {
	r := startedRig(t)
	ctx := context.Background()
	r.watch.set("s1", asking(t0))
	r.clock.Advance(8 * time.Minute)
	if err := r.e.Send(ctx, "s1", "Go on."); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * time.Minute) // minute 10: 2 minutes after the action
	r.e.tick(ctx)
	r.clock.Advance(7 * time.Minute) // minute 17
	r.e.tick(ctx)
	if len(r.watch.calls()) != 0 {
		t.Fatal("the action did not move the fallback")
	}
	r.clock.Advance(time.Minute) // minute 18
	r.e.tick(ctx)
	if len(r.watch.calls()) != 1 {
		t.Fatalf("calls %+v", r.watch.calls())
	}
}

func TestBudgetPausesAndEscalates(t *testing.T) {
	r := startedRig(t)
	ctx := context.Background()
	r.clock.Advance(DefaultBudget - time.Minute)
	r.e.tick(ctx)
	if len(r.watch.calls()) != 0 || r.ints.Load() != 0 {
		t.Fatal("budget used too early")
	}
	r.clock.Advance(time.Minute)
	r.e.tick(ctx)
	r.e.tick(ctx)
	calls := r.watch.calls()
	if r.ints.Load() != 1 || len(calls) != 1 || calls[0].Q == nil || calls[0].Q.Items[0].Text != budgetAsk {
		t.Fatalf("ints %d calls %+v", r.ints.Load(), calls)
	}
	got := r.got()
	last := got[len(got)-1]
	if last.Kind != wire.KindWorkerEscalated || last.Actor != ActorSystem || last.Payload["reason"] != "budget" || last.Payload["question"] != budgetAsk {
		t.Fatalf("changes %+v", got)
	}
}

func TestBudgetAnswerAddsThreeHours(t *testing.T) {
	r := startedRig(t)
	ctx := context.Background()
	r.clock.Advance(DefaultBudget)
	r.e.tick(ctx)
	id := r.watch.calls()[0].Q.ID
	open := runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput, Question: &wire.RunnerQuestion{ID: id}}}
	r.watch.set("s1", open)
	r.e.tick(ctx)
	if rec, _ := r.store.Load("s1"); rec.Escalation == nil || rec.Budget != DefaultBudget {
		t.Fatalf("open question: %+v", rec)
	}
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWorking}})
	r.e.tick(ctx)
	rec, _ := r.store.Load("s1")
	if rec.Escalation != nil || rec.Budget != 2*DefaultBudget {
		t.Fatalf("answered: %+v", rec)
	}
	r.e.tick(ctx)
	if len(r.watch.calls()) != 1 {
		t.Fatal("the budget escalated again at once")
	}
	r.clock.Advance(DefaultBudget)
	r.e.tick(ctx)
	if len(r.watch.calls()) != 2 {
		t.Fatalf("calls %+v", r.watch.calls())
	}
}

func TestBudgetSkipsAWorkerThatWaitsForACheck(t *testing.T) {
	r := startedRig(t)
	if _, err := r.e.Done(context.Background(), "s1", "done", ""); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * DefaultBudget)
	r.e.tick(context.Background())
	if len(r.watch.calls()) != 0 || r.ints.Load() != 0 {
		t.Fatal("budget ran for an idle worker")
	}
}

func TestRunPostsOpenEscalationsAgain(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"), overseen("s2", "work:OVERSIRE-13"), overseen("s3", "work:OVERSIRE-14"))
	ctx := context.Background()
	esc := &Escalation{ID: "esc-77", Question: "Which db?", Reason: ReasonOverseer, At: t0}
	for _, rec := range []Record{
		{Session: "s1", Key: "work:OVERSIRE-12", State: StateWorking, StartedAt: t0, Budget: DefaultBudget, Escalation: esc},
		{Session: "s2", Key: "work:OVERSIRE-13", State: StateWorking, StartedAt: t0, Budget: DefaultBudget, Escalation: &Escalation{Reason: ReasonFallback, At: t0}},
		{Session: "s3", Key: "work:OVERSIRE-14", State: StateStopped, Escalation: esc},
	} {
		if err := r.store.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	r.e.tick(ctx)
	calls := r.watch.calls()
	if len(calls) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	byID := map[string]*wire.RunnerQuestion{}
	for _, c := range calls {
		byID[c.Session] = c.Q
	}
	if q := byID["s1"]; q == nil || q.ID != "esc-77" || q.Items[0].Text != "Which db?" || !q.AskedAt.Equal(t0) {
		t.Fatalf("s1 %+v", byID["s1"])
	}
	if q, ok := byID["s2"]; !ok || q != nil {
		t.Fatalf("s2 %+v", byID)
	}
	// The question is open now. Ticks keep the escalation and post nothing.
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput, Question: &wire.RunnerQuestion{ID: "esc-77"}}})
	r.e.tick(ctx)
	r.e.tick(ctx)
	if rec, _ := r.store.Load("s1"); rec.Escalation == nil || len(r.watch.calls()) != 2 {
		t.Fatalf("%+v %+v", rec, r.watch.calls())
	}
	if len(r.got()) != 0 {
		t.Fatalf("a repost wrote a change: %+v", r.got())
	}
}

func TestRunRepostsWhenTheSessionComesLater(t *testing.T) {
	r := newRig(t, nil)
	if err := r.store.Save(Record{Session: "s1", Key: "work:OVERSIRE-12", State: StateWorking, StartedAt: t0, Escalation: &Escalation{ID: "esc-1", Question: "Q", Reason: ReasonOverseer}}); err != nil {
		t.Fatal(err)
	}
	r.e.tick(context.Background())
	if len(r.watch.calls()) != 0 {
		t.Fatal("posted for a missing session")
	}
	r.book.byID["s1"] = overseen("s1", "work:OVERSIRE-12")
	r.e.tick(context.Background())
	if len(r.watch.calls()) != 1 {
		t.Fatalf("calls %+v", r.watch.calls())
	}
}

func TestRunTicksUntilCancelled(t *testing.T) {
	r := startedRig(t)
	r.e.Tick = 5 * time.Millisecond
	r.clock.Advance(DefaultBudget)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.e.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for len(r.watch.calls()) == 0 {
		select {
		case <-deadline:
			t.Fatal("no escalation")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
