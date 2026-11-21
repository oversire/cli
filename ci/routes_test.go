package ci

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/oversire/cli/membercall"
	"github.com/oversire/cli/wire"
)

// seeded gives an api with a tracked repo o/r and a running run on main.
func seeded(t *testing.T) *api {
	t.Helper()
	a := newAPI(t)
	a.put(state("o/r"), runsDoc("o/r", run("9001", "test", "in_progress", "", sha1, "main")))
	return a
}

func TestStatusRoute(t *testing.T) {
	a := seeded(t)
	var roll wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &roll)
	if roll.State != wire.CIRunning || roll.Done || roll.Ref != "main" || roll.SHA != sha1 || len(roll.Rev) != 12 || roll.Source != wire.CISourcePoll ||
		roll.AsOf.IsZero() || len(roll.Runs) != 1 || roll.Runs[0].DurationS != 60 {
		t.Fatalf("%+v", roll)
	}
	if len(a.calls.actions()) != 0 {
		t.Fatalf("calls: %v", a.calls.actions())
	}
	a.getJSON("/v1/ci/status?repo=o/r&ref="+sha1[:8], 200, &roll)
	if roll.SHA != sha1 {
		t.Fatalf("short SHA: %+v", roll)
	}
	if e := a.errText("/v1/ci/status?ref=main", 400); e != "Give the repo as owner/name." {
		t.Fatal(e)
	}
	if e := a.errText("/v1/ci/status?repo=o/r", 400); e != "Give the ref." {
		t.Fatal(e)
	}
}

func TestStatusUntracked(t *testing.T) {
	a := seeded(t)
	if e := a.errText("/v1/ci/status?repo=o/x&ref=main", 404); e != "OverSire does not track o/x. Add the repo in Settings › Code hosts." {
		t.Fatal(e)
	}
	if e := a.errText("/v1/ci/runs/1?repo=o/r", 404); e != "OverSire has no run 1 in o/r." {
		t.Fatal(e)
	}
}

func TestStatusNoCIData(t *testing.T) {
	a := newAPI(t)
	a.put(runsDoc("o/r"))
	if e := a.errText("/v1/ci/status?repo=o/r&ref=main", 503); e != "The OverSire app on mba sends no CI data. Update the app." {
		t.Fatal(e)
	}
}

func TestStatusStaleWhenAppClosed(t *testing.T) {
	a := seeded(t)
	a.setOnline(false)
	var roll wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &roll)
	if !roll.Stale || len(a.calls.actions()) != 0 {
		t.Fatalf("%+v %v", roll, a.calls.actions())
	}
}

func TestStatusFreshOnce(t *testing.T) {
	a := seeded(t)
	var roll wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main&fresh=1", 200, &roll)
	if got := a.calls.actions(); len(got) != 1 || got[0] != "ci.refresh" || a.calls.calls[0].From.Machine != "mbp" {
		t.Fatalf("%v", got)
	}
	code, body, h := a.get("/v1/ci/status?repo=o/r&ref=main&fresh=1")
	if code != 429 || h.Get("Retry-After") != "20" || len(a.calls.actions()) != 1 {
		t.Fatalf("%d %s %v", code, body, h)
	}
	a.clk.add(15 * time.Second)
	if e := a.errText("/v1/ci/status?repo=o/r&ref=main&fresh=1", 429); e != "Wait 5 s. OverSire fetched this repo 15 s ago." {
		t.Fatal(e)
	}
	a.clk.add(6 * time.Second)
	a.getJSON("/v1/ci/status?repo=o/r&ref=main&fresh=1", 200, &roll)
}

func TestStatusFreshAppClosedFreesTheWindow(t *testing.T) {
	a := seeded(t)
	a.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrNoSource }
	if e := a.errText("/v1/ci/status?repo=o/r&ref=main&fresh=1", 503); e != "The OverSire app on mba is not open." {
		t.Fatal(e)
	}
	a.errText("/v1/ci/status?repo=o/r&ref=main&fresh=1", 503)
}

