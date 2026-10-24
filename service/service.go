// Package service installs `oversire daemon run` as a per-user service:
// a LaunchAgent on macOS, a systemd user unit on Linux, a logon task on Windows.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Names are the names of one service install.
type Names struct {
	Label string // LaunchAgent
	Unit  string // systemd user unit
	Task  string // Windows logon task
	Log   string // log file in ~/Library/Logs
}

var (
	Current = Names{Label: "com.oversire.daemon", Unit: "oversire-daemon.service", Task: "OverSireDaemon", Log: "oversire-daemon.log"}
	Legacy  = Names{Label: "com.oversire.runner", Unit: "oversire-runner.service", Task: "OverSireRunner", Log: "oversire-runner.log"}
)

// Runner runs one child process.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

type Manager struct {
	names Names
	goos  string
	home  string
	uid   int
	exe   string
	run   Runner
	sleep func(time.Duration)
}

func NewManager(goos, home string, uid int, exe string, run Runner) *Manager {
	return &Manager{names: Current, goos: goos, home: home, uid: uid, exe: exe, run: run, sleep: time.Sleep}
}

// Legacy is a manager for the oversire-runner install, to find and remove it.
func (m *Manager) Legacy() *Manager {
	l := *m
	l.names = Legacy
	return &l
}

type Status struct {
	Installed bool `json:"installed"`
	Running   bool `json:"running"`
}

func (m *Manager) PlistPath() string {
	return filepath.Join(m.home, "Library", "LaunchAgents", m.names.Label+".plist")
}

func (m *Manager) LogPath() string {
	return filepath.Join(m.home, "Library", "Logs", m.names.Log)
}

func (m *Manager) UnitPath() string {
	return filepath.Join(m.home, ".config", "systemd", "user", m.names.Unit)
}

func (m *Manager) domain() string { return fmt.Sprintf("gui/%d", m.uid) }

// Install writes the service file, then loads and starts the service. It replaces an old install.
func (m *Manager) Install(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		if err := writeFile(m.PlistPath(), LaunchdPlist(m.names.Label, m.exe, m.LogPath())); err != nil {
			return err
		}
		_ = os.MkdirAll(filepath.Dir(m.LogPath()), 0o755)
		_, _, _ = m.run.Run(ctx, "launchctl", []string{"bootout", m.domain() + "/" + m.names.Label}, nil)
		// launchd can still remove the old job after bootout returns. Then bootstrap fails with "5: Input/output error".
		var err error
		for range 10 {
			if err = m.do(ctx, "launchctl", "bootstrap", m.domain(), m.PlistPath()); err == nil {
				return nil
			}
			m.sleep(500 * time.Millisecond)
		}
		return err
	case "linux":
		if err := writeFile(m.UnitPath(), SystemdUnit(m.exe)); err != nil {
			return err
		}
		if err := m.do(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := m.do(ctx, "systemctl", "--user", "enable", "--now", m.names.Unit); err != nil {
			return err
		}
		return m.do(ctx, "systemctl", "--user", "restart", m.names.Unit)
	case "windows":
		if err := m.do(ctx, "schtasks", SchtasksCreateArgs(m.names.Task, m.exe)...); err != nil {
			return err
		}
		return m.do(ctx, "schtasks", "/Run", "/TN", m.names.Task)
	}
	return fmt.Errorf("service: %s is not supported", m.goos)
}

func (m *Manager) Uninstall(ctx context.Context) error {
	switch m.goos {
	case "darwin":
		_, _, _ = m.run.Run(ctx, "launchctl", []string{"bootout", m.domain() + "/" + m.names.Label}, nil)
		return removeFile(m.PlistPath())
	case "linux":
		_, _, _ = m.run.Run(ctx, "systemctl", []string{"--user", "disable", "--now", m.names.Unit}, nil)
		if err := removeFile(m.UnitPath()); err != nil {
			return err
		}
		return m.do(ctx, "systemctl", "--user", "daemon-reload")
	case "windows":
		_, _, _ = m.run.Run(ctx, "schtasks", []string{"/End", "/TN", m.names.Task}, nil)
		return m.do(ctx, "schtasks", "/Delete", "/TN", m.names.Task, "/F")
	}
	return fmt.Errorf("service: %s is not supported", m.goos)
}

// Restart starts the service again when it is installed, so that it reads a new pairing.
func (m *Manager) Restart(ctx context.Context) error {
	if !m.Status(ctx).Installed {
		return nil
	}
	switch m.goos {
	case "darwin":
		return m.do(ctx, "launchctl", "kickstart", "-k", m.domain()+"/"+m.names.Label)
	case "linux":
		return m.do(ctx, "systemctl", "--user", "restart", m.names.Unit)
	case "windows":
		_, _, _ = m.run.Run(ctx, "schtasks", []string{"/End", "/TN", m.names.Task}, nil)
		return m.do(ctx, "schtasks", "/Run", "/TN", m.names.Task)
	}
	return nil
}

func (m *Manager) Status(ctx context.Context) Status {
	switch m.goos {
	case "darwin":
		_, err := os.Stat(m.PlistPath())
		out, _, perr := m.run.Run(ctx, "launchctl", []string{"print", m.domain() + "/" + m.names.Label}, nil)
		return Status{Installed: err == nil, Running: perr == nil && strings.Contains(string(out), "state = running")}
	case "linux":
		_, err := os.Stat(m.UnitPath())
		out, _, _ := m.run.Run(ctx, "systemctl", []string{"--user", "is-active", m.names.Unit}, nil)
		return Status{Installed: err == nil, Running: strings.TrimSpace(string(out)) == "active"}
	case "windows":
		out, _, err := m.run.Run(ctx, "schtasks", []string{"/Query", "/TN", m.names.Task, "/FO", "LIST"}, nil)
		return Status{Installed: err == nil, Running: err == nil && strings.Contains(string(out), "Running")}
	}
	return Status{}
}

func (m *Manager) do(ctx context.Context, exe string, args ...string) error {
	_, stderr, err := m.run.Run(ctx, exe, args, nil)
	if err == nil {
		return nil
	}
	if t := strings.TrimSpace(string(stderr)); t != "" {
		return fmt.Errorf("%s %s: %s", exe, args[0], t)
	}
	return fmt.Errorf("%s %s: %w", exe, args[0], err)
}

// LaunchdPlist restarts the daemon after a failed exit, at most each 30 s.
// The process type is Standard: macOS throttles a Background job and its children, and the Orca CLI then times out.
func LaunchdPlist(label, exe, logPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Standard</string>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(logPath) + `</string>
</dict>
</plist>
`
}

func SystemdUnit(exe string) string {
	return `[Unit]
Description=OverSire daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart="` + exe + `" daemon run
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`
}

func SchtasksCreateArgs(task, exe string) []string {
	return []string{"/Create", "/SC", "ONLOGON", "/TN", task, "/TR", `"` + exe + `" daemon run`, "/F"}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
