package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// overseen is a live worker session with an overseer.
func overseen(id, key string) wire.DaemonSession {
	s := live(id, key)
	s.Overseer = "ov1"
	return s
}

func startReq() StartRequest {
	return StartRequest{Key: "oversire-12", Harness: "codex", Model: "gpt-5", Dir: "/work/ws/.wt/oversire-12", Plan: "Build it.", Overseer: "ov1"}
}

func wantKind(t *testing.T, err error, k Kind) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != k {
		t.Fatalf("want %s, got %v", k, err)
	}
}

func TestStartLaunchesAKeyedInPlaceSession(t *testing.T) {
	r := newRig(t, nil)
	rec, reused, err := r.e.Start(context.Background(), startReq())
	if err != nil || reused {
		t.Fatalf("%v reused %v", err, reused)
	}
	if len(r.orders.launches) != 1 {
		t.Fatalf("launches %v", r.orders.launches)
	}
	l := r.orders.launches[0]
	if l.Key != "work:OVERSIRE-12" || l.Item == nil || l.Item.Key != "OVERSIRE-12" || !l.InPlace || l.Repo != "/work/ws/.wt/oversire-12" ||
		l.Overseer != "ov1" || l.Agent != "codex" || l.Model != "gpt-5" {
		t.Fatalf("request %+v", l)
	}
	if !strings.HasPrefix(l.Prompt, "Build it.") || !strings.Contains(l.Prompt, "oversire worker done --verdict done|blocked") || !strings.Contains(l.Prompt, "Do not push.") {
		t.Fatalf("prompt %q", l.Prompt)
	}
	if rec.Session != "s1" || rec.Key != "work:OVERSIRE-12" || rec.State != StateWorking || rec.Budget != DefaultBudget || !rec.StartedAt.Equal(t0) || rec.Overseer != "ov1" {
		t.Fatalf("record %+v", rec)
	}
	if saved, _ := r.store.Load("s1"); saved.Plan != "Build it." || saved.Dir != "/work/ws/.wt/oversire-12" {
		t.Fatalf("saved %+v", saved)
	}
	got := r.got()
	if len(got) != 1 || got[0].Kind != wire.KindWorkerStarted || got[0].Actor != ActorOverseer || got[0].Session != "ov1" ||
		got[0].Payload["harness"] != "codex" || got[0].Payload["reused"] != false {
		t.Fatalf("changes %+v", got)
	}
}

func TestStartRefusesBadInput(t *testing.T) {
	r := newRig(t, nil)
	for _, mod := range []func(*StartRequest){
		func(s *StartRequest) { s.Key = "" }, func(s *StartRequest) { s.Harness = "" }, func(s *StartRequest) { s.Overseer = "" },
		func(s *StartRequest) { s.Dir = "rel" }, func(s *StartRequest) { s.Budget = -1 },
	} {
		req := startReq()
		mod(&req)
		_, _, err := r.e.Start(context.Background(), req)
		wantKind(t, err, Invalid)
	}
	if len(r.orders.launches) != 0 {
		t.Fatal("a bad request launched")
	}
}

func TestSecondStartGivesTheLiveSession(t *testing.T) {
	r := newRig(t, nil)
	first, _, err := r.e.Start(context.Background(), startReq())
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	r.orders.started.Reused = true
	req := startReq()
	req.Overseer, req.Budget = "ov2", 9*time.Hour
	rec, reused, err := r.e.Start(context.Background(), req)
	if err != nil || !reused {
		t.Fatalf("%v reused %v", err, reused)
	}
	if rec.Overseer != "ov2" || !rec.StartedAt.Equal(first.StartedAt) || rec.Budget != DefaultBudget || !rec.LastAction.Equal(t0.Add(time.Hour)) {
		t.Fatalf("record %+v", rec)
	}
	if all, _ := r.store.List(); len(all) != 1 {
		t.Fatalf("records %+v", all)
	}
	if got := r.got(); len(got) != 2 || got[1].Payload["reused"] != true || got[1].Session != "ov2" {
		t.Fatalf("changes %+v", got)
	}
}

