package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/oversire/cli/appcall"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// fakeApp answers the appCall frames of the daemon as the app does: it posts the result to the result route.
type fakeApp struct {
	mu     sync.Mutex
	calls  []wire.AppCall
	answer func(wire.AppCall) wire.AppCallResult
}

func (a *fakeApp) last(t *testing.T) wire.AppCall {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		t.Fatal("no app call")
	}
	return a.calls[len(a.calls)-1]
}

func (a *fakeApp) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func (a *fakeApp) args(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.last(t).Args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// appDaemon runs a real server with the app-call relay on a free loopback port, points the config of c at it,
// and connects a fake app on /v1/connect.
func appDaemon(t *testing.T, c *cli) *fakeApp {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("OVERSIRE_SESSION", "")
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
	info := func() wire.DaemonInfo {
		return wire.DaemonInfo{Machine: "mbp", Version: "test", Contract: wire.DaemonContract}
	}
	srv := server.NewServer(cfg, info, nil, events, sessions.NewBook("mbp", "", events, nil), nil)
	srv.Log = c.log
	relay := appcall.New(srv.Deliver, func() bool { return srv.HasClient(true) }, 2*time.Second)
	relay.Log = c.log
	srv.Handle(relay.Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	base := "http://127.0.0.1:" + strconv.Itoa(port)

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
	t.Cleanup(func() { conn.CloseNow() })
	for i := 0; !srv.HasClient(true); i++ {
		if i == 100 {
			t.Fatal("the daemon has no app")
		}
		time.Sleep(10 * time.Millisecond)
	}
	app := &fakeApp{answer: func(c wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "ok."}
	}}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var f wire.AppCallFrame
			if wire.Decode(data, &f) != nil || f.Type != wire.FrameAppCall {
				continue
			}
			app.mu.Lock()
			app.calls = append(app.calls, f.Call)
			res := app.answer(f.Call)
			app.mu.Unlock()
			b, _ := json.Marshal(res)
			resp, err := http.Post(base+"/v1/app-calls/"+f.Call.ID+"/result", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("result: %d", resp.StatusCode)
			}
		}
	}()
	return app
}

func TestEpicListText(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "2 epics.", Data: json.RawMessage(`[
			{"project":"OVERSIRE","id":"e1","title":"Project Plan tab","state":"active","progress":"6 of 9 done","next":["OVERSIRE-484"],"lateDays":2},
			{"project":"OVERSIRE","id":"e2","title":"Review desk","state":"active","progress":"3 of 3 done","next":[],"readyToClose":true}]`)}
	}
	if err := c.dispatch(context.Background(), []string{"epic", "list", "--project", "oversire"}); err != nil {
		t.Fatal(err)
	}
	want := `2 epics.
PROJECT   EPIC              STATE                   PROGRESS     LATE  NEXT
OVERSIRE  Project Plan tab  active                  6 of 9 done  2d    OVERSIRE-484
OVERSIRE  Review desk       active, ready to close  3 of 3 done  -     -
`
	if out.String() != want {
		t.Fatalf("got\n%s", out)
	}
	if call := app.last(t); call.Action != "epic.list" || string(call.Args) != `{"project":"OVERSIRE"}` || call.Agent != "" {
		t.Fatal(call)
	}
}

