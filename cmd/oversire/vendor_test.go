package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

func vendorOut(t *testing.T, o wire.VendorRunOutput) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func vendorCLI(t *testing.T) (*cli, *bytes.Buffer, *bytes.Buffer, *fakeApp) {
	t.Helper()
	c, out, _ := testCLI(t, "")
	errOut := &bytes.Buffer{}
	c.errOut = errOut
	c.wd = t.TempDir()
	return c, out, errOut, appDaemon(t, c)
}

func TestVendorRunSendsArgs(t *testing.T) {
	c, _, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "plane-cli", Account: "plane:ocral"}))
	args := []string{"vendor", "run", "--project", "oversire", "--", "wi", "attachment", "list", "OVERSIRE-12", "--json"}
	if err := c.dispatch(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); call.Action != "vendor.run" {
		t.Fatal(call)
	}
	var got wire.VendorRunArgs
	if err := json.Unmarshal(app.last(t).Args, &got); err != nil {
		t.Fatal(err)
	}
	want := wire.VendorRunArgs{Project: "OVERSIRE", Args: []string{"wi", "attachment", "list", "OVERSIRE-12", "--json"}, Cwd: c.wd}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, ok := app.args(t)["account"]; ok {
		t.Fatal("account is set")
	}
}

func TestVendorRunAccount(t *testing.T) {
	c, _, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:oversire"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--account", "github:oversire", "--", "pr", "list"}); err != nil {
		t.Fatal(err)
	}
	a := app.args(t)
	if a["account"] != "github:oversire" || a["project"] != nil {
		t.Fatal(a)
	}
}

func TestVendorRunWritesStdoutAndStderr(t *testing.T) {
	c, out, errOut, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "plane-cli", Account: "plane:ocral", Stdout: "[1]\n", Stderr: "warn\n"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--project", "X", "--", "wi", "list"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[1]\n" || errOut.String() != "warn\n" {
		t.Fatalf("out %q err %q", out, errOut)
	}
}

func TestVendorRunVendorExitIsExit1(t *testing.T) {
	c, out, errOut, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:o", Exit: 4, Stdout: "x", Stderr: "bad\n"}))
	err := c.dispatch(context.Background(), []string{"vendor", "run", "--account", "o", "--", "pr", "view"})
	if exitCode(err) != 1 || err.Error() != "gh exited with 4" {
		t.Fatal(err)
	}
	if out.String() != "x" || errOut.String() != "bad\n" {
		t.Fatalf("out %q err %q", out, errOut)
	}
}

func TestVendorRunTruncatedNote(t *testing.T) {
	c, _, errOut, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:o", Stdout: "x", Truncated: true}))
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--account", "o", "--", "api", "/x"}); err != nil {
		t.Fatal(err)
	}
	if errOut.String() != "oversire: the output is cut at 1 MB.\n" {
		t.Fatalf("%q", errOut)
	}
}

func TestVendorRunJSON(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	data := vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:o", Stdout: "x"})
	answerWith(app, wire.AppCallOK, "ok.", data)
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--account", "o", "--json", "--", "api", "/x"}); err != nil {
		t.Fatal(err)
	}
	var res wire.AppCallResult
	err := json.Unmarshal(out.Bytes(), &res)
	var want, got any
	json.Unmarshal([]byte(data), &want)
	json.Unmarshal(res.Data, &got)
	if err != nil || res.Status != wire.AppCallOK || !jsonEqual(want, got) {
		t.Fatalf("%s %v", out, err)
	}

	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:o", Exit: 2}))
	out.Reset()
	err = c.dispatch(context.Background(), []string{"vendor", "run", "--account", "o", "--json", "--", "api", "/x"})
	if exitCode(err) != 1 || !strings.Contains(out.String(), `"exit": 2`) {
		t.Fatal(err, out)
	}
}