func TestStartMapsARuntimeRefusal(t *testing.T) {
	r := newRig(t, nil)
	r.orders.err = runtime.ErrConflict
	_, _, err := r.e.Start(context.Background(), startReq())
	wantKind(t, err, Conflict)
	if len(r.got()) != 0 {
		t.Fatal("a failed start wrote a change")
	}
}

func TestEachOrderHasANewID(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	if err := r.e.Send(ctx, "s1", "one"); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Send(ctx, "s1", "two"); err != nil {
		t.Fatal(err)
	}
	if len(r.orders.sends) != 2 || r.orders.sends[0].Order == r.orders.sends[1].Order || !strings.HasPrefix(r.orders.sends[0].Order, "worker-send-") {
		t.Fatalf("%+v", r.orders.sends)
	}
}

func TestDoneSetsAwaitingCheck(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	r.e.Poll = 5 * time.Millisecond
	rec, err := r.e.Done(context.Background(), "OVERSIRE-12", "done", "Tests pass.")
	if err != nil || rec.State != StateAwaitingCheck || rec.Verdict != "done" || rec.Note != "Tests pass." {
		t.Fatalf("%+v %v", rec, err)
	}
	got := r.got()
	if len(got) != 1 || got[0].Kind != wire.KindWorkerDone || got[0].Actor != ActorWorker || got[0].Session != "s1" || got[0].Payload["verdict"] != "done" {
		t.Fatalf("changes %+v", got)
	}
	w, err := r.e.Wait(context.Background(), "s1", time.Second)
	if err != nil || w.Reason != WakeDone || w.Header.Verdict != "done" || w.Header.Note != "Tests pass." {
		t.Fatalf("%+v %v", w, err)
	}
	blocked, err := r.e.Done(context.Background(), "s1", "blocked", strings.Repeat("x", 400))
	if err != nil || blocked.State != StateBlocked || len([]rune(blocked.Note)) != maxDoneNote {
		t.Fatalf("%+v %v", blocked, err)
	}
	if w, err = r.e.Wait(context.Background(), "s1", time.Second); err != nil || w.Reason != WakeDone {
		t.Fatalf("blocked: %+v %v", w, err)
	}
}

func TestDoneRefusesABadVerdict(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	_, err := r.e.Done(context.Background(), "s1", "pass", "")
	wantKind(t, err, Invalid)
	_, err = r.e.Done(context.Background(), "nope", "done", "")
	wantKind(t, err, NotFound)
}

func TestSendMarksOriginOverseer(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	if _, err := r.e.Done(ctx, "s1", "done", ""); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Minute)
	if err := r.e.Send(ctx, "OVERSIRE-12", "Fix the tests."); err != nil {
		t.Fatal(err)
	}
	s := r.orders.sends[0]
	if s.Rt != wire.RuntimeOversire || s.P.Text != "Fix the tests." || s.P.Origin != wire.OriginOverseer || s.P.Session.ID != "s1" {
		t.Fatalf("%+v", s)
	}
	rec, _ := r.store.Load("s1")
	if rec.State != StateWorking || !rec.LastAction.Equal(t0.Add(time.Minute)) {
		t.Fatalf("record %+v", rec)
	}
	last := r.got()[1]
	if last.Kind != wire.KindWorkerNudged || last.Payload["how"] != "send" || last.Actor != ActorOverseer || last.Session != "ov1" {
		t.Fatalf("change %+v", last)
	}
	wantKind(t, r.e.Send(ctx, "s1", " "), Invalid)
}

