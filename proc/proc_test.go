package proc

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggedWritesAFailureOnce(t *testing.T) {
	unix(t)
	var buf bytes.Buffer
	l := NewLogged(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	up := filepath.Join(t.TempDir(), "up")
	cmd := []string{"-c", "test -e " + up + " || { echo no tailscale >&2; exit 3; }"}
	run := func() { _, _, _ = l.Run(context.Background(), "/bin/sh", cmd, []byte("secret")) }
	run()
	run()
	if err := os.WriteFile(up, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	if err := os.Remove(up); err != nil {
		t.Fatal(err)
	}
	run()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var levels []string
	for _, ln := range lines {
		levels = append(levels, strings.Fields(ln)[1])
	}
	if got := strings.Join(levels, " "); got != "level=WARN level=DEBUG level=DEBUG level=WARN" {
		t.Fatalf("levels %q\n%s", got, buf.String())
	}
	if !strings.Contains(lines[0], "exe=sh") || !strings.Contains(lines[0], "exit=3") || !strings.Contains(lines[0], `stderr="no tailscale"`) {
		t.Fatal(lines[0])
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatal("stdin is in the log")
	}
}

func TestLoggedNoExe(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogged(slog.New(slog.NewTextHandler(&buf, nil)))
	if _, _, err := l.Run(context.Background(), "/no/such/exe", nil, nil); err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "err=") {
		t.Fatal(buf.String())
	}
}

func TestShortArgs(t *testing.T) {
	args := make([]string, 14)
	for i := range args {
		args[i] = "a"
	}
	args[0] = strings.Repeat("p", 200)
	got := short(args)
	if !strings.HasPrefix(got, strings.Repeat("p", 79)+"… a") || !strings.HasSuffix(got, " a …") || strings.Count(got, " a") != 11 {
		t.Fatal(got)
	}
}

func TestLoggedKilledHasTheSignal(t *testing.T) {
	unix(t)
	var buf bytes.Buffer
	l := NewLogged(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := l.Run(ctx, "/bin/sleep", []string{"5"}, nil); err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(buf.String(), `err="signal: killed"`) || strings.Contains(buf.String(), "exit=") {
		t.Fatal(buf.String())
	}
}
