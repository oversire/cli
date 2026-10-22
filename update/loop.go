package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	firstCheck = time.Minute
	every      = 6 * time.Hour
	timeout    = 5 * time.Minute
)

// Checker installs a newer release (Updater).
type Checker interface {
	Check(ctx context.Context) (Result, error)
}

// Loop checks for a newer CLI on a timer, on a hub hint and on a request. It keeps the result of the last check.
type Loop struct {
	u     Checker
	clock func() time.Time
	Log   *slog.Logger

	hints     chan struct{}
	installed chan string

	run    sync.Mutex // one check at a time
	mu     sync.Mutex
	status *wire.UpdateStatus
}

func NewLoop(u Checker, clock func() time.Time) *Loop {
	if clock == nil {
		clock = time.Now
	}
	return &Loop{u: u, clock: clock, Log: slog.Default(), hints: make(chan struct{}, 1), installed: make(chan string, 1)}
}

// Installed gets the version after an install. The daemon then restarts.
func (l *Loop) Installed() <-chan string { return l.installed }

// Hint asks for a check now.
func (l *Loop) Hint() {
	select {
	case l.hints <- struct{}{}:
	default:
	}
}

// Status gives the last check. nil before the first check.
func (l *Loop) Status() *wire.UpdateStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status == nil {
		return nil
	}
	s := *l.status
	return &s
}

// Run checks one minute after the start, then each 6 hours and on each hint.
func (l *Loop) Run(ctx context.Context) {
	timer := time.NewTimer(firstCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(every)
		case <-l.hints:
		}
		s, err := l.Check(ctx)
		if errors.Is(err, ErrDev) {
			return
		}
		if s.Installed != "" {
			l.done(s.Installed)
			return
		}
	}
}

// Check runs one check now. The caller sends Installed to done after it answers its client.
func (l *Loop) Check(ctx context.Context) (wire.UpdateStatus, error) {
	l.run.Lock()
	defer l.run.Unlock()
	r, err := l.u.Check(ctx)
	if errors.Is(err, ErrDev) {
		return wire.UpdateStatus{CheckedAt: l.clock(), Error: err.Error()}, err
	}
	s := wire.UpdateStatus{CheckedAt: l.clock(), Latest: r.Latest, Installed: r.Installed}
	if err != nil {
		s.Error = err.Error()
		l.Log.Warn("update check failed", "err", err)
	}
	l.mu.Lock()
	l.status = &s
	l.mu.Unlock()
	return s, err
}

func (l *Loop) done(version string) {
	select {
	case l.installed <- version:
	default:
	}
}

// Routes: POST /v1/update checks now and answers the UpdateStatus. After an install, the daemon restarts.
func (l *Loop) Routes() []server.Route {
	return []server.Route{{Pattern: "POST /v1/update", Access: server.Member, Handle: func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
		defer cancel()
		s, _ := l.Check(ctx)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
		if s.Installed != "" {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			l.done(s.Installed)
		}
	}}}
}
