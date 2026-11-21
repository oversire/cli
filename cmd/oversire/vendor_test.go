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
github: gh is not installed. Run ` + "`oversire vendor install github`." + `
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
	for _, args := range [][]string{{"vendor", "ls"}, {"vendor", "run", "--project", "X", "--", "wi", "list"}} {
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
