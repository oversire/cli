package ci

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeSessions struct {
	mu sync.Mutex
	m  map[string]wire.DaemonSession
}

func (f *fakeSessions) Get(id string) (wire.DaemonSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[id]
	return s, ok
}

func (f *fakeSessions) set(s wire.DaemonSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[s.ID] = s
}

type fakeSender struct {
	mu    sync.Mutex
	texts []string
	to    []string
	err   error
}

func (f *fakeSender) CanSend(s wire.DaemonSession) bool { return s.Runtime != wire.RuntimeCLI }

func (f *fakeSender) Deliver(_ context.Context, s wire.DaemonSession, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.texts, f.to = append(f.texts, text), append(f.to, s.ID)
	return nil
}

func (f *fakeSender) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

type fakeWaiter struct {
	mu    sync.Mutex
	calls []WaitQuery
	reply func(n int, q WaitQuery) (wire.CIRollup, error)
}

func (f *fakeWaiter) Wait(ctx context.Context, q WaitQuery) (wire.CIRollup, error) {
	f.mu.Lock()
	f.calls = append(f.calls, q)
	n := len(f.calls)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return wire.CIRollup{}, err
	}
	return f.reply(n, q)
}

type fakeAdder struct {
	mu   sync.Mutex
	reqs []wire.CIWatchRequest
	ans  wire.CIWatchAnswer
	err  error
}

func (f *fakeAdder) AddWatch(_ context.Context, req wire.CIWatchRequest) (wire.CIWatchAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.ans, f.err
}

func (f *fakeAdder) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeAdder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

type notifyEnv struct {
	n        *Notifier
	sessions *fakeSessions
	sender   *fakeSender
	waiter   *fakeWaiter
	adder    *fakeAdder
	clk      *clock
	dir      string
}

func failedRollup() wire.CIRollup {
	return wire.CIRollup{Repo: "o/r", Ref: "main", SHA: sha1, State: wire.CIFailure, Done: true, Rev: "r1", Checks: []wire.CICheck{},
		Runs: []wire.CIRunView{{ID: "7", Workflow: "CI", Name: "CI", Status: "completed", Conclusion: "failure",
			Failures: []wire.CIFailureView{{Job: "build", JobID: "99", Step: "Run tests", Messages: []string{"exit code 1"}, LogTail: []string{"FAIL TestX", ""}}}}}}
}

func newNotifyEnv(t *testing.T, reply func(n int, q WaitQuery) (wire.CIRollup, error)) *notifyEnv {
	t.Helper()
	e := &notifyEnv{
		sessions: &fakeSessions{m: map[string]wire.DaemonSession{
			"s1":    {ID: "s1", Runtime: wire.RuntimeOversire, State: wire.PhaseWorking},
			"t1:l1": {ID: "t1:l1", Runtime: wire.RuntimeOrca, State: wire.PhaseWaiting, Terminal: "term1"},
			"c1":    {ID: "c1", Runtime: wire.RuntimeCLI},
		}},
		sender: &fakeSender{}, waiter: &fakeWaiter{reply: reply}, adder: &fakeAdder{}, clk: &clock{t: t0}, dir: t.TempDir(),
	}
	e.n = NewNotifier(e.sessions, e.sender, e.waiter, e.adder, e.dir, e.clk.now)
	e.n.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.n.idleStep, e.n.retry, e.n.floor = time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { e.n.cancel(); e.n.wg.Wait() })
	return e
}

func (e *notifyEnv) req(session string) wire.CIWatchRequest {
	return wire.CIWatchRequest{Repo: "o/r", Ref: "main", TTL: 600, Notify: &wire.CINotifyTarget{Session: session}}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timeout: " + what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNotifierSendsOnFail(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { return failedRollup(), nil })
	ans, err := e.n.Start(context.Background(), e.req("s1"))
	if err != nil || ans.Notify != "queued" || ans.Done {
		t.Fatalf("%v %+v", err, ans)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
	want := "[OverSire] CI of 9d1e07a (o/r) failed. CI › build › Run tests: exit code 1 Run: oversire ci why 7"
	if got := e.sender.sent()[0]; got != want || e.sender.to[0] != "s1" {
		t.Fatalf("%q", got)
	}
	eventually(t, "the job to end", func() bool { return len(e.n.List()) == 0 })
	if b, _ := os.ReadFile(filepath.Join(e.dir, "notify.json")); strings.Contains(string(b), "s1") {
		t.Fatalf("notify.json: %s", b)
	}
	q := e.waiter.calls[0]
	if q.Repo != "o/r" || q.Ref != "main" || q.Timeout != 300*time.Second || !q.Started.Equal(t0) {
		t.Fatalf("%+v", q)
	}
}

func TestNotifierPassText(t *testing.T) {
	r := failedRollup()
	r.State = wire.CISuccess
	r.Runs = []wire.CIRunView{{ID: "7", DurationS: 252, Conclusion: "success"}, {ID: "8", DurationS: 60, Conclusion: "success"}}
	if got, want := resultText(r, "main"), "[OverSire] CI of 9d1e07a (o/r) passed. 2 runs in 4 min 12 s."; got != want {
		t.Fatalf("%q", got)
	}
	r.State = wire.CINone
	if got := resultText(r, "main"); !strings.Contains(got, "started no run") {
		t.Fatalf("%q", got)
	}
}

func TestNotifierTextHasNoControlBytes(t *testing.T) {
	r := failedRollup()
	r.Runs[0].Failures[0].Messages = []string{"line one\nline two\x1b[31m\ttab"}
	got := resultText(r, "main")
	for _, c := range got {
		if c < 0x20 || c == 0x7f {
			t.Fatalf("control byte %q in %q", c, got)
		}
	}
}

func TestNotifierDoneAtOnce(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { t.Error("wait ran"); return wire.CIRollup{}, nil })
	r := failedRollup()
	e.adder.ans = wire.CIWatchAnswer{Done: true, Rollup: &r}
	ans, err := e.n.Start(context.Background(), e.req("s1"))
	if err != nil || !ans.Done || ans.Notify != "done" || ans.Rollup == nil || len(e.sender.sent()) != 0 || len(e.n.List()) != 0 {
		t.Fatalf("%v %+v", err, ans)
	}
}

