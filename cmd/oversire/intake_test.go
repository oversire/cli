package main

import (
	"context"
	"strings"
	"testing"
	"time"

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

const intakeListData = `{"items":[{"project":"ACME","key":"ACME-7","id":"i1","workItemId":"w1","title":"Add export","priority":"high","author":"Ann","createdAt":"2026-11-18T10:00:00Z","status":"pending"},
	{"project":"ACME","id":"i2","workItemId":"w2","title":"Fix login","author":"Bo","createdAt":"2026-11-19T10:00:00Z","status":"snoozed","snoozedUntil":"2026-11-25T09:00:00Z"}],"total":3}`

func TestIntakeListArgsAndTable(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "3 intake items.", intakeListData)
	call, got, err := runItem(t, c, app, "intake", "list", "--project", "acme", "--status", "ALL", "--detail", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.list", map[string]any{"project": "ACME", "status": "all", "detail": true, "limit": 2}, call, got)
	s := out.String()
	for _, want := range []string{"KEY", "STATUS", "PRIORITY", "TITLE", "ACME-7", "Add export", "high", "snoozed until", "Fix login", "Showing 2 of 3."} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q in %q", want, s)
		}
	}
	out.Reset()
	if _, got, err = runItem(t, c, app, "intake", "list"); err != nil || len(got) != 0 {
		t.Fatal(err, got)
	}
}

func TestIntakeListBadStatus(t *testing.T) {
	c, _, _ := testCLI(t, "")
	appDaemon(t, c)
	for _, args := range [][]string{{"intake", "list", "--status", "done"}, {"intake", "list", "x"}, {"intake", "list", "--limit", "-1"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestIntakeShowKeyAndNumber(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "1 intake item.", `{"items":[{"project":"ACME","key":"ACME-7","id":"i1","title":"Add export","priority":"high","author":"Ann","createdAt":"2026-11-18T10:00:00Z","status":"pending","body":"Why.\n","labels":["bug"]}],"total":1}`)
	call, got, err := runItem(t, c, app, "intake", "show", "acme-7")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.list", map[string]any{"project": "ACME", "id": "ACME-7", "detail": true}, call, got)
	for _, want := range []string{"# Add export", "Key: ACME-7", "Labels: bug", "Why."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in %q", want, out)
		}
	}
	call, got, err = runItem(t, c, app, "intake", "show", "7", "--project", "acme")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.list", map[string]any{"project": "ACME", "id": "ACME-7", "detail": true}, call, got)
}

func TestIntakeShowIdNeedsProject(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	for _, args := range [][]string{{"intake", "show", "3a9f"}, {"intake", "show", "7"}, {"intake", "show", "ACME-7", "--project", "other"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	answerWith(app, wire.AppCallOK, "1 intake item.", `{"items":[],"total":0}`)
	call, got, err := runItem(t, c, app, "intake", "show", "3a9f", "--project", "acme")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.list", map[string]any{"project": "ACME", "id": "3a9f", "detail": true}, call, got)
}

func TestIntakeAcceptWithComment(t *testing.T) {
	c, out, _ := testCLI(t, "Looks good.\n")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "ACME-7 is accepted.", `{}`)
	call, got, err := runItem(t, c, app, "intake", "accept", "ACME-7", "--comment-file", "-")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.accept", map[string]any{"project": "ACME", "id": "ACME-7", "comment": "Looks good.\n"}, call, got)
	if !strings.Contains(out.String(), "ACME-7 is accepted.") {
		t.Fatal(out)
	}
	call, got, err = runItem(t, c, app, "intake", "decline", "ACME-7")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.decline", map[string]any{"project": "ACME", "id": "ACME-7"}, call, got)
}

func TestIntakeSnoozeDateToNine(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "Snoozed.", `{}`)
	day := time.Now().AddDate(0, 0, 3)
	_, got, err := runItem(t, c, app, "intake", "snooze", "ACME-7", "--until", day.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, time.Local).Format(time.RFC3339)
	if got["until"] != want {
		t.Fatalf("got %v, want %s", got["until"], want)
	}
	call, got, err := runItem(t, c, app, "intake", "snooze", "ACME-7", "--days", "4")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.snooze", map[string]any{"project": "ACME", "id": "ACME-7", "days": 4}, call, got)
	for _, args := range [][]string{
		{"intake", "snooze", "ACME-7"},
		{"intake", "snooze", "ACME-7", "--until", "2020-01-01"},
		{"intake", "snooze", "ACME-7", "--until", "soon"},
		{"intake", "snooze", "ACME-7", "--until", "2099-01-01", "--days", "2"},
	} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestIntakeDuplicateNeedsOf(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	if err := c.dispatch(context.Background(), []string{"intake", "duplicate", "ACME-7"}); exitCode(err) != 2 {
		t.Fatal(err)
	}
	answerWith(app, wire.AppCallOK, "Closed as a duplicate.", `{}`)
	call, got, err := runItem(t, c, app, "intake", "duplicate", "ACME-7", "--of", "3")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.merge", map[string]any{"project": "ACME", "id": "ACME-7", "of": "ACME-3"}, call, got)
}

func TestIntakeEditParentAndRelation(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "Edited.", `{}`)
	call, got, err := runItem(t, c, app, "intake", "edit", "ACME-7", "--priority", "high", "--parent", "ACME-2", "--blocked-by", "ACME-5")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.edit", map[string]any{"project": "ACME", "id": "ACME-7", "priority": "high", "parent": "ACME-2", "kind": "blockedBy", "other": "ACME-5"}, call, got)
	call, got, err = runItem(t, c, app, "intake", "edit", "ACME-7", "--parent", "none")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.edit", map[string]any{"project": "ACME", "id": "ACME-7", "parent": "none"}, call, got)
	for _, args := range [][]string{
		{"intake", "edit", "ACME-7"},
		{"intake", "edit", "ACME-7", "--blocks", "ACME-1", "--relates-to", "ACME-2"},
	} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestIntakeEditTitle(t *testing.T) {
	c, _, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallOK, "Edited.", `{}`)
	call, got, err := runItem(t, c, app, "intake", "edit", "ACME-7", "--title", "Better title")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, "intake.edit", map[string]any{"project": "ACME", "id": "ACME-7", "patch": map[string]any{"title": "Better title"}}, call, got)
}

func TestIntakeNoIntakeExit1(t *testing.T) {
	c, out, _ := testCLI(t, "")
	app := appDaemon(t, c)
	answerWith(app, wire.AppCallNotAllowed, "Big Timer has no intake.", "")
	err := c.dispatch(context.Background(), []string{"intake", "list", "--project", "bt"})
	if exitCode(err) != 1 || !strings.Contains(err.Error(), "Big Timer has no intake.") || out.Len() != 0 {
		t.Fatalf("%v %q", err, out)
	}
}