func TestEpicPlanJSON(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	data := `{"project":"OVERSIRE","end":"2026-12-04","relations":{"loaded":2,"total":5,"failed":0},"critical":["OVERSIRE-485"],` +
		`"epics":[{"id":"e1","title":"Plan","state":"active","forecast":{"start":"2026-11-19","end":"2026-12-04","lateBy":3},` +
		`"steps":[{"key":"OVERSIRE-485","title":"Relay","state":"ready","start":"2026-11-19","end":"2026-11-20","slack":0,"critical":true}]}]}`
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "Plan: 1 open step.", Data: json.RawMessage(data)}
	}
	err := c.dispatch(context.Background(), []string{"epic", "plan", "Plan", "--project", "OVERSIRE", "--json", "--harness", "codex", "--session", "s9"})
	if err != nil {
		t.Fatal(err)
	}
	var res wire.AppCallResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	var want, got any
	json.Unmarshal([]byte(data), &want)
	json.Unmarshal(res.Data, &got)
	if res.Status != wire.AppCallOK || !jsonEqual(want, got) {
		t.Fatalf("%s", out)
	}
	if call := app.last(t); call.Action != "epic.plan" || call.Agent != "codex s9" || app.args(t)["epic"] != "Plan" {
		t.Fatal(call)
	}

	out.Reset()
	if err := c.dispatch(context.Background(), []string{"epic", "plan", "--project", "OVERSIRE"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Plan (active): 2026-11-19 to 2026-12-04, 3 days late.", "*OVERSIRE-485", "Critical path (*): OVERSIRE-485", "Relations 2 of 5 loaded"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("no %q in\n%s", s, out)
		}
	}
	if _, ok := app.args(t)["epic"]; ok {
		t.Fatal("an epic for the whole plan")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestEpicUpdateNoneClears(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "5b6e")
	if err := c.dispatch(context.Background(), []string{"epic", "update", "Plan tab", "--target", "none", "--state", "paused", "--project", "OVERSIRE"}); err != nil {
		t.Fatal(err)
	}
	a := app.args(t)
	if app.last(t).Action != "epic.update" || a["targetDate"] != "none" || a["state"] != "paused" || a["epic"] != "Plan tab" || len(a) != 4 {
		t.Fatal(a)
	}
	if app.last(t).Agent != "claude 5b6e" || out.String() != "ok.\n" {
		t.Fatal(app.last(t).Agent, out)
	}
	err := c.dispatch(context.Background(), []string{"epic", "update", "Plan tab", "--project", "OVERSIRE"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 2 {
		t.Fatal(err)
	}
}

func TestEpicAddTakesTheProjectOfTheKeys(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"epic", "add", "Plan", "oversire-1", "OVERSIRE-2"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(app.args(t)); string(b) != `{"epic":"Plan","keys":["OVERSIRE-1","OVERSIRE-2"],"project":"OVERSIRE"}` {
		t.Fatal(string(b))
	}
	err := c.dispatch(context.Background(), []string{"epic", "remove", "OVERSIRE-1", "TREBI-2"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 2 {
		t.Fatal(err)
	}
}

func TestEpicRefusalExit1(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallNotAllowed, Summary: "2 items are open."}
	}
	err := c.dispatch(context.Background(), []string{"epic", "close", "Plan", "--project", "OVERSIRE"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 1 || err.Error() != "2 items are open." {
		t.Fatal(err)
	}
	err = c.dispatch(context.Background(), []string{"epic", "close", "Plan", "--project", "OVERSIRE", "--json"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 1 || !errors.Is(err, errSilent) || !strings.Contains(out.String(), `"status": "notAllowed"`) {
		t.Fatal(err, out)
	}
}

func TestProjectNorthStarSet(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"project", "north-star", "--project", "OVERSIRE", "--set", "x"}); err != nil {
		t.Fatal(err)
	}
	if a := app.args(t); app.last(t).Action != "project.setNorthStar" || a["text"] != "x" || a["project"] != "OVERSIRE" {
		t.Fatal(app.last(t))
	}
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "Ship the plan.", Data: json.RawMessage(`{"text":"Ship the plan."}`)}
	}
	out.Reset()
	if err := c.dispatch(context.Background(), []string{"project", "north-star", "--project", "OVERSIRE"}); err != nil {
		t.Fatal(err)
	}
	if app.last(t).Action != "project.northStar" || out.String() != "Ship the plan.\n" {
		t.Fatal(app.last(t), out)
	}
}

func TestAppRun(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"app", "run", "item.setEpic", `{"key":"OVERSIRE-1","epic":"none"}`}); err != nil {
		t.Fatal(err)
	}
	if a := app.args(t); app.last(t).Action != "item.setEpic" || a["epic"] != "none" {
		t.Fatal(app.last(t))
	}
	err := c.dispatch(context.Background(), []string{"app", "run", "item.setEpic", `[1]`})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 2 {
		t.Fatal(err)
	}
}

func TestNoDaemonExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}})
	err := c.dispatch(context.Background(), []string{"epic", "list"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 3 || !strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}

func TestNoAppExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	off := false
	cfg := config.ServeConfig{Port: port, Tailscale: &off}
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &cfg})
	events := eventlog.New(10)
	srv := server.NewServer(cfg, func() wire.DaemonInfo { return wire.DaemonInfo{} }, nil, events, sessions.NewBook("mbp", "", events, nil), nil)
	srv.Log = c.log
	srv.Handle(appcall.New(srv.Deliver, func() bool { return srv.HasClient(true) }, time.Second).Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	var err error
	for i := 0; i < 50; i++ {
		err = c.dispatch(context.Background(), []string{"epic", "list"})
		if !strings.Contains(err.Error(), "does not answer") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 3 || err.Error() != "Open the OverSire app." {
		t.Fatal(err)
	}
}

func TestEpicShowText(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "Plan: 1 of 3 done. Next: Relay.", Data: json.RawMessage(`{
			"project":"OVERSIRE","id":"e1","title":"Plan","state":"active","url":"https://plane/e1","goal":"Ship it.\nDone when: CI passes.",
			"targetDate":"2026-12-01","forecast":{"start":"2026-11-19","end":"2026-12-04","lateBy":3},"relations":{"loaded":5,"total":5,"failed":0},
			"progress":{"done":1,"total":3,"open":2,"started":1,"readyToClose":false},"members":[],"done":["OVERSIRE-480"],
			"steps":[{"number":1,"key":"OVERSIRE-484","title":"App side","state":"started","start":"2026-11-19","end":"2026-11-20","slack":0,"critical":true},
			{"number":2,"key":"OVERSIRE-485","title":"Relay","state":"waiting","waitsOnStep":1,"start":"2026-11-21","end":"2026-11-22","slack":1,"critical":false}],
			"next":"OVERSIRE-484"}`)}
	}
	if err := c.dispatch(context.Background(), []string{"epic", "show", "Plan", "--project", "OVERSIRE"}); err != nil {
		t.Fatal(err)
	}
	want := `Plan: 1 of 3 done. Next: Relay.
State: active. Progress: 1 of 3 done, 1 started. Forecast: 2026-11-19 to 2026-12-04, 3 days late. Dates: - to 2026-12-01.
Goal: Ship it.
  Done when: CI passes.
URL: https://plane/e1
#  KEY            STATE    START       END         SLACK  WAITS ON  TITLE
1  *OVERSIRE-484  started  2026-11-19  2026-11-20  0d     -         App side
2  OVERSIRE-485   waiting  2026-11-21  2026-11-22  1d     step 1    Relay
Done: OVERSIRE-480
`
	if out.String() != want {
		t.Fatalf("got\n%s", out)
	}
}

