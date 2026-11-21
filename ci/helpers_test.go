package ci

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/wire"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var t0 = time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)

const sha1 = "9d1e07a2c4b85f10e3a6d7b9c1f2e4a8b0d3c5e7"

// env is a mirror store, a book and an index on them. online is the answer of the online func.
type env struct {
	t      *testing.T
	feed   *mirror.Store
	book   *Book
	x      *Index
	clk    *clock
	mu     sync.Mutex
	online bool
}

func (e *env) isOnline() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.online
}

func (e *env) setOnline(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.online = v
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, clk: &clock{t: t0}, online: true}
	var err error
	if e.feed, err = mirror.Open(filepath.Join(dir, "mirror.db"), e.clk.now); err != nil {
		t.Fatal(err)
	}
	if e.book, err = Open(filepath.Join(dir, "ci.db"), e.clk.now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.feed.Close(); e.book.Close() })
	e.x = NewIndex(e.feed, e.book, e.isOnline, e.clk.now)
	return e
}

func mdoc(kind wire.MirrorKind, id string, body any) wire.MirrorDoc {
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return wire.MirrorDoc{Kind: kind, ID: id, V: 1, At: t0, Body: b}
}

// put writes docs to the feed and reads them into the index.
func (e *env) put(docs ...wire.MirrorDoc) {
	e.t.Helper()
	src := mirror.Source{Machine: "mbp", Loopback: true}
	if _, err := e.feed.Put(wire.MirrorPut{Epoch: e.feed.Head().Epoch, Source: "app-1", SourceName: "mbp", Docs: docs}, src); err != nil {
		e.t.Fatal(err)
	}
	e.x.sync()
}

func ts(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }

func state(repos ...string) wire.MirrorDoc {
	var rs []wire.CIRepoState
	for _, r := range repos {
		rs = append(rs, wire.CIRepoState{Repo: r, CI: "GitHub Actions", Capabilities: []string{"logs"}})
	}
	return mdoc(wire.MirrorCIState, "home", wire.CIStateDoc{AsOf: t0, Source: wire.CISourcePoll, Repos: rs})
}

func run(id, workflow, status, conclusion, sha, branch string) wire.CIRun {
	return wire.CIRun{ID: id, Name: workflow, WorkflowName: workflow, Status: status, Conclusion: conclusion, HeadSha: sha, HeadBranch: branch,
		Event: "push", URL: "https://github.com/o/r/actions/runs/" + id, UpdatedAt: ts(0), CreatedAt: ts(-time.Minute), StartedAt: ts(-time.Minute),
		Number: 1, Attempt: 1}
}

func runsDoc(repo string, runs ...wire.CIRun) wire.MirrorDoc {
	return mdoc(wire.MirrorRuns, repo, wire.CIRunsDoc{Runs: runs})
}

func jobsDoc(repo, runID string, attempt int, final bool, jobs ...wire.CIJob) wire.MirrorDoc {
	return mdoc(wire.MirrorCIJobs, fmt.Sprintf("%s/%s/%d", repo, runID, attempt), wire.CIJobsDoc{Repo: repo, Run: runID, Attempt: attempt,
		Status: "completed", HeadSha: sha1, HeadBranch: "main", Event: "push", URL: "u", Jobs: jobs, Failures: []wire.CIFailureView{}, Final: final, FetchedAt: t0})
}