func TestNotifierCannotSend(t *testing.T) {
	e := newNotifyEnv(t, nil)
	if _, err := e.n.Start(context.Background(), e.req("c1")); !errors.Is(err, ErrCannotSend) {
		t.Fatalf("%v", err)
	}
	if _, err := e.n.Start(context.Background(), e.req("nope")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("%v", err)
	}
	if _, err := e.n.Start(context.Background(), wire.CIWatchRequest{Repo: "o/r", Ref: "main"}); err == nil || !strings.Contains(err.Error(), "notify") {
		t.Fatalf("%v", err)
	}
	if e.adder.count() != 0 {
		t.Fatal("a refused call added a watch")
	}
}

func TestNotifierFindsPaneKey(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { return failedRollup(), nil })
	req := e.req("")
	req.Notify = &wire.CINotifyTarget{Session: "unknown", PaneKey: "t1:l1"}
	if _, err := e.n.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
	if e.sender.to[0] != "t1:l1" {
		t.Fatalf("%v", e.sender.to)
	}
}

func TestNotifierWaitsForOrcaIdle(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { return failedRollup(), nil })
	e.sessions.set(wire.DaemonSession{ID: "t1:l1", Runtime: wire.RuntimeOrca, State: wire.PhaseWorking, Terminal: "term1"})
	req := e.req("t1:l1")
	if _, err := e.n.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the wait call", func() bool { e.waiter.mu.Lock(); defer e.waiter.mu.Unlock(); return len(e.waiter.calls) > 0 })
	time.Sleep(60 * time.Millisecond)
	if len(e.sender.sent()) != 0 {
		t.Fatal("the text went into a working session")
	}
	e.sessions.set(wire.DaemonSession{ID: "t1:l1", Runtime: wire.RuntimeOrca, State: wire.PhaseWaiting, Terminal: "term1"})
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
}

