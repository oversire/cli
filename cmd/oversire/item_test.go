package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

func answerWith(app *fakeApp, status wire.AppCallStatus, summary, data string) {
	app.answer = func(call wire.AppCall) wire.AppCallResult {
		return wire.AppCallResult{ID: call.ID, Status: status, Summary: summary, Data: json.RawMessage(data)}
	}
}

func exitCode(err error) int {
	if e, ok := errors.AsType[*exitError](err); ok {
		return e.code
	}
	return -1
}

func TestItemShowText(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME-12: Fix login", `{"key":"ACME-12","markdown":"# Fix login\n\nBody\n"}`)
	if err := c.dispatch(context.Background(), []string{"item", "show", "acme-12"}); err != nil {
		t.Fatal(err)
	}
	if want := "ACME-12: Fix login\n\n# Fix login\n\nBody\n"; out.String() != want {
		t.Fatalf("got %q", out)
	}
	if app.last(t).Action != "item.markdown" || app.args(t)["key"] != "ACME-12" {
		t.Fatal(app.last(t))
	}
}

func TestItemShowCommentsNote(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME-12: Fix login", `{"key":"ACME-12","markdown":"# Fix login\n","commentsNote":"Big Timer did not answer. The comments are missing."}`)
	if err := c.dispatch(context.Background(), []string{"item", "show", "ACME-12"}); err != nil {
		t.Fatal(err)
	}
	if want := "ACME-12: Fix login\n\n# Fix login\n\nBig Timer did not answer. The comments are missing.\n"; out.String() != want {
		t.Fatalf("got %q", out)
	}
}

func TestItemShowJSON(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	data := `{"key":"ACME-12","title":"Fix login","commentCount":2}`
	answerWith(app, wire.AppCallOK, "ACME-12: Fix login", data)
	if err := c.dispatch(context.Background(), []string{"item", "show", "ACME-12", "--json"}); err != nil {
		t.Fatal(err)
	}
	var want, got any
	json.Unmarshal([]byte(data), &want)
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !jsonEqual(want, got) {
		t.Fatalf("got %s", out)
	}
	if app.last(t).Action != "item.get" || app.args(t)["key"] != "ACME-12" {
		t.Fatal(app.last(t))
	}
}

func TestItemShowJSONNotOK(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallNotAllowed, "The app does not run item.get from the CLI.", "")
	err := c.dispatch(context.Background(), []string{"item", "show", "ACME-12", "--json"})
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "does not run item.get") || out.Len() != 0 {
		t.Fatalf("%v %q", err, out)
	}
}

func TestItemShowProjectNumber(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"item", "show", "12", "--project", "acme"}); err != nil {
		t.Fatal(err)
	}
	if app.args(t)["key"] != "ACME-12" {
		t.Fatal(app.last(t))
	}
	if err := c.dispatch(context.Background(), []string{"item", "show", "acme-13", "--project", "acme"}); err != nil || app.args(t)["key"] != "ACME-13" {
		t.Fatal(err, app.last(t))
	}
}

func TestItemShowProjectMismatch(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	err := c.dispatch(context.Background(), []string{"item", "show", "OTHER-1", "--project", "ACME"})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "the key is not in project ACME") {
		t.Fatal(err)
	}
}

func TestItemShowBadUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{{"item"}, {"item", "show"}, {"item", "show", "A-1", "B-2"}, {"item", "list"}, {"item", "show", "nokey"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemShowNoDaemonExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}})
	err := c.dispatch(context.Background(), []string{"item", "show", "ACME-1"})
	if exitCode(err) != 3 || !strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}

func TestItemShowAgent(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"item", "show", "ACME-1", "--harness", "codex", "--session", "s9"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); call.Agent != "codex s9" {
		t.Fatal(call)
	}
}
