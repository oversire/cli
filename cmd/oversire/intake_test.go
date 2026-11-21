package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oversire/cli/wire"
)

func TestIntakeCreate(t *testing.T) {
	c, out, _ := testCLI(t, "# Details\n")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME-9 is in the intake.", `{"key":"ACME-9","id":"w1","url":"u","intake":{}}`)
	call, got, err := runItem(t, c, app, "intake", "create", "--project", "acme", "--title", "Idea", "--body-file", "-", "--priority", "high")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.create", map[string]any{"project": "ACME", "title": "Idea", "body": "# Details\n", "priority": "high"}, call, got)
	if out.String() != "ACME-9 is in the intake.\nKey: ACME-9\n" {
		t.Fatalf("got %q", out)
	}
	call, got, _ = runItem(t, c, app, "intake", "create", "--title", "Bare", "--project", "acme")
	wantArgs(t, "intake.create", map[string]any{"project": "ACME", "title": "Bare"}, call, got)
}

func TestIntakeCreateUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for _, args := range [][]string{{"intake"}, {"intake", "frob"}, {"intake", "create", "--title", "T"}, {"intake", "create", "--project", "acme"},
		{"intake", "create", "--project", "acme", "--title", " "}, {"intake", "create", "--project", "acme", "--title", "T", "x"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	if app.count() != 0 {
		t.Fatal("a bad command called the app")
	}
}

func TestIntakeCreateNoIntakeExit1(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallNotAllowed, "Big Timer has no intake.", "")
	err := c.dispatch(context.Background(), []string{"intake", "create", "--project", "bt", "--title", "T"})
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "Big Timer has no intake.") || out.Len() != 0 {
		t.Fatalf("%v %q", err, out)
	}
}
