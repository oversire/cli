package orca

import (
	"context"
	"slices"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	vorca "github.com/trebi-ai/agent-visor/backend/orca"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// fakeHost is the visor with one orca session for each start.
type fakeHost struct {
	starts   []agentvisor.StartSpec
	sessions []agentvisor.Session
	sent     []string
	stopped  []string
}

func (h *fakeHost) Start(_ context.Context, spec agentvisor.StartSpec) (agentvisor.Session, error) {
	h.starts = append(h.starts, spec)
	s := agentvisor.Session{ID: "ses_1", Kind: spec.Kind, State: agentvisor.StateRunning,
		Labels: map[string]string{vorca.LabelTerminal: "h9", vorca.LabelPane: "t9:l9"}}
	h.sessions = append(h.sessions, s)
	return s, nil
}

func (h *fakeHost) Send(_ context.Context, id string, in agentvisor.Input) (agentvisor.Delivery, error) {
	h.sent = append(h.sent, id+" "+in.Text)
	return agentvisor.DeliveryPrompted, nil
}

func (h *fakeHost) Stop(_ context.Context, id string) error {
	h.stopped = append(h.stopped, id)
	return nil
}

func (h *fakeHost) Sessions(_ context.Context, f agentvisor.Filter) ([]agentvisor.Session, error) {
	var out []agentvisor.Session
	for _, s := range h.sessions {
		if s.Kind == f.Kind && s.Labels[vorca.LabelTerminal] == f.Labels[vorca.LabelTerminal] {
			out = append(out, s)
		}
	}
	return out, nil
}

type fakeBackend struct{ snap vorca.Snapshot }

func (b *fakeBackend) Snapshot() vorca.Snapshot { return b.snap }

func (b *fakeBackend) Binding(string) (pane, terminal, worktree string, ok bool) {
	return "t9:l9", "h9", "c1", true
}

func TestLaunchInTheVisor(t *testing.T) {
	o, r := setup()
	h := &fakeHost{}
	o.WithVisor(h, &fakeBackend{})
	r.on("repo list", `{"ok":true,"result":{"repos":[{"id":"r1","path":"/src/app"}]}}`)
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[]}}`)
	r.on("worktree create", `{"ok":true,"result":{"worktree":{"id":"r1::/src/app-1","path":"/src/app-1"}}}`)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[{"handle":"sh1","worktreeId":"r1::/src/app-1","tabId":"t1","leafId":"l1"}]}}`)

	item := &wire.ItemRef{Key: "eng-4", URL: "https://x/ENG-4"}
	st, err := o.Launch(context.Background(), runtime.Order{}, wire.LaunchRequest{Repo: "/src/app", Item: item, Agent: "claude", Prompt: "go", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.ID != "h9" || st.Session.Workspace.ID != "c1" || st.PaneKey != "t9:l9" || !st.TurnStarted {
		t.Fatalf("started %#v", st)
	}
	spec := h.starts[0]
	if spec.Kind != vorca.Kind || spec.Harness != "claude" || spec.Prompt != "go" || spec.WorkingDir != "/src/app-1" {
		t.Fatalf("spec %#v", spec)
	}
	if spec.Labels[vorca.LabelWorktree] != "r1::/src/app-1" || spec.Labels[vorca.LabelShell] != "sh1" {
		t.Fatalf("labels %v", spec.Labels)
	}
	if !slices.Equal(spec.Command, []string{"sh", "-c", "claude -n 'eng-4' --model opus"}) {
		t.Fatalf("command %q", spec.Command)
	}
	create := r.args("worktree create")[0]
	if !slices.Contains(create, "--comment") {
		t.Fatalf("create %q", create)
	}
	noEnvironment(t, r)
}

func TestVisorSessionIO(t *testing.T) {
	o, r := setup()
	h := &fakeHost{sessions: []agentvisor.Session{{ID: "ses_1", Kind: vorca.Kind, State: agentvisor.StateIdle, Labels: map[string]string{vorca.LabelTerminal: "h9"}}}}
	o.WithVisor(h, &fakeBackend{})
	ctx := context.Background()
	in := wire.SessionRef{ID: "h9"}
	if err := o.Send(ctx, wire.SendPayload{Session: in, Text: "next"}); err != nil {
		t.Fatal(err)
	}
	if err := o.Stop(ctx, in); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.sent, []string{"ses_1 next"}) || !slices.Equal(h.stopped, []string{"ses_1"}) {
		t.Fatalf("sent %q, stopped %q", h.sent, h.stopped)
	}
	// A terminal that the visor does not follow goes to the Orca CLI.
	r.on("terminal close", `{"ok":true,"result":{}}`)
	if err := o.Stop(ctx, wire.SessionRef{ID: "h1"}); err != nil || len(r.args("terminal close")) != 1 {
		t.Fatalf("stop outside the visor: %v %q", err, r.calls)
	}
}

func TestWorktreesFromTheSnapshot(t *testing.T) {
	o, r := setup()
	now := time.Now()
	o.now = func() time.Time { return now }
	b := &fakeBackend{snap: vorca.Snapshot{Worktrees: []WorktreeCard{{ID: "w1"}}, Terminals: []Terminal{{Handle: "h1"}}, At: now.Add(-time.Second)}}
	o.WithVisor(&fakeHost{}, b)
	wts, err := o.Worktrees(context.Background())
	terms, _ := o.Terminals(context.Background()) //nolint:errcheck // the snapshot has no error
	if err != nil || len(wts) != 1 || len(terms) != 1 || len(r.calls) != 0 {
		t.Fatalf("fresh snapshot: %v %v %v %q", err, wts, terms, r.calls)
	}
	b.snap.At = now.Add(-time.Minute)
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[{"worktreeId":"w2"}]}}`)
	if wts, err := o.Worktrees(context.Background()); err != nil || wts[0].ID != "w2" {
		t.Fatalf("old snapshot: %v %v", err, wts)
	}
}
