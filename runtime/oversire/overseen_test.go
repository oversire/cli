//go:build !windows

package oversire

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// counted ends each turn with a result that has usage.
const counted = `while read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"assistant","text":"ok"}'; echo '{"type":"result","subtype":"success","usage":{"input":10,"output":5}}';;
  esac
done`

func (f fixture) overseen(t *testing.T, name string) wire.SessionRef {
	t.Helper()
	ref, err := f.rt.Start(context.Background(), runtime.Order{ID: "o-" + name}, wire.LaunchRequest{Repo: f.repo, Name: name, Agent: "fake", Prompt: "go", Overseer: "claude-1"})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (f fixture) inspect(t *testing.T, ref wire.SessionRef) runtime.Inspection {
	t.Helper()
	in, err := f.rt.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestSpecAddsTheOverseerLabelAndAuto(t *testing.T) {
	var spy *startSpy
	f := setupWith(t, chat, Options{}, func(v *agentvisor.Visor) Host {
		spy = &startSpy{Visor: v}
		return spy
	})
	ref := f.overseen(t, "APP-20")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	spy.mu.Lock()
	sp := spy.spec
	spy.mu.Unlock()
	if sp.Labels[labelOverseer] != "claude-1" || sp.Permissions.Mode != agentwire.PermissionAuto || sp.PromptKind != string(wire.OriginOverseer) || sp.Raw["sandbox"] != "danger-full-access" {
		t.Fatalf("overseen spec %#v", sp)
	}
	plain := f.launch(t, "APP-21", runtime.Order{ID: "o2"})
	f.rec.wait(t, plain.ID, wire.SessionWaitingInput)
	spy.mu.Lock()
	sp = spy.spec
	spy.mu.Unlock()
	if _, ok := sp.Labels[labelOverseer]; ok || sp.Permissions.Mode != agentwire.PermissionAsk || sp.PromptKind != "" || sp.Raw != nil {
		t.Fatalf("plain spec %#v", sp)
	}
}

func TestAdoptedSessionKeepsTheOverseer(t *testing.T) {
	f := setup(t, chat, Options{})
	ref := f.overseen(t, "APP-22")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if err := f.rt.Stop(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	again := f.again(t, nil)
	s, err := again.session(ref)
	if err != nil || s.overseer != "claude-1" {
		t.Fatalf("adopted session %+v %v", s, err)
	}
}

func TestFirstPromptOfAnOverseenStartIsOverseer(t *testing.T) {
	f := setup(t, chat, Options{})
	ref := f.overseen(t, "APP-23")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	u := userTurns(t, ref.ID, f.rt)
	if len(u) != 1 || u[0].Text != "go" || u[0].Origin != wire.OriginOverseer {
		t.Fatalf("user turns %+v", u)
	}
}

func TestResultUsageAddsTokens(t *testing.T) {
	f := setup(t, counted, Options{})
	ref := f.launch(t, "APP-24", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	waitFor(t, func() bool { return f.inspect(t, ref).Tokens == 15 })
	if err := f.rt.Send(context.Background(), wire.SendPayload{Session: ref, Text: "more"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.inspect(t, ref).Tokens == 30 })
}

func TestInspectGivesTheLastTurnAndAnUnknownSessionIsNotFound(t *testing.T) {
	f := setup(t, chat, Options{})
	ref := f.launch(t, "APP-25", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if in := f.inspect(t, ref); in.LastTurn.IsZero() || !in.AskedAt.IsZero() {
		t.Fatalf("inspection %+v", in)
	}
	_, err := f.rt.Inspect(context.Background(), wire.SessionRef{ID: "s-nope"})
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

func TestPublishHidesTheAskOfAnOverseenSession(t *testing.T) {
	f := setup(t, asks, Options{})
	ref := f.overseen(t, "APP-26")
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingPermission)
	if s.Permission != nil || s.Question != nil {
		t.Fatalf("the report shows the ask: %+v", s)
	}
	in := f.inspect(t, ref)
	if in.Session.Permission == nil || in.Session.Permission.ID != "p-1" || in.AskedAt.IsZero() {
		t.Fatalf("inspection %+v", in)
	}
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	for _, r := range f.rec.all {
		if r.Permission != nil {
			t.Fatalf("a report shows the ask: %+v", r)
		}
	}
}

func TestEscalateNilPublishesTheOpenAsk(t *testing.T) {
	f := setup(t, asks, Options{})
	ref := f.overseen(t, "APP-27")
	f.rec.wait(t, ref.ID, wire.SessionWaitingPermission)
	if err := f.rt.Escalate(context.Background(), ref, nil); err != nil {
		t.Fatal(err)
	}
	s, _ := f.rec.latest(ref.ID)
	if s.Permission == nil || s.Permission.ID != "p-1" {
		t.Fatalf("report %+v", s)
	}
	if err := f.rt.Send(context.Background(), wire.SendPayload{Session: ref, Permission: &wire.PermissionAnswer{ID: "p-1", Allow: true}}); err != nil {
		t.Fatal(err)
	}
	s = f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Permission != nil {
		t.Fatalf("report %+v", s)
	}
}

func escalation(id string) *wire.RunnerQuestion {
	return &wire.RunnerQuestion{ID: id, AskedAt: time.Now().UTC(), Items: []wire.RunnerQuestionItem{
		{ID: "q", Text: "Which database?", Kind: wire.QuestionFree, AllowsOther: true}}}
}

func TestEscalateQuestionSurvivesATurnEnd(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	ref := f.overseen(t, "APP-28")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if err := f.rt.Escalate(ctx, ref, escalation("esc-1")); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.rec.latest(ref.ID); s.Question == nil || s.Question.ID != "esc-1" {
		t.Fatalf("report %+v", s)
	}
	if err := f.rt.Escalate(ctx, ref, escalation("esc-2")); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("a second question: %v", err)
	}
	if err := f.rt.Escalate(ctx, ref, escalation("other")); err == nil {
		t.Fatal("an id with no esc- prefix")
	}
	// The agent writes text and ends a turn.
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Text: "more", Origin: wire.OriginOverseer}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		lines, _ := f.rt.Read(ctx, ref, 10)
		return strings.Count(strings.Join(lines, "\n"), "turn ") == 2
	})
	if s, _ := f.rec.latest(ref.ID); s.Question == nil || s.Question.ID != "esc-1" || s.State != wire.SessionWaitingInput {
		t.Fatalf("report after the turn %+v", s)
	}
	if in := f.inspect(t, ref); in.Session.Question == nil || !in.AskedAt.IsZero() {
		t.Fatalf("inspection %+v", in)
	}
}

func TestEscalateAnswerGoesToTheWorkerAsUserText(t *testing.T) {
	var host *recordingHost
	f := setupWith(t, chat, Options{}, func(v *agentvisor.Visor) Host { host = &recordingHost{Host: v}; return host })
	ctx := context.Background()
	ref := f.overseen(t, "APP-29")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if err := f.rt.Escalate(ctx, ref, escalation("esc-1")); err != nil {
		t.Fatal(err)
	}
	a := wire.QuestionAnswer{ID: "esc-1", Items: []wire.QuestionAnswerItem{{ID: "q", Other: "Use SQLite."}}}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &a}); err != nil {
		t.Fatal(err)
	}
	if got := host.last(); got.Text != "Use SQLite." || got.Kind != string(wire.OriginUser) {
		t.Fatalf("input %+v", got)
	}
	waitFor(t, func() bool { return len(userTurns(t, ref.ID, f.rt)) == 2 })
	if u := userTurns(t, ref.ID, f.rt); u[1].Text != "Use SQLite." || u[1].Origin != wire.OriginUser {
		t.Fatalf("user turns %+v", u)
	}
	if s, _ := f.rec.latest(ref.ID); s.Question != nil {
		t.Fatalf("report %+v", s)
	}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &a}); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a repeat: %v", err)
	}
}