func TestRunsLimit(t *testing.T) {
	a := newAPI(t)
	var rs []wire.CIRun
	for i := 0; i < 40; i++ {
		rs = append(rs, run(string(rune('A'+i)), "test", "completed", "success", sha1, "main"))
	}
	a.put(state("o/r"), runsDoc("o/r", rs...))
	for _, bad := range []string{"0", "201", "x"} {
		a.errText("/v1/ci/runs?repo=o/r&limit="+bad, 400)
	}
	var page wire.CIRunsPage
	a.getJSON("/v1/ci/runs?repo=o/r", 200, &page)
	if len(page.Runs) != 30 {
		t.Fatalf("default: %d", len(page.Runs))
	}
	a.getJSON("/v1/ci/runs?repo=o/r&limit=200", 200, &page)
	if len(page.Runs) != 40 {
		t.Fatalf("200: %d", len(page.Runs))
	}
	a.errText("/v1/ci/runs?repo=o/r&state=none", 400)
}

func TestRunsFilters(t *testing.T) {
	a := newAPI(t)
	a.put(state("o/r"), mdoc(wire.MirrorPRs, "o/r", wire.CIPRsDoc{PRs: []wire.CIPullRef{{Number: 5, HeadRefName: "feat", HeadSha: "ffff", URL: "u"}}}),
		runsDoc("o/r", run("1", "test", "completed", "success", sha1, "main"), run("2", "lint", "completed", "failure", sha1, "main"),
			run("3", "test", "in_progress", "", "ffff", "feat")))
	count := func(path string) int {
		var page wire.CIRunsPage
		a.getJSON(path, 200, &page)
		return len(page.Runs)
	}
	for path, want := range map[string]int{
		"/v1/ci/runs?repo=o/r&branch=main":   2,
		"/v1/ci/runs?repo=o/r&sha=ffff":      1,
		"/v1/ci/runs?repo=o/r&pr=5":          1,
		"/v1/ci/runs?repo=o/r&workflow=lint": 1,
		"/v1/ci/runs?repo=o/r&state=failure": 1,
		"/v1/ci/runs?repo=o/r&state=running": 1,
		"/v1/ci/runs?repo=o/r&branch=nope":   0,
	} {
		if got := count(path); got != want {
			t.Errorf("%s: %d, want %d", path, got, want)
		}
	}
}

func TestRunMissCallsJobs(t *testing.T) {
	a := seeded(t)
	a.calls.reply = okData(jobsBody("9001", 1, wire.CIJob{ID: "70001", Name: "go test", Status: "in_progress"}))
	var ans wire.CIRunAnswer
	a.getJSON("/v1/ci/runs/9001?repo=o/r", 200, &ans)
	if got := a.calls.actions(); len(got) != 1 || got[0] != "ci.jobs" || len(ans.Run.Jobs) != 1 || ans.Run.Jobs[0].ID != "70001" {
		t.Fatalf("%v %+v", got, ans)
	}
	// The app puts the doc in the mirror. The next read needs no call.
	a.put(jobsDoc("o/r", "9001", 1, false, wire.CIJob{ID: "70001", Name: "go test"}))
	a.getJSON("/v1/ci/runs/9001?repo=o/r", 200, &ans)
	if len(a.calls.actions()) != 1 || len(ans.Run.Jobs) != 1 {
		t.Fatalf("%v %+v", a.calls.actions(), ans)
	}
	a.errText("/v1/ci/runs/9001?repo=o/r&fresh=1", 429)
	a.clk.add(21 * time.Second)
	a.getJSON("/v1/ci/runs/9001?repo=o/r&fresh=1", 200, &ans)
	if len(a.calls.actions()) != 2 {
		t.Fatalf("fresh: %v", a.calls.actions())
	}
}

func jobsBody(run string, attempt int, jobs ...wire.CIJob) wire.CIJobsDoc {
	return wire.CIJobsDoc{Repo: "o/r", Run: run, Attempt: attempt, Status: "in_progress", HeadSha: sha1, HeadBranch: "main", Event: "push", URL: "u",
		Jobs: jobs, Failures: []wire.CIFailureView{}, FetchedAt: t0}
}

func TestRunMissAppClosed(t *testing.T) {
	a := seeded(t)
	a.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrNoSource }
	if e := a.errText("/v1/ci/runs/9001?repo=o/r", 503); e != "The OverSire app on mba is not open." {
		t.Fatal(e)
	}
}

