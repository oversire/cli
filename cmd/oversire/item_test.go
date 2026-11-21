package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
	for _, args := range [][]string{{"item"}, {"item", "show"}, {"item", "show", "A-1", "B-2"}, {"item", "frob"}, {"item", "show", "nokey"}} {
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

// runItem runs an item command against the fake app and gives the last call.
func runItem(t *testing.T, c *cli, app *fakeApp, args ...string) (wire.AppCall, map[string]any, error) {
	t.Helper()
	err := c.dispatch(context.Background(), args)
	if err != nil {
		return wire.AppCall{}, nil, err
	}
	return app.last(t), app.args(t), nil
}

func wantArgs(t *testing.T, action string, want map[string]any, call wire.AppCall, got map[string]any) {
	t.Helper()
	b, _ := json.Marshal(want)
	var w map[string]any
	json.Unmarshal(b, &w)
	if call.Action != action || !reflect.DeepEqual(got, w) {
		t.Fatalf("%s %v, want %s %v", call.Action, got, action, w)
	}
}

func TestItemListArgsAndTable(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "9 items.", `{"total":9,"items":[
		{"key":"ACME-1","title":"Fix login","status":"In Progress","assignees":["Ann","Bo"]},
		{"key":"ACME-2","title":"Docs","status":"Todo","assignees":[]}]}`)
	call, got, err := runItem(t, c, app, "item", "list", "--project", "acme", "--group", "progress,review", "--mine", "--label", "bug", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.list", map[string]any{"project": "ACME", "group": "progress,review", "assignee": "me", "label": "bug", "limit": 5}, call, got)
	for _, want := range []string{"KEY", "STATUS", "ASSIGNEES", "TITLE", "ACME-1", "Ann,Bo", "In Progress", "Fix login", "Showing 2 of 9."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in %q", want, out)
		}
	}
}

func TestItemSearch(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "3 results.", `{"items":[{"key":"ACME-1","title":"Fix login","status":"Todo"},{"key":"ACME-7","title":"Login page"}],"prs":[{"number":4,"title":"PR title"}],"runs":[],"sessions":[]}`)
	call, got, err := runItem(t, c, app, "item", "search", "login", "--project", "acme")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "search.all", map[string]any{"query": "login", "project": "ACME"}, call, got)
	if s := out.String(); !strings.Contains(s, "ACME-7") || !strings.Contains(s, "Fix login") || strings.Contains(s, "PR title") {
		t.Fatalf("got %q", s)
	}
}

func TestItemCreateBodyFromStdin(t *testing.T) {
	c, out, _ := testCLI(t, "# Spec\n")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME-9 is made.", `{"key":"ACME-9"}`)
	call, got, err := runItem(t, c, app, "item", "create", "--project", "acme", "--title", "New", "--body-file", "-", "--status", "todo",
		"--priority", "high", "--parent", "3", "--label", "a", "--label", "b", "--epic", "Plan")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.create", map[string]any{"project": "ACME", "title": "New", "body": "# Spec\n", "status": "todo", "priority": "high",
		"parent": "ACME-3", "labels": []string{"a", "b"}, "epic": "Plan"}, call, got)
	if !strings.Contains(out.String(), "Key: ACME-9") {
		t.Fatal(out)
	}
}

func TestItemCreateUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{{"item", "create", "--title", "T"}, {"item", "create", "--project", "acme"}, {"item", "create", "--project", "acme", "--title", "T", "x"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemEditTitleOnlyUsesUpdate(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "edit", "acme-1", "--title", "New title")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.update", map[string]any{"key": "ACME-1", "title": "New title"}, call, got)
	if err := c.dispatch(context.Background(), []string{"item", "edit", "acme-1"}); exitCode(err) != 2 {
		t.Fatal(err)
	}
}

func TestItemEditBodyUsesEdit(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	file := filepath.Join(t.TempDir(), "b.md")
	os.WriteFile(file, []byte("New body"), 0o600)
	call, got, err := runItem(t, c, app, "item", "edit", "acme-1", "--body-file", file, "--title", "T")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.edit", map[string]any{"key": "ACME-1", "title": "T", "body": "New body"}, call, got)
}

func TestItemStatusStartDoneReopen(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for _, tc := range []struct {
		args   []string
		action string
		want   map[string]any
	}{
		{[]string{"item", "status", "acme-1", "In Review"}, "item.setStatus", map[string]any{"key": "ACME-1", "status": "In Review", "force": false}},
		{[]string{"item", "done", "acme-1", "--force"}, "item.setStatus", map[string]any{"key": "ACME-1", "status": "done", "force": true}},
		{[]string{"item", "start", "acme-1"}, "item.start", map[string]any{"key": "ACME-1"}},
		{[]string{"item", "reopen", "acme-1"}, "item.reopen", map[string]any{"key": "ACME-1"}},
		{[]string{"item", "priority", "acme-1", "high"}, "item.setPriority", map[string]any{"key": "ACME-1", "priority": "high"}},
	} {
		call, got, err := runItem(t, c, app, tc.args...)
		if err != nil {
			t.Fatal(tc.args, err)
		}
		wantArgs(t, tc.action, tc.want, call, got)
	}
}

func TestItemAssign(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "assign", "acme-1", "--me")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.assignMe", map[string]any{"key": "ACME-1"}, call, got)
	call, got, _ = runItem(t, c, app, "item", "assign", "acme-1", "alice", "bob")
	wantArgs(t, "item.assign", map[string]any{"key": "ACME-1", "people": []string{"alice", "bob"}}, call, got)
	call, got, _ = runItem(t, c, app, "item", "assign", "acme-1", "--none")
	wantArgs(t, "item.assign", map[string]any{"key": "ACME-1", "people": []string{}}, call, got)
	for _, args := range [][]string{{"item", "assign", "acme-1", "--me", "alice"}, {"item", "assign", "acme-1"}, {"item", "assign", "acme-1", "--none", "alice"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemRelateFlags(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for flag, kind := range map[string]string{"--blocks": "blocks", "--blocked-by": "blockedBy", "--relates-to": "relatesTo", "--duplicate-of": "duplicate"} {
		call, got, err := runItem(t, c, app, "item", "relate", "acme-1", flag, "acme-2")
		if err != nil {
			t.Fatal(flag, err)
		}
		wantArgs(t, "item.relate", map[string]any{"key": "ACME-1", "kind": kind, "other": "ACME-2"}, call, got)
	}
	call, got, _ := runItem(t, c, app, "item", "relate", "acme-1", "--blocks", "acme-2", "--remove")
	wantArgs(t, "item.relate", map[string]any{"key": "ACME-1", "kind": "blocks", "other": "ACME-2", "remove": true}, call, got)
	for _, args := range [][]string{{"item", "relate", "acme-1", "--blocks", "acme-2", "--relates-to", "acme-3"}, {"item", "relate", "acme-1"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemParentNone(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "parent", "acme-2", "none")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.update", map[string]any{"key": "ACME-2", "parent": ""}, call, got)
	call, got, _ = runItem(t, c, app, "item", "parent", "acme-2", "acme-1")
	wantArgs(t, "item.update", map[string]any{"key": "ACME-2", "parent": "ACME-1"}, call, got)
}

func TestItemLink(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "link", "acme-1", "https://example.com/x", "--title", "Doc")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.link", map[string]any{"key": "ACME-1", "url": "https://example.com/x", "title": "Doc"}, call, got)
	for _, u := range []string{"example.com", "ftp://example.com"} {
		if err := c.dispatch(context.Background(), []string{"item", "link", "acme-1", u}); exitCode(err) != 2 {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestItemAttach(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	file := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(file, []byte("hello"), 0o600)
	call, got, err := runItem(t, c, app, "item", "attach", "acme-1", file)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.attach", map[string]any{"key": "ACME-1", "name": "note.txt", "mimeType": "text/plain", "data": "aGVsbG8="}, call, got)
	call, got, _ = runItem(t, c, app, "item", "attach", "acme-1", file, "--name", "other.txt")
	if got["name"] != "other.txt" || call.Action != "item.attach" {
		t.Fatal(got)
	}
}

func TestItemAttachTooLarge(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	file := filepath.Join(t.TempDir(), "big.bin")
	f, _ := os.Create(file)
	f.Truncate(11 << 20)
	f.Close()
	err := c.dispatch(context.Background(), []string{"item", "attach", "acme-1", file})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "The file has 11 MB. The limit is 10 MB.") {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.calls) != 0 {
		t.Fatal("a call went out")
	}
	if err := c.dispatch(context.Background(), []string{"item", "attach", "acme-1", t.TempDir()}); exitCode(err) != 2 {
		t.Fatal(err)
	}
}

func TestItemNotSupportedExit1(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallNotAllowed, "Big Timer has no links.", "")
	err := c.dispatch(context.Background(), []string{"item", "link", "BT-1", "https://example.com"})
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "Big Timer has no links.") || out.Len() != 0 {
		t.Fatalf("%v %q", err, out)
	}
}

func TestItemCommentEmptyBody(t *testing.T) {
	c, _, _ := testCLI(t, "  \n")
	appDaemon(t, c)
	for _, args := range [][]string{{"item", "comment", "acme-1", "--body-file", "-"}, {"item", "comment", "acme-1"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemCommentSends(t *testing.T) {
	c, _, _ := testCLI(t, "Done in abc123.\n")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "comment", "acme-1", "--body-file", "-")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.comment", map[string]any{"key": "ACME-1", "body": "Done in abc123.\n"}, call, got)
}

func TestItemLabelModes(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"item", "label", "acme-1", "--add", "bug", "ui"}, map[string]any{"key": "ACME-1", "add": []string{"bug", "ui"}}},
		{[]string{"item", "label", "acme-1", "bug", "--remove"}, map[string]any{"key": "ACME-1", "remove": []string{"bug"}}},
		{[]string{"item", "label", "acme-1", "--set", "Needs review", "bug"}, map[string]any{"key": "ACME-1", "labels": []string{"Needs review", "bug"}}},
		{[]string{"item", "label", "acme-1", "--none"}, map[string]any{"key": "ACME-1", "labels": []string{}}},
	} {
		call, got, err := runItem(t, c, app, tc.args...)
		if err != nil {
			t.Fatal(tc.args, err)
		}
		wantArgs(t, "item.setLabels", tc.want, call, got)
	}
}

func TestItemLabelUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{
		{"item", "label", "acme-1", "bug"},
		{"item", "label", "acme-1", "--add"},
		{"item", "label", "acme-1", "--set"},
		{"item", "label", "acme-1", "--none", "bug"},
		{"item", "label", "acme-1", "--add", "--remove", "bug"},
		{"item", "label", "acme-1", "--add", "--none"},
		{"item", "label", "--add", "bug"},
	} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestItemAreas(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "areas", "acme-1", "api", "ui-kit", "v2")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.setAreas", map[string]any{"key": "ACME-1", "areas": []string{"api", "ui-kit", "v2"}}, call, got)
	call, got, _ = runItem(t, c, app, "item", "areas", "acme-1", "--none")
	wantArgs(t, "item.setAreas", map[string]any{"key": "ACME-1", "areas": []string{}}, call, got)
}

func TestItemAreasBad(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{
		{"item", "areas", "acme-1"},
		{"item", "areas", "acme-1", "--none", "api"},
		{"item", "areas", "acme-1", "API"},
		{"item", "areas", "acme-1", "a_b"},
		{"item", "areas", "acme-1", strings.Repeat("a", 33)},
		{"item", "areas", "acme-1", "a", "b", "c", "d", "e", "f", "g"},
	} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	c2, _, _ := testCLI(t, "")
	app := appDaemon(t, c2)
	if _, _, err := runItem(t, c2, app, "item", "areas", "acme-1", strings.Repeat("a", 32), "a", "b", "c", "d", "e"); err != nil {
		t.Fatal("six areas of 32 characters are valid:", err)
	}
}

func TestItemDoneSubItems(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	call, got, err := runItem(t, c, app, "item", "done", "acme-1", "--sub-items")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "item.setStatus", map[string]any{"key": "ACME-1", "status": "done", "force": false, "subItems": true}, call, got)
	call, got, _ = runItem(t, c, app, "item", "status", "acme-1", "In Review", "--sub-items")
	wantArgs(t, "item.setStatus", map[string]any{"key": "ACME-1", "status": "In Review", "force": false, "subItems": true}, call, got)
}