func TestEscalateAnswerNeedsText(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	ref := f.overseen(t, "APP-30")
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if err := f.rt.Escalate(ctx, ref, escalation("esc-1")); err != nil {
		t.Fatal(err)
	}
	a := wire.QuestionAnswer{ID: "esc-1", Items: []wire.QuestionAnswerItem{{ID: "q"}}}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &a}); err == nil {
		t.Fatal("an empty answer")
	}
	if in := f.inspect(t, ref); in.Session.Question == nil {
		t.Fatal("the question closed")
	}
}

func TestOverseenPermissionUsesTheLongTimeout(t *testing.T) {
	f := setup(t, asks, Options{PermissionTimeout: time.Minute, OverseenTimeout: 2 * time.Hour})
	ref := f.overseen(t, "APP-31")
	plain := f.launch(t, "APP-32", runtime.Order{ID: "o2"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingPermission)
	f.rec.wait(t, plain.ID, wire.SessionWaitingPermission)
	for _, c := range []struct {
		ref  wire.SessionRef
		want time.Duration
	}{{ref, 2 * time.Hour}, {plain, time.Minute}} {
		in := f.inspect(t, c.ref)
		if got := in.Session.Permission.ExpiresAt.Sub(in.AskedAt); got != c.want {
			t.Errorf("expiry %s, want %s", got, c.want)
		}
	}
}

func TestNewDefaultsTheOverseenTimeout(t *testing.T) {
	if got := New(Options{}, nil, nil, nil).opts.OverseenTimeout; got != 40*time.Minute {
		t.Fatal(got)
	}
}
