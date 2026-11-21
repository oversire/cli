package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func TestPeopleListTable(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	data := `[{"id":"u1","name":"Ann Lee","login":"ann","email":"ann@example.com","sources":["plane:acme","github:acme"],"machines":["mbp"]},
		{"id":"u2","name":"Bo","sources":[],"machines":[]}]`
	answerWith(app, wire.AppCallOK, "2 people.", data)
	if err := c.dispatch(context.Background(), []string{"people", "list", "--project", "acme"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); call.Action != "people.list" || string(call.Args) != `{"project":"ACME"}` {
		t.Fatal(call.Action, string(call.Args))
	}
	s := out.String()
	for _, want := range []string{"NAME", "LOGIN", "SOURCES", "MACHINES", "ID", "Ann Lee", "ann", "plane:acme,github:acme", "mbp", "u2"} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in %q", want, s)
		}
	}
	if strings.Contains(s, "example.com") {
		t.Fatalf("the table has the email: %q", s)
	}
	out.Reset()
	if err := c.dispatch(context.Background(), []string{"people", "list", "--json"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); string(call.Args) != `{}` {
		t.Fatal(string(call.Args))
	}
	var res wire.AppCallResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || !strings.Contains(string(res.Data), "ann@example.com") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestPeopleListUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{{"people"}, {"people", "frob"}, {"people", "list", "x"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestActivityListArgs(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	at := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	data := `{"entries":[{"at":"` + at.Format(time.RFC3339) + `","key":"ACME-1","text":"Status:\nTodo to Done","actor":"Ann","unread":true,"url":"u"},
		{"at":"` + at.Format(time.RFC3339) + `","key":null,"text":"Comment","actor":null,"unread":false,"url":"u"}]}`
	answerWith(app, wire.AppCallOK, "2 entries.", data)
	if err := c.dispatch(context.Background(), []string{"activity", "list", "--project", "acme", "--unread", "--limit", "5"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(app.last(t).Args, &got)
	if app.last(t).Action != "activity.list" || got["project"] != "ACME" || got["unread"] != true || got["limit"] != float64(5) {
		t.Fatal(app.last(t).Action, got)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	local := at.Local().Format("2006-01-02 15:04")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "2 entries.") || !strings.Contains(lines[1], "AT") {
		t.Fatalf("got %q", out)
	}
	if !strings.HasPrefix(lines[2], "* "+local) || !strings.Contains(lines[2], "ACME-1") || !strings.Contains(lines[2], "Status: Todo to Done") {
		t.Errorf("unread row: %q", lines[2])
	}
	if !strings.HasPrefix(lines[3], "  "+local) {
		t.Errorf("read row: %q", lines[3])
	}
	if err := c.dispatch(context.Background(), []string{"activity", "list"}); err != nil {
		t.Fatal(err)
	}
	if string(app.last(t).Args) != `{}` {
		t.Fatal(string(app.last(t).Args))
	}
}

func TestActivityListBadArgs(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for _, args := range [][]string{{"activity"}, {"activity", "frob"}, {"activity", "list", "x"}, {"activity", "list", "--limit", "0"},
		{"activity", "list", "--limit", "201"}, {"activity", "list", "--limit", "-1"}, {"activity", "list", "--limit", "x"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	if app.count() != 0 {
		t.Fatal("a bad command called the app")
	}
	for _, n := range []string{"1", "200"} {
		if err := c.dispatch(context.Background(), []string{"activity", "list", "--limit", n}); err != nil {
			t.Error(n, err)
		}
	}
}