func TestNotifierSendsAfterIdleMax(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { return failedRollup(), nil })
	e.n.idleMax = 30 * time.Millisecond
	e.sessions.set(wire.DaemonSession{ID: "t1:l1", Runtime: wire.RuntimeOrca, State: wire.PhaseWorking, Terminal: "term1"})
	e.n.now = time.Now
	if _, err := e.n.Start(context.Background(), e.req("t1:l1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
}

func TestNotifierTimeout(t *testing.T) {
	var e *notifyEnv
	e = newNotifyEnv(t, func(n int, _ WaitQuery) (wire.CIRollup, error) {
		e.clk.add(31 * time.Second)
		return wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"}, nil
	})
	req := e.req("s1")
	req.TTL = 60
	if _, err := e.n.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
	if got, want := e.sender.sent()[0], "[OverSire] CI of main (o/r) did not end in 1 min. Run: oversire ci status main"; got != want {
		t.Fatalf("%q", got)
	}
	// Each round after the first extends the watch and sends the last rev.
	if e.adder.count() < 2 || e.adder.reqs[1].TTL != 60 || e.waiter.calls[1].Since != "r1" {
		t.Fatalf("adds %d, %+v", e.adder.count(), e.waiter.calls)
	}
}

func TestNotifierRetriesAfterError(t *testing.T) {
	e := newNotifyEnv(t, func(n int, _ WaitQuery) (wire.CIRollup, error) {
		if n < 3 {
			return wire.CIRollup{}, &HomeError{Text: "mbp (home) is offline."}
		}
		return failedRollup(), nil
	})
	if _, err := e.n.Start(context.Background(), e.req("s1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
}

func TestNotifierResume(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) { return failedRollup(), nil })
	live := NotifyJob{Key: "s1|o/r|main", Session: "s1", Request: e.req("s1"), CreatedAt: t0.Add(-time.Minute), ExpiresAt: t0.Add(time.Hour)}
	dead := NotifyJob{Key: "s1|o/r|old", Session: "s1", Request: e.req("s1"), CreatedAt: t0.Add(-time.Hour), ExpiresAt: t0.Add(-time.Minute)}
	b, _ := json.Marshal(map[string]any{"jobs": []NotifyJob{live, dead}})
	if err := os.WriteFile(filepath.Join(e.dir, "notify.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.n.Resume(ctx) }()
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
	eventually(t, "the file", func() bool {
		b, _ := os.ReadFile(filepath.Join(e.dir, "notify.json"))
		return !strings.Contains(string(b), "s1")
	})
	time.Sleep(30 * time.Millisecond)
	if len(e.sender.sent()) != 1 {
		t.Fatalf("%v", e.sender.sent())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNotifierStopKeepsJob(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) {
		return wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"}, nil
	})
	if _, err := e.n.Start(context.Background(), e.req("s1")); err != nil {
		t.Fatal(err)
	}
	e.n.cancel()
	e.n.wg.Wait()
	if b, _ := os.ReadFile(filepath.Join(e.dir, "notify.json")); !strings.Contains(string(b), `"session": "s1"`) {
		t.Fatalf("notify.json: %s", b)
	}
	if len(e.sender.sent()) != 0 {
		t.Fatal("a stop sent text")
	}
}

func TestNotifierOneJobPerSession(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) {
		return wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"}, nil
	})
	for i := 0; i < 2; i++ {
		if ans, err := e.n.Start(context.Background(), e.req("s1")); err != nil || ans.Notify != "queued" {
			t.Fatalf("%v %+v", err, ans)
		}
	}
	if len(e.n.List()) != 1 || e.adder.count() != 1 {
		t.Fatalf("jobs %d adds %d", len(e.n.List()), e.adder.count())
	}
	other := e.req("s1")
	other.Ref = "dev"
	if _, err := e.n.Start(context.Background(), other); err != nil || len(e.n.List()) != 2 {
		t.Fatalf("%v %d", err, len(e.n.List()))
	}
}

func TestNotifierFull(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) {
		return wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"}, nil
	})
	for i := 0; i < maxNotifyJobs; i++ {
		req := e.req("s1")
		req.Ref = "b" + string(rune('a'+i))
		if _, err := e.n.Start(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	req := e.req("s1")
	req.Ref = "zz"
	if _, err := e.n.Start(context.Background(), req); !errors.Is(err, ErrFull) {
		t.Fatalf("%v", err)
	}
}

func TestNotifierRoutes(t *testing.T) {
	e := newNotifyEnv(t, func(int, WaitQuery) (wire.CIRollup, error) {
		return wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"}, nil
	})
	routes := e.n.Routes()
	mux := http.NewServeMux()
	for _, r := range routes {
		if r.Access != server.LoopbackOnly {
			t.Fatalf("%s: access %v", r.Pattern, r.Access)
		}
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	post := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/v1/ci/notify", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(`{"repo":"o/r","ref":"main","notify":{"session":"s1"}}`); code != 200 || !strings.Contains(body, `"notify":"queued"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := post(`{"repo":"o/r","ref":"main","notify":{"session":"c1"}}`); code != 422 || !strings.Contains(body, "without --notify") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := post(`{"repo":"o/r","ref":"main","notify":{"session":"zz"}}`); code != 422 {
		t.Fatalf("%d", code)
	}
	if code, _ := post(`{"repo":"o/r","ref":"main"}`); code != 400 {
		t.Fatalf("%d", code)
	}
	e.adder.fail(&HomeError{Status: 404, Text: "OverSire does not track o/r."})
	if code, body := post(`{"repo":"o/r","ref":"dev","notify":{"session":"s1"}}`); code != 404 || !strings.Contains(body, "does not track") {
		t.Fatalf("%d %s", code, body)
	}
	resp, err := http.Get(srv.URL + "/v1/ci/notify")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jobs []NotifyJob
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil || len(jobs) != 1 || jobs[0].Session != "s1" {
		t.Fatalf("%v %+v", err, jobs)
	}
}

func TestNotifierUnknownRun(t *testing.T) {
	e := newNotifyEnv(t, func(n int, _ WaitQuery) (wire.CIRollup, error) {
		if n == 1 {
			return wire.CIRollup{Repo: "o/r", Ref: "9002", State: wire.CINone, Rev: "r0"}, nil
		}
		return wire.CIRollup{}, &HomeError{Status: http.StatusNotFound, Text: "OverSire has no run 9002 in o/r."}
	})
	e.adder.ans = wire.CIWatchAnswer{Rollup: &wire.CIRollup{Repo: "o/r", Ref: "9002", State: wire.CINone}}
	req := e.req("s1")
	req.Ref = "9002"
	ans, err := e.n.Start(context.Background(), req)
	if err != nil || ans.Notify != "queued" || ans.Watch != nil {
		t.Fatalf("%v %+v", err, ans)
	}
	eventually(t, "the text", func() bool { return len(e.sender.sent()) == 1 })
	if got, want := e.sender.sent()[0], "[OverSire] OverSire has no run 9002 in o/r. Run: oversire ci status 9002"; got != want {
		t.Fatalf("%q", got)
	}
	eventually(t, "the job to end", func() bool { return len(e.n.List()) == 0 })
}
