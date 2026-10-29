package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/runbook"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// bookLauncher is a runtime that starts each session in the book at once, with the start key, as orders.Runner does.
type bookLauncher struct {
	book *sessions.Book
	mu   sync.Mutex
	reqs []wire.LaunchRequest
}

func (l *bookLauncher) Launch(_ context.Context, rt string, _ runtime.Order, r wire.LaunchRequest) (runtime.Started, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, r)
	id := fmt.Sprintf("s%d", len(l.reqs))
	l.mu.Unlock()
	working := wire.PhaseWorking
	l.book.Apply(sessions.Patch{ID: id, Source: sessions.SourceWire, Runtime: &rt, Key: &r.Key, Prompt: &r.Prompt, State: &working})
	return runtime.Started{Session: wire.SessionRef{ID: id}, PaneKey: id, TurnStarted: true}, nil
}

func (l *bookLauncher) Stop(context.Context, string, runtime.Order, wire.SessionRef) error {
	return nil
}

type onePicker struct{}

func (onePicker) Pick(context.Context, string) (string, error) { return wire.RuntimeOversire, nil }

// stepDaemon runs a real server with the runbook engine on a free loopback port, and points the config of c at it.
func stepDaemon(t *testing.T, c *cli) (base string, frames <-chan wire.RunbookRun) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	off := false
	cfg := config.ServeConfig{Port: port, Tailscale: &off}
	if err := c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &cfg}); err != nil {
		t.Fatal(err)
	}
	events := eventlog.New(10)
	book := sessions.NewBook("mbp", "", events, nil)
	info := func() wire.DaemonInfo {
		return wire.DaemonInfo{Machine: "mbp", Version: "test", Contract: wire.DaemonContract}
	}
	srv := server.NewServer(cfg, info, nil, events, book, nil)
	srv.Log = c.log
	engine := runbook.NewEngine(runbook.NewStore(filepath.Join(t.TempDir(), "runbooks")), &bookLauncher{book: book}, onePicker{}, book, "mbp",
		func(r wire.RunbookRun) { srv.Notify(false, map[string]any{"type": "runbookRun", "run": r}) }, nil)
	engine.Log = c.log
	srv.Handle(runbook.NewRoutes(engine).Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	go engine.Run(ctx)
	t.Cleanup(func() { cancel(); <-done })
	base = "http://127.0.0.1:" + strconv.Itoa(port)

	var conn *websocket.Conn
	for i := 0; ; i++ {
		dctx, dcancel := context.WithTimeout(ctx, time.Second)
		conn, _, err = websocket.Dial(dctx, "ws://127.0.0.1:"+strconv.Itoa(port)+"/v1/connect", nil)
		dcancel()
		if err == nil {
			break
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := make(chan wire.RunbookRun, 64)
	go func() {
		defer close(out)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f struct {
				Type string          `json:"type"`
				Run  wire.RunbookRun `json:"run"`
			}
			if json.Unmarshal(data, &f) == nil && f.Type == "runbookRun" {
				// Only the keys "run" and "type" are in the frame.
				var keys map[string]json.RawMessage
				json.Unmarshal(data, &keys)
				if len(keys) != 2 {
					t.Errorf("frame %s", data)
				}
				out <- f.Run
			}
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return base, out
}

// await reads frames until f is true for one.
func await(t *testing.T, frames <-chan wire.RunbookRun, f func(wire.RunbookRun) bool) wire.RunbookRun {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case r, ok := <-frames:
			if !ok {
				t.Fatal("the socket closed")
			}
			if f(r) {
				return r
			}
		case <-timeout:
			t.Fatal("no frame")
		}
	}
}

func working(step string) func(wire.RunbookRun) bool {
	return func(r wire.RunbookRun) bool {
		return r.Current == step && len(r.Steps) > 0 && r.Steps[len(r.Steps)-1].State == wire.StepWorking
	}
}

// TestStepDoneRunsATwoStepRunbook is the check of OVERSIRE-352: a test daemon runs a two-step runbook against a fake runtime.
// The review passes, so its fix step does not run.
func TestStepDoneRunsATwoStepRunbook(t *testing.T) {
	c, out, _ := testCLI(t, "")
	base, frames := stepDaemon(t, c)
	ctx := context.Background()
	d, _ := c.daemonClient()
	start := wire.RunbookStart{
		Item: wire.ItemRef{Project: wire.ProjectRef{Account: wire.AccountID{Provider: "plane", ID: "ocral"}, ID: "p1", Key: "OVERSIRE"}, ID: "i1", Key: "OVERSIRE-341"},
		Repo: "/r/app", Agents: map[string]wire.AgentLaunch{"claude": {CLI: "claude"}},
		Runbook: wire.Runbook{ID: "two", Name: "Two", Steps: []wire.RunbookStep{
			{ID: "plan", Name: "Plan", Agent: "claude", Prompt: "Plan {key}. Then run {done} --verdict done."},
			{ID: "review", Name: "Review", Agent: "claude", Prompt: "Review {key}. Then run {done} --verdict pass.", Review: &wire.Review{Fix: "fix", Rounds: 1}},
			{ID: "fix", Name: "Fix", Agent: "claude", Prompt: "Fix {key}. Then run {done} --verdict done."},
		}},
	}
	var run wire.RunbookRun
	if err := d.call(ctx, "POST", "/v1/runbook-runs", start, &run); err != nil {
		t.Fatal(err)
	}
	await(t, frames, working("plan"))
	step := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := c.dispatch(ctx, append([]string{"step", "done", "--run", run.ID}, args...)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(out.String())
	}
	if s := step("--step", "plan", "--verdict", "done", "--note", strings.Repeat("x", 400)); s != "Step 1 of 3 Plan: done. Next: Review." {
		t.Fatal(s)
	}
	r := await(t, frames, working("review"))
	if len([]rune(r.Steps[0].Note)) != runbook.MaxNote {
		t.Fatal("the note is not cut")
	}
	if s := step("--step", "review", "--verdict", "pass"); s != "Step 2 of 3 Review: pass. Next: done." {
		t.Fatal(s)
	}
	r = await(t, frames, func(r wire.RunbookRun) bool { return r.State == wire.RunDone })
	if trail := fmt.Sprintf("%d %s %s", len(r.Steps), r.Steps[2].Step, r.Steps[2].State); trail != "3 fix skipped" {
		t.Fatal(trail)
	}

	err := c.dispatch(ctx, []string{"step", "done", "--run", run.ID, "--step", "plan", "--verdict", "done"})
	if err == nil || err.Error() != "The run is done." {
		t.Fatal(err)
	}
	if err := c.dispatch(ctx, []string{"step", "done", "--run", run.ID, "--step", "plan"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatal(err)
	}
	if err := c.dispatch(ctx, []string{"step", "done", "--run", "rb_00000000", "--step", "plan", "--verdict", "done"}); err == nil || err.Error() != "There is no run rb_00000000." {
		t.Fatal(err)
	}
	resp, err := http.Get(base + "/v1/runbook-runs?item=OVERSIRE-341&open=1")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp)
	}
	var open []wire.RunbookRun
	json.NewDecoder(resp.Body).Decode(&open)
	resp.Body.Close()
	if open == nil || len(open) != 0 {
		t.Fatal(open)
	}
}

func TestStepDoneWithNoDaemon(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}})
	err := c.dispatch(context.Background(), []string{"step", "done", "--run", "rb_1", "--step", "plan", "--verdict", "done"})
	if err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}
