package oversire

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// gated runs the first part of each turn, then waits for the file $GATE.
const gated = `while read -r line; do
  case "$line" in
    *'"type":"user"'*)
      echo '{"type":"init","session_id":"v1"}'
      echo '{"type":"assistant","text":"step1"}'
      while [ ! -f "$GATE" ]; do sleep 0.05; done
      echo '{"type":"assistant","text":"step2"}'
      echo '{"type":"result","subtype":"success","session_id":"v1"}';;
  esac
done`

// heldVisor runs the fake harness in a holder process, so a session lives through a restart of the visor.
func heldVisor(t *testing.T, home, gate string) *agentvisor.Visor {
	t.Helper()
	v, err := agentvisor.New(context.Background(), agentvisor.Options{
		Home: home, Client: "oversire-test",
		Env: map[string]string{agentwire.FakeScriptEnv: gated, "GATE": gate},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRestartDuringTurn restarts the daemon (the runtime and its visor) during a turn. The session goes on,
// its state stays right, and it is not "working" after the turn ends.
func TestRestartDuringTurn(t *testing.T) {
	home, data := shortHome(t), t.TempDir()
	gate := filepath.Join(home, "gate")
	repo := gitRepo(t)
	ctx := context.Background()
	item := &wire.ItemRef{Project: wire.ProjectRef{Account: wire.AccountID{Provider: "plane", ID: "ocral"}, ID: "p", Key: "APP"}, ID: "9a", Key: "APP-5", URL: "https://x/APP-5"}

	v := heldVisor(t, home, gate)
	rec := &recorder{}
	rt := New(Options{Runner: "r1", DataDir: data, Fake: true}, v, proc.Local{}, rec)
	if err := rt.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	ref, err := rt.Start(ctx, runtime.Order{ID: "o1"}, wire.LaunchRequest{Repo: repo, Item: item, Agent: "fake", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, ref.ID, wire.SessionWorking)
	waitFor(t, func() bool {
		lines, _ := rt.Read(ctx, ref, 10)
		return slices.Contains(lines, "step1")
	})
	rt.Close(ctx)
	if err := v.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The daemon starts again during the turn.
	v = heldVisor(t, home, gate)
	t.Cleanup(func() { _ = v.Close(context.Background()) })
	rec = &recorder{}
	rt = New(Options{Runner: "r1", DataDir: data, Fake: true}, v, proc.Local{}, rec)
	if err := rt.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	s, ok := rec.latest(ref.ID)
	if !ok || s.State != wire.SessionWorking || s.Order != "o1" || s.Item == nil || s.Item.Key != "APP-5" || s.Session != ref {
		t.Fatalf("after the restart %#v", s)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s = rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Summary != "step2" || s.Error != "" {
		t.Fatalf("after the turn %#v", s)
	}
	lines, _ := rt.Read(ctx, ref, 10)
	if !slices.Equal(lines, []string{"> go", "step1", "step2"}) {
		t.Fatalf("lines %q", lines)
	}

	// The session resumes for the next turn in a new holder.
	if err := rt.Send(ctx, wire.SendPayload{Session: ref, Text: "more"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"> go", "step1", "step2", "> more", "step1", "step2"}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		lines, _ := rt.Read(ctx, ref, 10)
		if slices.Equal(lines, want) {
			break
		}
		if time.Now().After(deadline) {
			s, _ := rec.latest(ref.ID)
			t.Fatalf("lines %q, state %s", lines, s.State)
		}
	}
	rec.wait(t, ref.ID, wire.SessionWaitingInput)
	stop, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rt.Stop(stop, ref); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, ref.ID, wire.SessionDone)
}
