package oversire

import (
	"context"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// TestConformance runs the cases of RuntimeConformance in the app (OverSireTestKit) that apply to
// a runner: launch, send, tail and close, through the registry as the hub orders them.
func TestConformance(t *testing.T) {
	f := setup(t, chat, Options{})
	reg := runtime.NewRegistry(f.rt)
	ctx := context.Background()
	do := func(step string, p wire.Payload) *wire.WorkOrderResult {
		t.Helper()
		res, err := reg.Do(ctx, wire.WorkOrder{ID: "o-" + step, Runtime: ID, Payload: p, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return res
	}

	res := do("launch", wire.LaunchRequest{Repo: f.repo, Name: "CONFORMANCE-1", Agent: "fake", Prompt: "Say hello."})
	if res.Session == nil {
		t.Fatal("launch gave no session")
	}
	s := *res.Session
	if s.Workspace.Account != (wire.AccountID{Provider: ID, ID: Local}) || s.Workspace.ID != "app/CONFORMANCE-1" {
		t.Fatalf("launch gave %#v", s)
	}
	f.rec.wait(t, s.ID, wire.SessionWaitingInput)
	do("send", wire.SendPayload{Session: s, Text: "Conformance input"})
	if lines := do("tail", wire.ReadPayload{Session: s, Lines: 3}).Lines; len(lines) > 3 || len(lines) == 0 {
		t.Fatalf("tail gave %d lines for 3", len(lines))
	}
	do("close", wire.StopPayload{Session: s})
	if got, _ := f.rec.latest(s.ID); got.State != wire.SessionDone {
		t.Fatalf("close left %s", got.State)
	}
}
