//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	vorca "github.com/trebi-ai/agent-visor/backend/orca"
)

// claudeHook is a hook payload of claude in dir.
func claudeHook(event, dir string) []byte {
	return fmt.Appendf(nil, `{"hook_event_name":%q,"session_id":"c1","cwd":%q}`, event, dir)
}

func spooled(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "hooks", "spool"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestVisorForward(t *testing.T) {
	if visorHook(func(string) string { return "" }, "") != nil {
		t.Fatal("a forward outside a visor session")
	}
	home, err := os.MkdirTemp("", "ovf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	sock := filepath.Join(home, "visor.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, err := agentvisor.New(ctx, agentvisor.Options{Home: home, Client: "oversire-test", Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close(context.Background())
	ln, err := agentvisor.Listen(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = agentvisor.ServeListener(ctx, v, ln) }()

	dir := t.TempDir()
	s, err := v.Hook(ctx, agentvisor.HookCall{Harness: "claude", Payload: claudeHook("SessionStart", dir)})
	if err != nil || s.ID == "" {
		t.Fatalf("observed session: %+v %v", s, err)
	}
	env := map[string]string{agentvisor.EnvSession: s.ID, agentvisor.EnvSocket: sock}
	fwd := visorHook(func(k string) string { return env[k] }, "")
	fwd.Forward(ctx, "claude", claudeHook("UserPromptSubmit", dir))
	got, err := v.Session(ctx, s.ID)
	if err != nil || got.State != agentvisor.StateRunning {
		t.Fatalf("after the forward: %+v %v", got, err)
	}
	all, _ := v.Sessions(ctx, agentvisor.Filter{})
	if len(all) != 1 {
		t.Fatalf("%d sessions", len(all))
	}

	// The visor refuses a call for an unknown session. It does not go to the spool.
	env[agentvisor.EnvSession] = "ses-none"
	visorHook(func(k string) string { return env[k] }, "").Forward(ctx, "claude", claudeHook("Stop", dir))
	if n := spooled(t, home); n != 0 {
		t.Fatalf("%d spooled after a refusal", n)
	}

	// An Orca pane outside a visor session goes to the visor of home with its labels.
	pane := map[string]string{"ORCA_PANE_KEY": "t1:l1"}
	if visorHook(func(k string) string { return pane[k] }, "") != nil {
		t.Fatal("an Orca pane forward with no visor home")
	}
	visorHook(func(k string) string { return pane[k] }, home).Forward(ctx, "claude", claudeHook("SessionStart", t.TempDir()))
	byPane, _ := v.Sessions(ctx, agentvisor.Filter{Labels: map[string]string{vorca.LabelPane: "t1:l1"}}) //nolint:errcheck // the count check covers it
	if len(byPane) != 1 {
		t.Fatalf("%d sessions of the pane", len(byPane))
	}

	// A visor that does not answer gets the call in its spool.
	cancel()
	_ = v.Close(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); os.IsNotExist(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	visorHook(func(k string) string { return env[k] }, "").Forward(context.Background(), "claude", claudeHook("Stop", dir))
	if n := spooled(t, home); n != 1 {
		t.Fatalf("%d spooled with no visor", n)
	}
	// A call with only labels does not go to the spool.
	visorHook(func(k string) string { return pane[k] }, home).Forward(context.Background(), "claude", claudeHook("Stop", dir))
	if n := spooled(t, home); n != 1 {
		t.Fatalf("%d spooled after an Orca pane call", n)
	}
}