func TestAnswerSendsTheTextAsOther(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput,
		Question: &wire.RunnerQuestion{ID: "q-1", Items: []wire.RunnerQuestionItem{{ID: "i1", Text: "Which store?"}}}}})
	if err := r.e.Answer(ctx, "s1", Answer{Question: "q-1", Text: "Use sqlite."}); err != nil {
		t.Fatal(err)
	}
	a := r.orders.sends[0].P.Answer
	if a == nil || a.ID != "q-1" || len(a.Items) != 1 || a.Items[0].ID != "i1" || a.Items[0].Other != "Use sqlite." {
		t.Fatalf("%+v", r.orders.sends[0].P)
	}
	wantKind(t, r.e.Answer(ctx, "s1", Answer{Question: "q-9", Text: "x"}), Conflict)
	wantKind(t, r.e.Answer(ctx, "s1", Answer{Question: "esc-1", Text: "x"}), Conflict)
	wantKind(t, r.e.Answer(ctx, "s1", Answer{Question: "q-1"}), Invalid)

	yes := true
	if err := r.e.Answer(ctx, "s1", Answer{Permission: "p-1", Allow: &yes}); err != nil {
		t.Fatal(err)
	}
	p := r.orders.sends[1].P.Permission
	if p == nil || p.ID != "p-1" || !p.Allow {
		t.Fatalf("%+v", r.orders.sends[1].P)
	}
	wantKind(t, r.e.Answer(ctx, "s1", Answer{Permission: "p-1"}), Invalid)
	wantKind(t, r.e.Answer(ctx, "s1", Answer{}), Invalid)
	got := r.got()
	if len(got) != 2 || got[0].Kind != wire.KindWorkerAnswered || got[0].Payload["request"] != "question" || got[1].Payload["allow"] != true {
		t.Fatalf("changes %+v", got)
	}
}

func TestInterruptCallsTheChatSource(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	if err := r.e.Interrupt(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	got := r.got()
	if r.ints.Load() != 1 || len(got) != 1 || got[0].Kind != wire.KindWorkerNudged || got[0].Payload["how"] != "interrupt" {
		t.Fatalf("%d %+v", r.ints.Load(), got)
	}
}

func TestStopEndsTheSessionAndTheRecord(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, _, err := r.e.Start(ctx, startReq()); err != nil {
		t.Fatal(err)
	}
	r.book.byID["s1"] = overseen("s1", "work:OVERSIRE-12")
	if err := r.e.Escalate(ctx, "s1", "Which db?", ReasonOverseer); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Stop(ctx, "OVERSIRE-12"); err != nil {
		t.Fatal(err)
	}
	rec, _ := r.store.Load("s1")
	if len(r.orders.stops) != 1 || r.orders.stops[0].ID != "s1" || rec.State != StateStopped || rec.Escalation != nil {
		t.Fatalf("%+v %+v", r.orders.stops, rec)
	}
}

func TestWaitUsesInspectForAnOverseenSession(t *testing.T) {
	r := newRig(t, turnsTo(2), overseen("s1", "work:OVERSIRE-12"))
	r.e.Poll = 5 * time.Millisecond
	q := func(id string) runtime.Inspection {
		return runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput,
			Question: &wire.RunnerQuestion{ID: id, Items: []wire.RunnerQuestionItem{{ID: "i", Text: "Which store?"}}}}}
	}
	ctx := context.Background()
	r.watch.set("s1", q("q-1"))
	w, err := r.e.Wait(ctx, "s1", time.Second)
	if err != nil || w.Reason != WakeQuestion || w.Header.Request != "question Which store?" {
		t.Fatalf("%+v %v", w, err)
	}
	r.watch.set("s1", q("esc-5"))
	if w, err = r.e.Wait(ctx, "s1", 50*time.Millisecond); err != nil || w.Reason != WakeNone {
		t.Fatalf("esc: %+v %v", w, err)
	}
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput}})
	if w, _ = r.e.Wait(ctx, "s1", time.Second); w.Reason != WakeInput {
		t.Fatalf("turn end: %+v", w)
	}
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingPermission, Permission: &wire.PermissionPrompt{ID: "p", Tool: "Bash", Text: "rm x"}}})
	if w, _ = r.e.Wait(ctx, "s1", time.Second); w.Reason != WakePermission || w.Header.Request != "permission Bash rm x" {
		t.Fatalf("permission: %+v", w)
	}
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionFailed}})
	if w, _ = r.e.Wait(ctx, "s1", time.Second); w.Reason != WakeFailed {
		t.Fatalf("failed: %+v", w)
	}
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionDone}})
	if w, _ = r.e.Wait(ctx, "s1", time.Second); w.Reason != WakeDone {
		t.Fatalf("done: %+v", w)
	}
}

