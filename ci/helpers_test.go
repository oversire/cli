package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
	src := mirror.Source{Machine: "mba", Loopback: true}
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

// fakeCaller answers the member calls of a test. It records them.
type fakeCaller struct {
	mu    sync.Mutex
	calls []wire.MemberCall
	reply func(c wire.MemberCall) (wire.AppCallResult, error)
}

func (f *fakeCaller) Do(_ context.Context, c wire.MemberCall) (wire.AppCallResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK}, nil
	}
	return reply(c)
}

func (f *fakeCaller) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Action)
	}
	return out
}

func okData(v any) func(wire.MemberCall) (wire.AppCallResult, error) {
	return func(c wire.MemberCall) (wire.AppCallResult, error) {
		b, _ := json.Marshal(v)
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Data: b}, nil
	}
}

// api is the routes of an env behind an httptest server.
type api struct {
	*env
	calls  *fakeCaller
	routes *Routes
	logs   *Logs
	srv    *httptest.Server
}

func newAPI(t *testing.T) *api {
	t.Helper()
	a := &api{env: newEnv(t), calls: &fakeCaller{}}
	self := func() wire.GroupMember { return wire.GroupMember{Machine: "mbp"} }
	lim := NewLimiter(20*time.Second, a.clk.now)
	a.logs = NewLogs(a.book, a.x, a.calls, lim, self)
	a.routes = NewRoutes(a.x, a.logs, a.calls, lim, self)
	a.routes.freshWait = 20 * time.Millisecond
	a.srv = httptest.NewServer(a.mux())
	t.Cleanup(a.srv.Close)
	return a
}

func (a *api) mux() http.Handler {
	mux := http.NewServeMux()
	for _, r := range a.routes.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	return mux
}

func (a *api) get(path string) (int, []byte, http.Header) {
	a.t.Helper()
	resp, err := http.Get(a.srv.URL + path)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

// getJSON expects status want and decodes the body into v.
func (a *api) getJSON(path string, want int, v any) {
	a.t.Helper()
	code, b, _ := a.get(path)
	if code != want {
		a.t.Fatalf("GET %s: %d %s, want %d", path, code, b, want)
	}
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			a.t.Fatalf("GET %s: %v: %s", path, err, b)
		}
	}
}

func (a *api) errText(path string, want int) string {
	a.t.Helper()
	var e struct{ Error string }
	a.getJSON(path, want, &e)
	return e.Error
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