func TestVendorRunUsage(t *testing.T) {
	c, _, _, _ := vendorCLI(t)
	for _, args := range [][]string{
		{"vendor"},
		{"vendor", "frob"},
		{"vendor", "run", "--project", "X"},
		{"vendor", "run", "--project", "X", "--"},
		{"vendor", "run", "--project", "X", "wi", "--", "list"},
		{"vendor", "run", "--", "wi", "list"},
		{"vendor", "run", "--project", "X", "--account", "a", "--", "wi", "list"},
		{"vendor", "run", "--project", "9x", "--", "wi", "list"},
		{"vendor", "run", "--bogus", "--", "wi", "list"},
		{"vendor", "ls", "extra"},
		{"vendor", "skill"},
		{"vendor", "skill", "a", "b"},
		{"vendor", "install"},
		{"vendor", "install", "plane", "bigtimer"},
		{"vendor", "remove"},
		{"vendor", "install", "--bogus", "plane"},
	} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestVendorRunRefusedExit1(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallNotAllowed, "The app refuses the flag --token.", "")
	err := c.dispatch(context.Background(), []string{"vendor", "run", "--project", "X", "--", "wi", "list", "--token", "x"})
	if exitCode(err) != 1 || err.Error() != "The app refuses the flag --token." || out.Len() != 0 {
		t.Fatal(err, out)
	}
}

func TestVendorRunTimeoutPartial(t *testing.T) {
	c, out, errOut, app := vendorCLI(t)
	answerWith(app, wire.AppCallFailed, "plane-cli did not end in 45 s.",
		vendorOut(t, wire.VendorRunOutput{Vendor: "plane-cli", Account: "plane:o", Exit: -1, Stdout: "part", Stderr: "slow\n", TimedOut: true}))
	err := c.dispatch(context.Background(), []string{"vendor", "run", "--project", "X", "--", "wi", "list"})
	if exitCode(err) != 1 || err.Error() != "plane-cli did not end in 45 s." {
		t.Fatal(err)
	}
	if out.String() != "part" || errOut.String() != "slow\n" {
		t.Fatalf("out %q err %q", out, errOut)
	}
}

func TestVendorRunHelpFirst(t *testing.T) {
	c, _, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "plane-cli", Account: "plane:ocral"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--project", "X", "--", "--help"}); err != nil {
		t.Fatal(err)
	}
	var got wire.VendorRunArgs
	if err := json.Unmarshal(app.last(t).Args, &got); err != nil || !reflect.DeepEqual(got.Args, []string{"--help"}) {
		t.Fatalf("got %+v %v", got, err)
	}
}

func skillOut(t *testing.T, o wire.VendorSkillOutput) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVendorSkillPrintsMarkdown(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	md := "# Plane (plane): plane-cli through OverSire\n\n## plane-cli reference\n\n# plane-cli\n"
	answerWith(app, wire.AppCallOK, "The skill of plane-cli.",
		skillOut(t, wire.VendorSkillOutput{Vendor: "plane", Binary: "plane-cli", Path: "/usr/local/bin/plane-cli", Reference: "skill", Markdown: md}))
	if err := c.dispatch(context.Background(), []string{"vendor", "skill", "plane"}); err != nil {
		t.Fatal(err)
	}
	if call, a := app.last(t), app.args(t); call.Action != "vendor.skill" || len(a) != 1 || a["vendor"] != "plane" {
		t.Fatalf("%s %s", call.Action, call.Args)
	}
	if want := "The skill of plane-cli.\n\n" + md; out.String() != want {
		t.Fatalf("got %q", out)
	}
}

func TestVendorSkillJSON(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	data := skillOut(t, wire.VendorSkillOutput{Vendor: "github", Binary: "gh", Reference: "help", Markdown: "# GitHub\n"})
	answerWith(app, wire.AppCallOK, "gh has no skill text. Read its help.", data)
	if err := c.dispatch(context.Background(), []string{"vendor", "skill", "github", "--json"}); err != nil {
		t.Fatal(err)
	}
	var res wire.AppCallResult
	err := json.Unmarshal(out.Bytes(), &res)
	var want, got any
	json.Unmarshal([]byte(data), &want)
	json.Unmarshal(res.Data, &got)
	if err != nil || res.Status != wire.AppCallOK || !jsonEqual(want, got) {
		t.Fatalf("%s %v", out, err)
	}
}

func TestVendorSkillNotAllowedExit1(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallNotAllowed, "OverSire has no vendor linear. Give bigtimer, github or plane.", "")
	err := c.dispatch(context.Background(), []string{"vendor", "skill", "linear"})
	if exitCode(err) != 1 || err.Error() != "OverSire has no vendor linear. Give bigtimer, github or plane." || out.Len() != 0 {
		t.Fatal(err, out)
	}
}

