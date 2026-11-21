package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

// ciDaemon serves the CI routes. It calls h for each request and records the queries.
type ciDaemon struct {
	mu      sync.Mutex
	queries []map[string][]string
	h       func(n int, w http.ResponseWriter, r *http.Request)
}

func (d *ciDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.queries = append(d.queries, r.URL.Query())
	n := len(d.queries)
	d.mu.Unlock()
	d.h(n, w, r)
}

func (d *ciDaemon) query(i int) map[string][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.queries[i]
}

func (d *ciDaemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queries)
}

func gitFake(remote, head string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case "remote":
			return remote, nil
		case "rev-parse":
			return head, nil
		}
		return "", errors.New("unexpected git call")
	}
}

func rollupOf(state wire.CIRollupState, done bool, rev string) wire.CIRollup {
	return wire.CIRollup{Repo: "o/r", Ref: "abc", SHA: "abc1234", State: state, Done: done, Rev: rev, Runs: []wire.CIRunView{},
		Checks: []wire.CICheck{}, CIFreshness: wire.CIFreshness{AsOf: time.Now(), Source: wire.CISourceWebhook}}
}

func failedRollup() wire.CIRollup {
	r := rollupOf(wire.CIFailure, true, "r1")
	r.Runs = []wire.CIRunView{{ID: "7", Workflow: "CI", Status: "completed", Conclusion: "failure", URL: "https://x/runs/7",
		Failures: []wire.CIFailureView{{Job: "build", JobID: "99", Step: "Run tests", Messages: []string{"exit code 1"},
			LogTail: []string{"FAIL TestX"}, URL: "https://x/job/99"}}}}
	return r
}

func writeJSON(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

func TestParseRemote(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"git@github.com:o/r.git", "o/r", true},
		{"https://github.com/o/r", "o/r", true},
		{"https://github.com/o/r.git\n", "o/r", true},
		{"ssh://git@github.com/o/r.git", "o/r", true},
		{"git@gitlab.com:g/sub/r.git", "g/sub/r", true},
		{"https://github.com/o", "", false},
		{"/srv/git/r.git", "", false},
		{"file:///srv/git/r.git", "", false},
		{"", "", false},
	} {
		got, ok := parseRemote(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %q %v", tc.in, got, ok)
		}
	}
}

func TestCIStatusDefaultsToHead(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.git = gitFake("git@github.com:o/r.git", "abc")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, rollupOf(wire.CISuccess, true, "r")) }}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"status"}); err != nil {
		t.Fatal(err)
	}
	q := d.query(0)
	if q["repo"][0] != "o/r" || q["ref"][0] != "abc" {
		t.Fatalf("query %v", q)
	}
	if !strings.Contains(out.String(), "success") {
		t.Fatalf("output %q", out.String())
	}
}

func TestCIRepoFlagMapsAccountPath(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, rollupOf(wire.CISuccess, true, "r")) }}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"status", "main", "--repo", "gh:o/r"}); err != nil {
		t.Fatal(err)
	}
	if q := d.query(0); q["repo"][0] != "o/r" || q["ref"][0] != "main" {
		t.Fatalf("query %v", q)
	}
}

func TestCIStatusPrintsStale(t *testing.T) {
	c, out, _ := testCLI(t, "")
	r := rollupOf(wire.CIRunning, false, "r")
	r.AsOf, r.Stale, r.Degraded = time.Now().Add(-12*time.Minute-time.Second), true, true
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, r) }}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"status", "abc", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "12 min old") || !strings.Contains(got, "GitHub quota is low") {
		t.Fatalf("output %q", got)
	}
}

func TestCIEventsDiff(t *testing.T) {
	run := func(job string) wire.CIRunView {
		return wire.CIRunView{ID: "7", Workflow: "CI", Status: "in_progress",
			Jobs: []wire.CIJob{{ID: "99", Name: "build", Status: "completed", Conclusion: job}}}
	}
	prev := rollupOf(wire.CIRunning, false, "r1")
	prev.Runs = []wire.CIRunView{run("")}
	prev.Runs[0].Jobs[0].Status = "in_progress"
	next := rollupOf(wire.CIFailure, false, "r2")
	next.Runs = []wire.CIRunView{run("failure")}
	ev := ciEvents(&prev, next)
	if len(ev) != 2 || ev[0].Type != "job" || ev[0].State != "failure" || ev[1].Type != "rollup" || ev[1].State != "failure" {
		t.Fatalf("events %+v", ev)
	}
	if ev := ciEvents(&next, next); len(ev) != 0 {
		t.Fatalf("same state gave %+v", ev)
	}
	if ev := ciEvents(nil, prev); len(ev) != 3 || ev[0].Type != "run" {
		t.Fatalf("first answer gave %+v", ev)
	}
}

func TestCIWaitFailureExit1(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, failedRollup()) }}
	fakeDaemon(t, c, d)
	err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	got := out.String()
	for _, want := range []string{"build", "Run tests", "https://x/job/99", "oversire ci logs 99 --repo o/r", "FAIL TestX"} {
		if !strings.Contains(got, want) {
			t.Errorf("output has no %q:\n%s", want, got)
		}
	}
}

func TestCIWaitSuccessExit0(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, rollupOf(wire.CISuccess, true, "r")) }}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
}

func TestCIWaitTimeoutExit4(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, rollupOf(wire.CIRunning, false, "r"))
	}}
	fakeDaemon(t, c, d)
	err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r", "--timeout", "1s"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 4 {
		t.Fatalf("err %v", err)
	}
}

