package oversire

import (
	"cmp"
	"context"
	"os"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/runner/proc"
	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/wire"
)

// TestLive runs a real harness. Set OVERSIRE_LIVE=1, and OVERSIRE_LIVE_AGENT for an agent other than claude.
func TestLive(t *testing.T) {
	if os.Getenv("OVERSIRE_LIVE") != "1" {
		t.Skip("set OVERSIRE_LIVE=1 to run a real harness")
	}
	agent := cmp.Or(os.Getenv("OVERSIRE_LIVE_AGENT"), "claude")
	repo := gitRepo(t)
	aw := agentwire.New(agentwire.Options{ClientName: "oversire-runner-live"})
	rec := &recorder{}
	rt := New(Options{Runner: "live", DataDir: t.TempDir()}, aw, proc.Local{}, rec)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer aw.Close(ctx)
	defer rt.Close(ctx)

	ref, err := rt.Start(ctx, runtime.Order{ID: "live"}, wire.LaunchRequest{Repo: repo, Name: "LIVE-1", Agent: agent, Prompt: "Answer with the one word hello. Do not use tools."})
	if err != nil {
		t.Fatal(err)
	}
	for {
		s, _ := rec.latest(ref.ID)
		if s.State == wire.SessionWaitingInput {
			t.Logf("summary %q", s.Summary)
			break
		}
		if s.State.Ended() {
			t.Fatalf("session ended: %s", s.Error)
		}
		if ctx.Err() != nil {
			t.Fatal("no answer in time")
		}
		time.Sleep(200 * time.Millisecond)
	}
	lines, err := rt.Read(ctx, ref, 20)
	if err != nil || len(lines) < 2 {
		t.Fatalf("read %q %v", lines, err)
	}
	if err := rt.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
}
