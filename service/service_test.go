package service

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

type record struct{ calls [][]string }

func (r *record) Run(_ context.Context, exe string, args []string, _ []byte) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string{exe}, args...))
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
	p := LaunchdPlist("/opt/o&r/oversire-runner", "/Users/ada/Library/Logs/oversire-runner.log")
	for _, want := range []string{
		"<string>com.oversire.runner</string>",
		"<string>/opt/o&amp;r/oversire-runner</string>\n\t\t<string>run</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<string>/Users/ada/Library/Logs/oversire-runner.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist has no %q", want)
		}
	}
}

func TestSystemdUnit(t *testing.T) {
	u := SystemdUnit("/home/ada/bin/oversire-runner")
	for _, want := range []string{`ExecStart="/home/ada/bin/oversire-runner" run`, "Restart=on-failure", "RestartSec=30", "WantedBy=default.target"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit has no %q", want)
		}
	}
}

func TestSchtasksArgs(t *testing.T) {
	got := SchtasksCreateArgs(`C:\Program Files\oversire-runner.exe`)
	want := []string{"/Create", "/SC", "ONLOGON", "/TN", "OverSireRunner", "/TR", `"C:\Program Files\oversire-runner.exe" run`, "/F"}
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
	if !r.has("launchctl", "bootout", "gui/501/com.oversire.runner") {
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
	if !r.has("systemctl", "--user", "daemon-reload") || !r.has("systemctl", "--user", "enable", "--now", "oversire-runner.service") {
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
	if !r.has(append([]string{"schtasks"}, SchtasksCreateArgs(`C:\o\oversire-runner.exe`)...)...) {
		t.Fatalf("calls %q", r.calls)
	}
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.has("schtasks", "/Delete", "/TN", "OverSireRunner", "/F") {
		t.Fatalf("calls %q", r.calls)
	}
}
