// Package proc runs child processes. The other packages get it as a small interface.
package proc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Local runs commands on this machine.
type Local struct{}

// Run starts exe with args, writes stdin to it and waits. stdout and stderr come back also on an error.
func (Local) Run(ctx context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// Logged runs commands like Local and writes one line for each command: debug when it works, warn when it fails.
// The same failure of the same command again is at debug, so a poll that fails each time does not fill the log.
// The line has no stdin and no stdout. A secret goes on stdin, so it stays out of the log.
type Logged struct {
	log *slog.Logger

	mu   sync.Mutex
	last map[string]string // the failure of each command line
}

func NewLogged(log *slog.Logger) *Logged {
	return &Logged{log: log, last: map[string]string{}}
}

func (l *Logged) Run(ctx context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	start := time.Now()
	out, errOut, err := Local{}.Run(ctx, exe, args, stdin)
	line := short(args)
	a := []any{"exe", filepath.Base(exe), "args", line, "ms", time.Since(start).Milliseconds()}
	key := exe + " " + line
	if err == nil {
		l.seen(key, "")
		l.log.Debug("command", a...)
		return out, errOut, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		a = append(a, "exit", exit.ExitCode())
	} else {
		a = append(a, "err", err)
	}
	a = append(a, "stderr", tail(errOut))
	if l.seen(key, err.Error()+tail(errOut)) {
		l.log.Debug("command", a...)
	} else {
		l.log.Warn("command", a...)
	}
	return out, errOut, err
}

// seen keeps the failure of the command line and tells if it is the same as the last one.
func (l *Logged) seen(key, failure string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	same := failure != "" && l.last[key] == failure
	if failure == "" {
		delete(l.last, key)
	} else {
		l.last[key] = failure
	}
	return same
}

// short cuts each argument to 80 characters and keeps at most 12 arguments.
func short(args []string) string {
	keep := args[:min(len(args), 12)]
	parts := make([]string, 0, len(keep)+1)
	for _, a := range keep {
		if len(a) > 80 {
			a = a[:79] + "…"
		}
		parts = append(parts, a)
	}
	if len(args) > len(keep) {
		parts = append(parts, "…")
	}
	return strings.Join(parts, " ")
}

// tail gives the last 300 characters of the output on one line.
func tail(b []byte) string {
	t := strings.Join(strings.Fields(string(b)), " ")
	if len(t) > 300 {
		t = "…" + t[len(t)-299:]
	}
	return t
}