func TestVendorListTable(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	rows := []wire.VendorCLIRow{
		{Provider: "plane", Vendor: "Plane", Binary: "plane-cli", Path: "/usr/local/bin/plane-cli", Version: "0.2.2", MinVersion: "0.2.2",
			Source: "path", State: "ok", Accounts: []string{"plane:ocral"}, Projects: []string{"OVERSIRE", "SLING"}},
		{Provider: "github", Vendor: "GitHub", Binary: "gh", State: "missing", Accounts: []string{}, Projects: []string{}},
	}
	b, _ := json.Marshal(rows)
	answerWith(app, wire.AppCallOK, "2 vendor CLIs.", string(b))
	if err := c.dispatch(context.Background(), []string{"vendor", "ls"}); err != nil {
		t.Fatal(err)
	}
	want := `2 vendor CLIs.
PROVIDER  BINARY     VERSION  SOURCE  STATE    PATH                      ACCOUNTS     PROJECTS
plane     plane-cli  0.2.2    path    ok       /usr/local/bin/plane-cli  plane:ocral  OVERSIRE,SLING
github    gh         -        -       missing  -                         -            -
github: gh is not installed. Install gh with Homebrew: ` + "`brew install gh`." + `
`
	if out.String() != want {
		t.Fatalf("got\n%s", out)
	}
	if app.last(t).Action != "vendor.list" {
		t.Fatal(app.last(t))
	}
}

func TestVendorListOldHint(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	rows := []wire.VendorCLIRow{
		{Provider: "bigtimer", Vendor: "Big Timer", Binary: "big-timer-cli", Path: "/opt/homebrew/bin/big-timer-cli", Version: "1.0.11",
			MinVersion: "1.0.12", Source: "path", State: "old", Accounts: []string{"bigtimer:bt"}, Projects: []string{}},
		{Provider: "plane", Vendor: "Plane", Binary: "plane-cli", Path: "/usr/local/bin/plane-cli", MinVersion: "0.2.2",
			Source: "path", State: "old", Accounts: []string{"plane:ocral"}, Projects: []string{}},
	}
	b, _ := json.Marshal(rows)
	answerWith(app, wire.AppCallOK, "2 vendor CLIs.", string(b))
	if err := c.dispatch(context.Background(), []string{"vendor", "ls"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"bigtimer: big-timer-cli 1.0.11 is older than 1.0.12. Run `oversire vendor install bigtimer`.\n",
		"plane: plane-cli at /usr/local/bin/plane-cli gives no version. OverSire needs 0.2.2 or newer. Run `oversire vendor install plane`.\n",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("no line %q in\n%s", line, out)
		}
	}
}

func TestVendorListOldApp(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	rows := []wire.VendorCLIRow{
		{Provider: "plane", Vendor: "Plane", Binary: "plane-cli", Path: "/usr/local/bin/plane-cli", Accounts: []string{"plane:ocral"}, Projects: []string{}},
	}
	b, _ := json.Marshal(rows)
	answerWith(app, wire.AppCallOK, "1 vendor CLI.", string(b))
	if err := c.dispatch(context.Background(), []string{"vendor", "ls"}); err != nil {
		t.Fatal(err)
	}
	want := `1 vendor CLI.
PROVIDER  BINARY     VERSION  SOURCE  STATE  PATH                      ACCOUNTS     PROJECTS
plane     plane-cli  -        -       -      /usr/local/bin/plane-cli  plane:ocral  -
`
	if out.String() != want {
		t.Fatalf("got\n%s", out)
	}
}

func TestVendorNoAppExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}})
	for _, args := range [][]string{{"vendor", "ls"}, {"vendor", "run", "--project", "X", "--", "wi", "list"}, {"vendor", "skill", "plane"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 3 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestAppCallReadsLargeAnswer(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	big := strings.Repeat("a", 3<<20)
	answerWith(app, wire.AppCallOK, "ok.", vendorOut(t, wire.VendorRunOutput{Vendor: "gh", Account: "github:o", Stdout: big}))
	if err := c.dispatch(context.Background(), []string{"vendor", "run", "--account", "o", "--", "api", "/x"}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != len(big) {
		t.Fatal(out.Len())
	}
}

func TestSplitDash(t *testing.T) {
	flags, rest, ok := splitDash([]string{"--project", "X", "--", "a", "--", "b"})
	if !ok || !reflect.DeepEqual(flags, []string{"--project", "X"}) || !reflect.DeepEqual(rest, []string{"a", "--", "b"}) {
		t.Fatal(flags, rest, ok)
	}
	if _, _, ok := splitDash([]string{"a", "b"}); ok {
		t.Fatal("ok without --")
	}
}

func changeOut(t *testing.T, ch wire.VendorChange) string {
	t.Helper()
	b, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVendorInstallSendsVendor(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "Installed plane-cli 0.2.2 in ~/.config/oversire/vendors/plane/0.2.2.",
		changeOut(t, wire.VendorChange{Vendor: "plane", Binary: "plane-cli", Action: "installed", Version: "0.2.2"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "install", "Plane"}); err != nil {
		t.Fatal(err)
	}
	call := app.last(t)
	if call.Action != "vendor.install" || string(call.Args) != `{"vendor":"plane"}` {
		t.Fatal(call.Action, string(call.Args))
	}
	if out.String() != "Installed plane-cli 0.2.2 in ~/.config/oversire/vendors/plane/0.2.2.\n" {
		t.Fatalf("%q", out)
	}
}

func TestVendorRemoveSendsVendor(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "OverSire has no copy of plane-cli. Nothing to remove.",
		changeOut(t, wire.VendorChange{Vendor: "plane", Binary: "plane-cli", Action: "unchanged"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "remove", "plane"}); err != nil {
		t.Fatal(err)
	}
	if call := app.last(t); call.Action != "vendor.remove" || string(call.Args) != `{"vendor":"plane"}` {
		t.Fatal(call.Action, string(call.Args))
	}
	if !strings.HasPrefix(out.String(), "OverSire has no copy") {
		t.Fatal(out)
	}
}

func TestVendorInstallUsage(t *testing.T) {
	c, _, _, app := vendorCLI(t)
	for _, args := range [][]string{{"vendor", "install"}, {"vendor", "install", "plane", "bigtimer"}, {"vendor", "remove", "a", "b"}} {
		if err := c.dispatch(context.Background(), args); exitCode(err) != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	if app.count() != 0 {
		t.Fatal("a usage error makes no call")
	}
}

func TestVendorInstallFailedExit1(t *testing.T) {
	c, _, _, app := vendorCLI(t)
	msg := "The install of plane-cli goes on in the app. Run `oversire vendor ls` in one minute."
	answerWith(app, wire.AppCallFailed, msg, "")
	err := c.dispatch(context.Background(), []string{"vendor", "install", "plane"})
	if exitCode(err) != 1 || err.Error() != msg {
		t.Fatal(err)
	}
	answerWith(app, wire.AppCallNotAllowed, "OverSire does not install gh. Install it with Homebrew: `brew install gh`. gh keeps its own login.", "")
	if err := c.dispatch(context.Background(), []string{"vendor", "install", "github"}); exitCode(err) != 1 {
		t.Fatal(err)
	}
}

func TestVendorInstallJSON(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	answerWith(app, wire.AppCallOK, "Updated plane-cli from 0.2.1 to 0.2.2.",
		changeOut(t, wire.VendorChange{Vendor: "plane", Binary: "plane-cli", Action: "updated", Version: "0.2.2", Previous: "0.2.1"}))
	if err := c.dispatch(context.Background(), []string{"vendor", "install", "plane", "--json"}); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Status string            `json:"status"`
		Data   wire.VendorChange `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err, out)
	}
	if res.Status != "ok" || res.Data.Previous != "0.2.1" || res.Data.Action != "updated" {
		t.Fatalf("%+v", res)
	}
}

func TestVendorListShowsJob(t *testing.T) {
	c, out, _, app := vendorCLI(t)
	rows := []wire.VendorCLIRow{
		{Provider: "bigtimer", Vendor: "Big Timer", Binary: "big-timer-cli", MinVersion: "1.0.12", State: "missing", Job: "installing",
			Accounts: []string{"bigtimer:bt"}, Projects: []string{}},
		{Provider: "plane", Vendor: "Plane", Binary: "plane-cli", Path: "/usr/local/bin/plane-cli", Version: "0.2.2", MinVersion: "0.2.2",
			Source: "path", State: "ok", OwnVersion: "0.2.1", Accounts: []string{"plane:ocral"}, Projects: []string{}},
	}
	b, _ := json.Marshal(rows)
	answerWith(app, wire.AppCallOK, "2 vendor CLIs.", string(b))
	if err := c.dispatch(context.Background(), []string{"vendor", "ls"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"missing (installing)", "path (oversire 0.2.1 old)"} {
		if !strings.Contains(out.String(), s) {
			t.Fatalf("no %q in\n%s", s, out)
		}
	}
}