func TestHeaderTakesTokensAndTheRequestFromInspect(t *testing.T) {
	r := newRig(t, turnsTo(2), overseen("s1", "work:OVERSIRE-12"))
	r.watch.set("s1", runtime.Inspection{Tokens: 4200, Session: wire.RunnerSession{State: wire.SessionWaitingPermission,
		Permission: &wire.PermissionPrompt{ID: "p", Tool: "Bash", Text: "npm test"}}})
	got, err := r.e.Read(context.Background(), "s1", "overseer", 0)
	if err != nil || got.Header.Tokens != 4200 || got.Header.Request != "permission Bash npm test" {
		t.Fatalf("%+v %v", got.Header, err)
	}
	r.watch.err = runtime.ErrUnsupported
	if got, err = r.e.Read(context.Background(), "s1", "overseer", 0); err != nil || got.Header.Tokens != 0 {
		t.Fatalf("no oversight: %+v %v", got.Header, err)
	}
}

func TestEscalateRefusesASecondOpenEscalation(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	if err := r.e.Escalate(ctx, "s1", "Which db?", ReasonOverseer); err != nil {
		t.Fatal(err)
	}
	wantKind(t, r.e.Escalate(ctx, "s1", "Another?", ReasonOverseer), Conflict)
	wantKind(t, r.e.Escalate(ctx, "s1", "", ReasonOverseer), Conflict)
	calls := r.watch.calls()
	if len(calls) != 1 || calls[0].Q == nil || !strings.HasPrefix(calls[0].Q.ID, "esc-") || calls[0].Q.Items[0].Text != "Which db?" || calls[0].Q.Items[0].Kind != wire.QuestionFree || !calls[0].Q.Items[0].AllowsOther {
		t.Fatalf("calls %+v", calls)
	}
	got := r.got()
	if len(got) != 1 || got[0].Kind != wire.KindWorkerEscalated || got[0].Actor != ActorOverseer || got[0].Payload["reason"] != "overseer" || got[0].Payload["question"] != "Which db?" {
		t.Fatalf("changes %+v", got)
	}
}

func TestEscalateWithNoQuestionPublishesTheOpenAsk(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	if err := r.e.Escalate(context.Background(), "s1", "", ReasonOverseer); err != nil {
		t.Fatal(err)
	}
	if calls := r.watch.calls(); len(calls) != 1 || calls[0].Q != nil {
		t.Fatalf("calls %+v", calls)
	}
	if rec, _ := r.store.Load("s1"); rec.Escalation == nil || rec.Escalation.ID != "" || rec.Escalation.Reason != ReasonOverseer {
		t.Fatalf("record %+v", rec)
	}
}

func TestEscalateMapsTheRuntimeRefusal(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	r.watch.escEr = runtime.ErrConflict
	wantKind(t, r.e.Escalate(context.Background(), "s1", "Q?", ReasonOverseer), Conflict)
	if rec, _ := r.store.Load("s1"); rec.Escalation != nil || len(r.got()) != 0 {
		t.Fatalf("a refused escalation left a trace: %+v", rec)
	}
	wantKind(t, r.e.Escalate(context.Background(), "s1", "Q?", "other"), Invalid)
}