func TestEpicCreate(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "Epic Plan is made.", Data: json.RawMessage(`{"id":"e7"}`)}
	}
	if err := c.dispatch(context.Background(), []string{"epic", "create", "--project", "OVERSIRE", "--title", "Plan", "--target", "2026-12-01"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(app.args(t)); string(b) != `{"project":"OVERSIRE","targetDate":"2026-12-01","title":"Plan"}` || out.String() != "Epic Plan is made.\nId: e7\n" {
		t.Fatal(string(b), out)
	}
}

func TestProjectStatusesLabelsPeople(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	data := map[string]string{
		"project.statuses": `[{"id":"s1","name":"Backlog","group":"backlog","category":"todo","color":"#000"},{"id":"s2","name":"Done","category":"done","color":"#0f0"}]`,
		"project.labels":   `[{"id":"l1","name":"bug","color":"#f00"}]`,
		"project.people":   `[{"id":"u1","name":"Ann","me":true},{"id":"u2","name":"Bo","me":false}]`,
	}
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "ok.", Data: json.RawMessage(data[call.Action])}
	}
	for _, sub := range []string{"statuses", "labels", "people"} {
		for _, args := range [][]string{{"project", sub, "oversire"}, {"project", sub, "--project", "oversire"}} {
			out.Reset()
			if err := c.dispatch(context.Background(), args); err != nil {
				t.Fatal(args, err)
			}
			if call := app.last(t); call.Action != "project."+sub || string(call.Args) != `{"project":"OVERSIRE"}` {
				t.Fatal(args, call)
			}
		}
	}
	checks := map[string][]string{"statuses": {"NAME", "GROUP", "CATEGORY", "Backlog", "backlog", "s2"}, "labels": {"bug", "l1"}, "people": {"Ann (you)", "u1", "Bo"}}
	for sub, wants := range checks {
		out.Reset()
		c.dispatch(context.Background(), []string{"project", sub, "oversire"})
		for _, w := range wants {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: no %q in %q", sub, w, out)
			}
		}
	}
	for _, args := range [][]string{{"project", "statuses"}, {"project", "statuses", "a", "b"}, {"project", "statuses", "a", "--project", "b"}, {"project", "frob"}, {"project"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestProjectLabelsAdd(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "Label ui is in ACME.", `{"id":"l9"}`)
	if err := c.dispatch(context.Background(), []string{"project", "labels", "acme", "--add", "ui"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); call.Action != "project.addLabel" || string(call.Args) != `{"color":"#3B82F6","name":"ui","project":"ACME"}` {
		t.Fatal(call.Action, string(call.Args))
	}
	if out.String() != "Label ui is in ACME.\nId: l9\n" {
		t.Fatalf("got %q", out)
	}
	if err := c.dispatch(context.Background(), []string{"project", "labels", "--project", "acme", "--add", "Needs review", "--color", "#00ff00"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); string(call.Args) != `{"color":"#00ff00","name":"Needs review","project":"ACME"}` {
		t.Fatal(string(call.Args))
	}
}

func TestProjectLabelsAddBadColor(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, color := range []string{"red", "#fff", "00ff00", "#00ff0g", "#00ff000"} {
		err := c.dispatch(context.Background(), []string{"project", "labels", "acme", "--add", "ui", "--color", color})
		if exitCode(err) != 2 {
			t.Errorf("%s: %v", color, err)
		}
	}
	if err := c.dispatch(context.Background(), []string{"project", "labels", "acme", "--add", " "}); exitCode(err) != 2 {
		t.Error(err)
	}
}

func TestProjectLabelsColorWithoutAdd(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	err := c.dispatch(context.Background(), []string{"project", "labels", "acme", "--color", "#00ff00"})
	if exitCode(err) != 2 || app.count() != 0 {
		t.Fatal(err)
	}
}

func TestProjectRefresh(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME is loaded again.", "")
	for _, args := range [][]string{{"project", "refresh", "acme"}, {"project", "refresh", "--project", "acme"}} {
		out.Reset()
		if err := c.dispatch(context.Background(), args); err != nil {
			t.Fatal(args, err)
		}
		if call := app.last(t); call.Action != "project.refresh" || string(call.Args) != `{"project":"ACME"}` {
			t.Fatal(args, call)
		}
	}
	if out.String() != "ACME is loaded again.\n" {
		t.Fatalf("got %q", out)
	}
	for _, args := range [][]string{{"project", "refresh"}, {"project", "refresh", "a", "b"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestEpicUpdateHealthAndNote(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	c.in = strings.NewReader("Two items slip.\n")
	err := c.dispatch(context.Background(), []string{"epic", "update", "Plan", "--project", "OVERSIRE", "--health", "at_risk", "--note-file", "-", "--target", "2026-12-01"})
	if err != nil {
		t.Fatal(err)
	}
	a := app.args(t)
	if app.last(t).Action != "epic.update" || a["health"] != "at_risk" || a["note"] != "Two items slip.\n" || a["targetDate"] != "2026-12-01" || len(a) != 5 {
		t.Fatal(a)
	}
	if out.String() != "ok.\n" {
		t.Fatal(out)
	}
	big := t.TempDir() + "/note.md"
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	err = c.dispatch(context.Background(), []string{"epic", "update", "Plan", "--project", "OVERSIRE", "--note-file", big})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 2 {
		t.Fatal(err)
	}
}

func TestEpicUpdateHealthOnlyCounts(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"epic", "update", "Plan", "--project", "OVERSIRE", "--health", "on_track"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(app.args(t)); string(b) != `{"epic":"Plan","health":"on_track","project":"OVERSIRE"}` {
		t.Fatal(string(b))
	}
}

func TestEpicUpdateHealthRefusalExit1(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallNotAllowed, Summary: "Plane has no epic health."}
	}
	err := c.dispatch(context.Background(), []string{"epic", "update", "Plan", "--project", "OVERSIRE", "--health", "at_risk"})
	if e, ok := errors.AsType[*exitError](err); !ok || e.code != 1 || err.Error() != "Plane has no epic health." {
		t.Fatal(err)
	}
}

func TestEpicShowAndListHealth(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		data := `[{"project":"ACME","id":"e1","title":"Invoicing v2","state":"active","health":"at_risk","progress":"1 of 2 done","next":[]}]`
		if call.Action == "epic.show" {
			data = `{"title":"Invoicing v2","state":"active","health":"off_track","forecast":{"start":"2026-11-19"},"progress":{"done":1,"total":2,"open":1,"started":0}}`
		}
		return wire.AppCallResult{ID: call.ID, Status: wire.AppCallOK, Summary: "ok.", Data: json.RawMessage(data)}
	}
	if err := c.dispatch(context.Background(), []string{"epic", "list", "--project", "ACME"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "active, at risk") {
		t.Fatal(out)
	}
	out.Reset()
	if err := c.dispatch(context.Background(), []string{"epic", "show", "Invoicing v2", "--project", "ACME"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Health: off track.") {
		t.Fatal(out)
	}
}
