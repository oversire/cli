package ci

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

const (
	defaultLines = 200
	maxLogLines  = 2000
	// openLogKeep is how long the log of a job that runs is good.
	openLogKeep = 20 * time.Second
)

// flight is one fetch of a log. The callers of the same job wait for it.
type flight struct {
	done chan struct{}
	err  error
}

// Logs is the log cache in front of the app.
type Logs struct {
	app
	book *Book
	x    *Index
	lim  *Limiter

	mu      sync.Mutex
	flights map[string]*flight
}

func NewLogs(book *Book, x *Index, calls Caller, lim *Limiter, self func() wire.GroupMember) *Logs {
	return &Logs{app: app{calls: calls, self: self}, book: book, x: x, lim: lim, flights: map[string]*flight{}}
}

// Get gives the last lines of the log of a job. A miss, an open log older than 20 s and fresh send one ci.log call.
// Callers of the same job share the call.
func (l *Logs) Get(ctx context.Context, repo, jobID string, lines int, fresh bool) (wire.CILogView, error) {
	st, err := l.x.Repo(repo)
	if err != nil {
		return wire.CILogView{}, err
	}
	if !st.Has("logs") {
		return wire.CILogView{}, capabilityError{ci: st.CI, capability: "logs"}
	}
	repo = st.Repo
	cached, ok, err := l.book.Log(repo, jobID)
	if err != nil {
		return wire.CILogView{}, err
	}
	hit := ok && !fresh && (cached.Complete || l.x.now().Sub(cached.Fetched) < openLogKeep)
	if !hit {
		if cached, err = l.fetch(ctx, repo, jobID, fresh); err != nil {
			return wire.CILogView{}, err
		}
	}
	return l.view(cached, lines, hit), nil
}

func (l *Logs) view(c CachedLog, lines int, cached bool) wire.CILogView {
	all := c.Lines
	truncated := len(all) > lines
	if truncated {
		all = all[len(all)-lines:]
	}
	if all == nil {
		all = []string{}
	}
	return wire.CILogView{Repo: c.Repo, JobID: c.Job, Run: c.Run, Lines: all, Complete: c.Complete, Truncated: truncated, Cached: cached,
		CIFreshness: l.x.Freshness(c.Repo)}
}

// fetch asks the app for the log. One call serves all callers of the job at the same time.
func (l *Logs) fetch(ctx context.Context, repo, jobID string, fresh bool) (CachedLog, error) {
	key := repo + "/" + jobID
	l.mu.Lock()
	f, running := l.flights[key]
	if !running {
		f = &flight{done: make(chan struct{})}
		l.flights[key] = f
	}
	l.mu.Unlock()
	if !running {
		// The call goes on if this caller leaves, for the callers that wait.
		go l.run(context.WithoutCancel(ctx), key, f, repo, jobID, fresh)
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		return CachedLog{}, ctx.Err()
	}
	if f.err != nil {
		return CachedLog{}, f.err
	}
	c, _, err := l.book.Log(repo, jobID)
	return c, err
}

func (l *Logs) run(ctx context.Context, key string, f *flight, repo, jobID string, fresh bool) {
	defer func() {
		l.mu.Lock()
		delete(l.flights, key)
		l.mu.Unlock()
		close(f.done)
	}()
	lim := "log:" + key
	if fresh {
		if retry, ok := l.lim.Allow(lim); !ok {
			f.err = tooSoonError{retry: retry, window: l.lim.Window(), what: "log"}
			return
		}
	}
	data, err := l.call(ctx, "ci.log", map[string]any{"repo": repo, "jobId": jobID, "lines": maxLogLines})
	if err != nil {
		l.lim.Forget(lim)
		f.err = err
		return
	}
	var v wire.CILogView
	if err := json.Unmarshal(data, &v); err != nil {
		f.err = &appError{status: wire.AppCallFailed, summary: "The OverSire app sent no log for job " + jobID + "."}
		return
	}
	info, _ := l.x.Job(repo, jobID)
	run := v.Run
	if run == "" {
		run = info.Run
	}
	f.err = l.book.PutLog(CachedLog{Repo: repo, Job: jobID, Run: run, Lines: v.Lines, Complete: v.Complete || info.Completed, Fetched: l.x.now()})
}