func TestCIWaitNoRunsExit5(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, rollupOf(wire.CINone, true, "r")) }}
	fakeDaemon(t, c, d)
	err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 5 {
		t.Fatalf("err %v", err)
	}
}

func TestCIWaitNoDaemonExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	off := false
	if err := c.paths.SaveConfig(config.Config{Serve: &config.ServeConfig{Port: port, Tailscale: &off}}); err != nil {
		t.Fatal(err)
	}
	err = c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("err %v", err)
	}
}

func TestCIWaitNoAppExit3(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "The OverSire app on mbp is not open."})
	}}
	fakeDaemon(t, c, d)
	err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 || !strings.Contains(err.Error(), "mbp") {
		t.Fatalf("err %v", err)
	}
}

func TestCIWaitStaleExit3(t *testing.T) {
	old := ciStaleGrace
	ciStaleGrace = 20 * time.Millisecond
	t.Cleanup(func() { ciStaleGrace = old })
	c, _, _ := testCLI(t, "")
	r := rollupOf(wire.CIRunning, false, "r")
	r.Stale = true
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, r) }}
	fakeDaemon(t, c, d)
	err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("err %v", err)
	}
}

func TestCIWaitPassesSince(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &ciDaemon{h: func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			writeJSON(w, rollupOf(wire.CIRunning, false, "rev1"))
			return
		}
		writeJSON(w, rollupOf(wire.CISuccess, true, "rev2"))
	}}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r", "--fail-fast", "--workflow", "CI", "--workflow", "Lint"}); err != nil {
		t.Fatal(err)
	}
	if d.count() != 2 {
		t.Fatalf("%d calls", d.count())
	}
	if _, has := d.query(0)["since"]; has {
		t.Fatal("the first call has since")
	}
	q := d.query(1)
	if q["since"][0] != "rev1" || q["failFast"][0] != "1" || len(q["workflow"]) != 2 || q["timeout"][0] == "" || q["started"][0] == "" {
		t.Fatalf("second query %v", q)
	}
}

func TestCIWaitEventsPrintsLines(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, failedRollup()) }}
	fakeDaemon(t, c, d)
	_ = c.ci(context.Background(), []string{"wait", "abc", "--repo", "o/r", "--events"})
	var types []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var e ciEvent
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		types = append(types, e.Type)
	}
	if strings.Join(types, ",") != "run,rollup,failure" {
		t.Fatalf("types %v", types)
	}
}

func TestCIWhyFetchesLogTail(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failures") {
			writeJSON(w, wire.CIFailuresAnswer{Repo: "o/r", Run: "7", Failures: []wire.CIFailureView{{Job: "build", JobID: "99", Messages: []string{"boom"}}}})
			return
		}
		writeJSON(w, wire.CILogView{JobID: "99", Lines: []string{"tail line"}})
	}}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"why", "7", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "tail line") || !strings.Contains(got, "boom") {
		t.Fatalf("output %q", got)
	}
}

func TestCIUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	for _, args := range [][]string{{}, {"nope"}, {"show"}, {"wait", "--json", "--events"}} {
		err := c.ci(context.Background(), args)
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestUsageHasCI(t *testing.T) {
	for _, sub := range []string{"status", "ls", "show", "why", "logs", "workflows", "wait", "explain"} {
		if !usageHas("ci", sub) {
			t.Errorf("usage has no ci %s", sub)
		}
	}
}

func TestCIExplainPrints(t *testing.T) {
	c, out, _ := testCLI(t, "")
	var req wire.CIExplainRequest
	d := &ciDaemon{h: func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ci/runs/7/explain" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, wire.CIExplanation{Repo: "o/r", Run: "7", Job: "99", Attempt: 1, Cause: "A test timed out.", Fix: "Raise the timeout.", Confidence: "high", At: time.Now(), Source: "cache"})
	}}
	fakeDaemon(t, c, d)
	if err := c.ci(context.Background(), []string{"explain", "7", "--repo", "o/r", "--job", "99", "--force"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"Cause: A test timed out.", "Fix: Raise the timeout.", "Confidence: high", "Next: oversire ci logs 99 --repo o/r"} {
		if !strings.Contains(got, want) {
			t.Errorf("output has no %q: %q", want, got)
		}
	}
	if req.Repo != "o/r" || req.Job != "99" || !req.Force {
		t.Fatalf("request %+v", req)
	}
}

func TestCIExplainJSON(t *testing.T) {
	c, out, _ := testCLI(t, "")
	fakeDaemon(t, c, &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, wire.CIExplanation{Repo: "o/r", Run: "7", Attempt: 1, Cause: "c", Confidence: "low", At: time.Now(), Source: "ai"})
	}})
	if err := c.ci(context.Background(), []string{"explain", "7", "--repo", "o/r", "--json"}); err != nil {
		t.Fatal(err)
	}
	var e wire.CIExplanation
	if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Cause != "c" || e.Source != "ai" {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestCIExplainExitCodes(t *testing.T) {
	for status, code := range map[int]int{404: 1, 422: 1, 429: 1, 503: 3, 504: 4} {
		c, _, _ := testCLI(t, "")
		fakeDaemon(t, c, &ciDaemon{h: func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"no"}`))
		}})
		err := c.ci(context.Background(), []string{"explain", "7", "--repo", "o/r"})
		var ee *exitError
		got := 1
		if errors.As(err, &ee) {
			got = ee.code
		}
		if err == nil || got != code {
			t.Errorf("status %d: %v, want exit %d", status, err, code)
		}
	}
}