func TestAppResultErrors(t *testing.T) {
	a := seeded(t)
	for _, c := range []struct {
		res  wire.AppCallResult
		err  error
		code int
		text string
	}{
		{wire.AppCallResult{Status: wire.AppCallFailed, Summary: "GitHub said no."}, nil, 502, "GitHub said no."},
		{wire.AppCallResult{Status: wire.AppCallNotAllowed, Summary: "Not tracked."}, nil, 422, "Not tracked."},
		{wire.AppCallResult{}, membercall.ErrBusy, 429, "mba is busy. Try again."},
		{wire.AppCallResult{Status: wire.AppCallOK, Data: []byte(`[1]`)}, nil, 502, "The OverSire app sent no jobs for run 9001."},
	} {
		c := c
		a.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return c.res, c.err }
		if e := a.errText("/v1/ci/runs/9001?repo=o/r&fresh=1", c.code); e != c.text {
			t.Errorf("%s != %s", e, c.text)
		}
		a.clk.add(time.Minute)
	}
}

func TestFailuresRoute(t *testing.T) {
	a := newAPI(t)
	r := run("8980", "test", "completed", "failure", sha1, "main")
	r.Attempt = 2
	doc := jobsDoc("o/r", "8980", 2, true, wire.CIJob{ID: "69950", Name: "go test", Status: "completed", Conclusion: "failure"})
	var body wire.CIJobsDoc
	if err := json.Unmarshal(doc.Body, &body); err != nil {
		t.Fatal(err)
	}
	body.Failures = []wire.CIFailureView{{Job: "go test", JobID: "69950", Step: "go test ./...", Messages: []string{"exit 1"}, LogTail: []string{"FAIL"}}}
	a.put(state("o/r"), runsDoc("o/r", r), mdoc(wire.MirrorCIJobs, "o/r/8980/2", body))
	var ans wire.CIFailuresAnswer
	a.getJSON("/v1/ci/runs/8980/failures?repo=o/r", 200, &ans)
	if ans.Attempt != 2 || len(ans.Failures) != 1 || ans.Failures[0].LogTail[0] != "FAIL" || len(a.calls.actions()) != 0 {
		t.Fatalf("%+v %v", ans, a.calls.actions())
	}
}

func TestFailuresMissCallsJobs(t *testing.T) {
	a := seeded(t)
	body := jobsBody("9001", 1)
	body.Failures = []wire.CIFailureView{{Job: "j", JobID: "1", Messages: []string{}}}
	a.calls.reply = okData(body)
	var ans wire.CIFailuresAnswer
	a.getJSON("/v1/ci/runs/9001/failures?repo=o/r", 200, &ans)
	if len(ans.Failures) != 1 || len(a.calls.actions()) != 1 {
		t.Fatalf("%+v", ans)
	}
}

func TestWorkflowsRoute(t *testing.T) {
	a := seeded(t)
	a.calls.reply = okData(wire.CIWorkflows{Workflows: []wire.CIWorkflow{{ID: "1", Name: "test"}}})
	var wf wire.CIWorkflows
	a.getJSON("/v1/ci/workflows?repo=o/r", 200, &wf)
	if wf.Repo != "o/r" || len(wf.Workflows) != 1 || len(a.calls.actions()) != 1 {
		t.Fatalf("%+v", wf)
	}
	a.getJSON("/v1/ci/workflows?repo=o/r", 200, &wf)
	if len(a.calls.actions()) != 1 || len(wf.Workflows) != 1 {
		t.Fatalf("cached: %+v %v", wf, a.calls.actions())
	}
	a.setOnline(false)
	a.getJSON("/v1/ci/workflows?repo=o/r", 200, &wf)
	if !wf.Stale {
		t.Fatal("the cache is not stale")
	}
}

func TestLimiterWindow(t *testing.T) {
	clk := &clock{t: t0}
	l := NewLimiter(20*time.Second, clk.now)
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("first")
	}
	clk.add(5 * time.Second)
	if retry, ok := l.Allow("a"); ok || retry != 15*time.Second {
		t.Fatalf("second: %v %v", retry, ok)
	}
	if _, ok := l.Allow("b"); !ok {
		t.Fatal("another key")
	}
	clk.add(16 * time.Second)
	if _, ok := l.Allow("a"); !ok {
		t.Fatal("after the window")
	}
	if len(l.last) != 2 {
		t.Fatalf("old keys: %v", l.last)
	}
}
