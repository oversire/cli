package service

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type record struct {
	calls     [][]string
	bootFails int // bootstrap fails this many times
}

func (r *record) Run(_ context.Context, exe string, args []string, _ []byte) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string{exe}, args...))
	if exe == "launchctl" && args[0] == "bootstrap" && r.bootFails > 0 {
		r.bootFails--
		return nil, []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
	}
	if exe == "launchctl" && args[0] == "print" {
		return []byte("\tstate = running\n"), nil, nil
	}
	if exe == "systemctl" && args[1] == "is-active" {
		return []byte("active\n"), nil, nil
	}
	return nil, nil, nil
}

func (r *record) has(call ...string) bool {
	return slices.ContainsFunc(r.calls, func(c []string) bool { return slices.Equal(c, call) })
}

func TestLaunchdPlist(t *testing.T) {
	p := LaunchdPlist(Current.Label, "/opt/o&r/oversire", "/Users/ada/Library/Logs/oversire-daemon.log")
	for _, want := range []string{
		"<string>com.oversire.daemon</string>",
		"<string>com.oversire.daemon</string>", "<string>/opt/o&amp;r/oversire</string>\n\t\t<string>daemon</string>\n\t\t<string>run</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<string>/Users/ada/Library/Logs/oversire-daemon.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist has no %q", want)
		}
	}
}

func TestSystemdUnit(t *testing.T) {
	u := SystemdUnit("/home/ada/bin/oversire")
	for _, want := range []string{`ExecStart="/home/ada/bin/oversire" daemon run`, "Restart=on-failure", "RestartSec=30", "WantedBy=default.target"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit has no %q", want)
		}
	}
}

func TestSchtasksArgs(t *testing.T) {
	got := SchtasksCreateArgs(Current.Task, `C:\Program Files\oversire.exe`)
	want := []string{"/Create", "/SC", "ONLOGON", "/TN", "OverSireDaemon", "/TR", `"C:\Program Files\oversire.exe" daemon run`, "/F"}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
}

func TestDarwin(t *testing.T) {
	home := t.TempDir()
	r := &record{}
	m := NewManager("darwin", home, 501, "/usr/local/bin/oversire-runner", r)
	ctx := context.Background()
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.PlistPath()); err != nil {
		t.Fatal(err)
	}
	if !r.has("launchctl", "bootstrap", "gui/501", m.PlistPath()) {
		t.Fatalf("calls %q", r.calls)
	}
	if s := m.Status(ctx); !s.Installed || !s.Running {
		t.Fatal(s)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("launchctl", "bootout", "gui/501/com.oversire.daemon") {
		t.Fatalf("calls %q", r.calls)
	}
	if _, err := os.Stat(m.PlistPath()); err == nil {
		t.Fatal("plist after uninstall")
	}
}

func TestLinux(t *testing.T) {
	home := t.TempDir()
	r := &record{}
	m := NewManager("linux", home, 1000, "/usr/bin/oversire-runner", r)
	ctx := context.Background()
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("systemctl", "--user", "daemon-reload") || !r.has("systemctl", "--user", "enable", "--now", "oversire-daemon.service") {
		t.Fatalf("calls %q", r.calls)
	}
	if s := m.Status(ctx); !s.Installed || !s.Running {
		t.Fatal(s)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.UnitPath()); err == nil {
		t.Fatal("unit after uninstall")
	}
}

func TestWindows(t *testing.T) {
	r := &record{}
	m := NewManager("windows", t.TempDir(), 0, `C:\o\oversire-runner.exe`, r)
	ctx := context.Background()
	if err := m.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has(append([]string{"schtasks"}, SchtasksCreateArgs(Current.Task, `C:\o\oversire-runner.exe`)...)...) {
		t.Fatalf("calls %q", r.calls)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("schtasks", "/Delete", "/TN", "OverSireDaemon", "/F") {
		t.Fatalf("calls %q", r.calls)
	}
}

func TestDarwinBootstrapAgain(t *testing.T) {
	r := &record{bootFails: 2}
	m := NewManager("darwin", t.TempDir(), 501, "/x/oversire", r)
	var slept int
	m.sleep = func(time.Duration) { slept++ }
	if err := m.Install(context.Background()); err != nil || slept != 2 {
		t.Fatal(err, slept)
	}
	r.bootFails = 99
	if err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatal(err)
	}
}
