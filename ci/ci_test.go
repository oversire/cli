package ci

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Ref
	}{
		{"#12", Ref{RefPR, "12", 12}},
		{"https://github.com/o/r/actions/runs/99", Ref{RefRun, "99", 0}},
		{"https://github.com/o/r/actions/runs/99/job/5", Ref{RefRun, "99", 0}},
		{"https://github.com/o/r/pull/7", Ref{RefPR, "7", 7}},
		{"123456", Ref{RefRun, "123456", 0}},
		{"9d1e07a2", Ref{RefSHA, "9d1e07a2", 0}},
		{"main", Ref{RefBranch, "main", 0}},
		{"feature/x", Ref{RefBranch, "feature/x", 0}},
	} {
		got, err := ParseRef(c.in)
		if err != nil || got != c.want {
			t.Errorf("%q: %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
	if _, err := ParseRef(" "); err == nil {
		t.Error("an empty ref gives no error")
	}
}

func TestResolvePR(t *testing.T) {
	e := newEnv(t)
	e.put(state("o/r"),
		mdoc(wire.MirrorPRs, "o/r", wire.CIPRsDoc{PRs: []wire.CIPullRef{
			{Number: 12, HeadRefName: "feat", HeadSha: "abc", URL: "u"},
			{Number: 13, HeadRefName: "other", URL: "u"}}}),
		runsDoc("o/r", run("5", "test", "completed", "success", "def", "other")))
	tg, err := e.x.Resolve("o/r", Ref{Kind: RefPR, PR: 12})
	if err != nil || tg.SHA != "abc" || tg.PR != 12 || tg.Branch != "feat" {
		t.Fatalf("%+v %v", tg, err)
	}
	tg, err = e.x.Resolve("o/r", Ref{Kind: RefPR, PR: 13})
	if err != nil || tg.SHA != "def" {
		t.Fatalf("no headSha: %+v %v", tg, err)
	}
	_, err = e.x.Resolve("o/r", Ref{Kind: RefPR, PR: 99})
	var ref *server.Refusal
	if !errors.As(err, &ref) || ref.Kind != server.NotFound {
		t.Fatalf("an unknown PR: %v", err)
	}
}

func TestResolveHexBranch(t *testing.T) {
	e := newEnv(t)
	e.put(state("o/r"), runsDoc("o/r", run("5", "test", "completed", "success", sha1, "deadbeef")))
	tg, err := e.x.Resolve("o/r", Ref{Kind: RefSHA, Value: "deadbeef"})
	if err != nil || tg.Branch != "deadbeef" || tg.SHA != sha1 {
		t.Fatalf("%+v %v", tg, err)
	}
	tg, err = e.x.Resolve("o/r", Ref{Kind: RefSHA, Value: "9d1e07a"})
	if err != nil || tg.SHA != sha1 || tg.Branch != "" {
		t.Fatalf("short SHA: %+v %v", tg, err)
	}
}

func view(workflow, status, conclusion string) wire.CIRunView {
	return wire.CIRunView{ID: workflow, Attempt: 1, Name: workflow, Workflow: workflow, Status: status, Conclusion: conclusion}
}

func TestLatestAttemptWins(t *testing.T) {
	a1, a2 := view("test", "completed", "failure"), view("test", "completed", "success")
	a1.ID, a2.ID, a2.Attempt = "7", "7", 2
	for _, s := range []runSet{{a1, a2}, {a2, a1}} {
		l := s.latest()
		if st, done := l.state(nil); len(l) != 1 || st != wire.CISuccess || !done {
			t.Fatalf("%+v %v %v", l, st, done)
		}
	}
	old, cur := view("test", "completed", "failure"), view("test", "in_progress", "")
	old.ID, cur.ID = "9", "10"
	if l := (runSet{old, cur}).latest(); len(l) != 1 || l[0].ID != "10" {
		t.Fatalf("the newest run: %+v", l)
	}
}

func TestRollupStates(t *testing.T) {
	ok, bad, going, queued := view("a", "completed", "success"), view("b", "completed", "failure"), view("c", "in_progress", ""), view("d", "queued", "")
	cancelled, timedOut := view("e", "completed", "cancelled"), view("f", "completed", "timed_out")
	vercelBad := []wire.CICheck{{Name: "Vercel", Kind: wire.CICheckStatus, State: wire.CICheckFailure}}
	for _, c := range []struct {
		name   string
		runs   runSet
		checks []wire.CICheck
		want   wire.CIRollupState
		done   bool
	}{
		{"none", nil, nil, wire.CINone, false},
		{"success", runSet{ok}, nil, wire.CISuccess, true},
		{"failure", runSet{ok, bad}, nil, wire.CIFailure, true},
		{"timed out", runSet{ok, timedOut}, nil, wire.CIFailure, true},
		{"failure while running", runSet{bad, going}, nil, wire.CIFailure, false},
		{"running", runSet{ok, going, queued}, nil, wire.CIRunning, false},
		{"pending", runSet{ok, queued}, nil, wire.CIPending, false},
		{"cancelled", runSet{ok, cancelled}, nil, wire.CICancelled, true},
		{"vercel failure", runSet{ok}, vercelBad, wire.CIFailure, true},
		{"checks only", nil, []wire.CICheck{{Name: "x", Kind: wire.CICheckRun, State: wire.CICheckSuccess}}, wire.CISuccess, true},
	} {
		got, done := c.runs.state(c.checks)
		if got != c.want || done != c.done {
			t.Errorf("%s: %s done %v, want %s done %v", c.name, got, done, c.want, c.done)
		}
	}
}

func TestDoneNeedsFinalChecks(t *testing.T) {
	s := runSet{view("a", "completed", "success")}
	st, done := s.state([]wire.CICheck{{Name: "Vercel", Kind: wire.CICheckStatus, State: wire.CICheckPending}})
	if st != wire.CIPending || done {
		t.Fatalf("%s %v", st, done)
	}
}

func TestETAMean(t *testing.T) {
	mk := func(start, end time.Duration, workflow string) wire.CIRun {
		r := run("1", workflow, "completed", "success", "s", "main")
		r.StartedAt, r.UpdatedAt = ts(start), ts(end)
		return r
	}
	hist := []wire.CIRun{
		mk(-time.Hour, -time.Hour+60*time.Second, "test"),
		mk(-2*time.Hour, -2*time.Hour+90*time.Second, "test"),
		mk(-3*time.Hour, -3*time.Hour+120*time.Second, "test"),
		mk(-96*time.Hour, -96*time.Hour+600*time.Second, "test"),
		mk(-time.Hour, -time.Hour+500*time.Second, "release"),
	}
	d, ok := eta(hist, "test", t0)
	if !ok || d != 90*time.Second {
		t.Fatalf("%v %v", d, ok)
	}
	if _, ok := eta(hist, "none", t0); ok {
		t.Fatal("an unknown workflow has an ETA")
	}
}

func TestRevIgnoresFreshness(t *testing.T) {
	a := wire.CIRollup{State: wire.CIRunning, Runs: []wire.CIRunView{view("a", "in_progress", "")}}
	b := a
	b.AsOf, b.Stale, b.Source = t0, true, wire.CISourceWebhook
	b.Runs = []wire.CIRunView{view("a", "in_progress", "")}
	b.Runs[0].DurationS, b.Runs[0].ETAS = 40, 90
	if rev(a) != rev(b) || len(rev(a)) != 12 {
		t.Fatalf("%s %s", rev(a), rev(b))
	}
	b.State = wire.CIFailure
	if rev(a) == rev(b) {
		t.Fatal("a new state keeps the rev")
	}
}

func TestIndexLoadsFromMirror(t *testing.T) {
	e := newEnv(t)
	e.put(state("o/r"),
		runsDoc("o/r", run("9001", "test", "in_progress", "", sha1, "main")),
		jobsDoc("o/r", "9001", 1, false, wire.CIJob{ID: "70001", Name: "go test", Status: "in_progress"}))
	a, err := e.x.RunAnswer("o/r", "9001", 0)
	if err != nil || len(a.Run.Jobs) != 1 || a.Run.Jobs[0].ID != "70001" || a.Run.Workflow != "test" {
		t.Fatalf("%+v %v", a, err)
	}
	if info, ok := e.x.Job("o/r", "70001"); !ok || info.Run != "9001" || info.Completed {
		t.Fatalf("%+v %v", info, ok)
	}
	if _, ok := e.x.Job("o/r", "nope"); ok {
		t.Fatal("an unknown job")
	}
}

func TestIndexResetOnEpoch(t *testing.T) {
	e := newEnv(t)
	e.put(state("o/r"), runsDoc("o/r", run("1", "test", "completed", "success", sha1, "main")))
	if _, err := e.x.Repo("o/r"); err != nil {
		t.Fatal(err)
	}
	if err := e.feed.Reset(); err != nil {
		t.Fatal(err)
	}
	e.x.sync()
	if _, err := e.x.Repo("o/r"); !errors.Is(err, ErrNoCIData) {
		t.Fatalf("after the reset: %v", err)
	}
	e.put(state("o/new"))
	if _, err := e.x.Repo("o/new"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.x.Repo("o/r"); !errors.Is(err, ErrUntracked) {
		t.Fatalf("%v", err)
	}
}

func TestIndexUntracked(t *testing.T) {
	e := newEnv(t)
	_, err := e.x.Repo("o/x")
	if !errors.Is(err, ErrNoCIData) || !strings.Contains(err.Error(), "sends no CI data") {
		t.Fatalf("no ciState: %v", err)
	}
	e.put(state("o/r"))
	_, err = e.x.Repo("o/x")
	if !errors.Is(err, ErrUntracked) || err.Error() != "OverSire does not track o/x. Add the repo in Settings › Code hosts." {
		t.Fatalf("%v", err)
	}
	if st, err := e.x.Repo("O/R"); err != nil || st.Repo != "o/r" {
		t.Fatalf("case: %+v %v", st, err)
	}
}

func TestFreshnessStale(t *testing.T) {
	e := newEnv(t)
	polled := t0.Add(-6 * time.Minute)
	e.put(mdoc(wire.MirrorCIState, "home", wire.CIStateDoc{AsOf: t0, Source: wire.CISourceWebhook, Repos: []wire.CIRepoState{
		{Repo: "o/r", CI: "GitHub Actions", Capabilities: []string{}, PolledAt: &polled},
		{Repo: "o/q", CI: "GitHub Actions", Capabilities: []string{}}}}))
	if f := e.x.Freshness("o/r"); !f.Stale || !f.AsOf.Equal(polled) || f.Source != wire.CISourceWebhook {
		t.Fatalf("old poll: %+v", f)
	}
	if f := e.x.Freshness("o/q"); f.Stale {
		t.Fatalf("fresh: %+v", f)
	}
	e.setOnline(false)
	if f := e.x.Freshness("o/q"); !f.Stale {
		t.Fatalf("offline: %+v", f)
	}
}

func TestBookKeepsOldRuns(t *testing.T) {
	e := newEnv(t)
	var a, b []wire.CIRun
	for i := 1; i <= 12; i++ {
		r := run(string(rune('a'+i)), "test", "completed", "success", sha1, "main")
		r.UpdatedAt = ts(time.Duration(i) * time.Second)
		if i <= 6 {
			a = append(a, r)
		} else {
			b = append(b, r)
		}
	}
	e.put(state("o/r"), runsDoc("o/r", a...))
	e.put(runsDoc("o/r", b...))
	runs, err := e.book.Runs(RunQuery{Repo: "o/r"})
	if err != nil || len(runs) != 12 || runs[0].UpdatedAt != ts(12*time.Second) {
		t.Fatalf("%d runs %v", len(runs), err)
	}
	if runs, _ = e.book.Runs(RunQuery{Repo: "o/r", Limit: 3}); len(runs) != 3 {
		t.Fatalf("limit: %d", len(runs))
	}
}

func TestBookNewestAttemptWins(t *testing.T) {
	e := newEnv(t)
	r2 := run("1", "test", "completed", "success", sha1, "main")
	r2.Attempt = 2
	r1 := run("1", "test", "completed", "failure", sha1, "main")
	if err := e.book.PutRuns("o/r", []wire.CIRun{r2}); err != nil {
		t.Fatal(err)
	}
	if err := e.book.PutRuns("o/r", []wire.CIRun{r1}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.book.Run("o/r", "1"); got.Attempt != 2 || got.Conclusion != "success" {
		t.Fatalf("%+v", got)
	}
}

func TestBookPrune(t *testing.T) {
	e := newEnv(t)
	put := func(job string, complete bool, age time.Duration) {
		if err := e.book.PutLog(CachedLog{Repo: "o/r", Job: job, Lines: []string{"x"}, Complete: complete, Fetched: t0.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	put("old", true, 8*24*time.Hour)
	put("new", true, time.Hour)
	put("stuck", false, 3*time.Minute)
	put("open", false, time.Minute)
	old := run("1", "test", "completed", "success", sha1, "main")
	old.UpdatedAt = ts(-8 * 24 * time.Hour)
	if err := e.book.PutRuns("o/r", []wire.CIRun{old, run("2", "test", "completed", "success", sha1, "main")}); err != nil {
		t.Fatal(err)
	}
	if err := e.book.PutWorkflows(wire.CIWorkflows{Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	e.clk.add(25 * time.Hour)
	if err := e.book.Prune(e.clk.now()); err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]bool{"old": false, "new": true, "stuck": false, "open": false} {
		if _, ok, _ := e.book.Log("o/r", job); ok != want {
			t.Errorf("log %s: %v", job, ok)
		}
	}
	if runs, _ := e.book.Runs(RunQuery{Repo: "o/r"}); len(runs) != 1 {
		t.Errorf("runs: %d", len(runs))
	}
	if _, ok, _ := e.book.Workflows("o/r"); ok {
		t.Error("the old workflows stay")
	}
}

func TestRollupFromIndex(t *testing.T) {
	e := newEnv(t)
	e.put(state("o/r"),
		runsDoc("o/r", run("1", "test", "completed", "success", sha1, "main"), run("2", "lint", "in_progress", "", sha1, "main"),
			run("3", "test", "completed", "failure", "other", "main")),
		mdoc(wire.MirrorCIChecks, "o/r/"+sha1, wire.CIChecksDoc{Repo: "o/r", Sha: sha1, PRs: []int{}, Final: false, FetchedAt: t0,
			Checks: []wire.CICheck{{Name: "Vercel", Kind: wire.CICheckStatus, State: wire.CICheckPending}}}))
	r, err := e.x.Rollup("o/r", Target{SHA: sha1}, nil)
	if err != nil || r.State != wire.CIRunning || r.Done || len(r.Runs) != 2 || len(r.Checks) != 1 || len(r.Rev) != 12 {
		t.Fatalf("%+v %v", r, err)
	}
	r2, _ := e.x.Rollup("o/r", Target{SHA: sha1}, []string{"test"})
	if len(r2.Runs) != 1 || r2.State != wire.CIPending || r2.Rev == r.Rev {
		t.Fatalf("workflow filter: %+v", r2)
	}
	none, _ := e.x.Rollup("o/r", Target{}, nil)
	if none.State != wire.CINone || none.Done || none.Runs == nil || none.Checks == nil {
		t.Fatalf("%+v", none)
	}
}

func TestExpectAttemptExpires(t *testing.T) {
	e := newEnv(t)
	r := run("9", "test", "completed", "failure", sha1, "main")
	r.Attempt = 2
	e.put(state("o/r"), runsDoc("o/r", r))
	roll := func() wire.CIRollup {
		t.Helper()
		out, err := e.x.Rollup("o/r", Target{SHA: sha1}, nil)
		if err != nil || len(out.Runs) != 1 {
			t.Fatalf("%+v %v", out, err)
		}
		return out
	}
	e.x.ExpectAttempt("o/r", "9", 3)
	if got := roll(); got.Runs[0].Status != "queued" || got.Done || got.State != wire.CIPending {
		t.Fatalf("%+v", got)
	}
	e.clk.add(2*time.Minute + time.Second)
	if got := roll(); got.Runs[0].Attempt != 2 || got.State != wire.CIFailure || !got.Done {
		t.Fatalf("after the hold: %+v", got)
	}
	e.x.ExpectAttempt("o/r", "9", 3)
	r.Attempt, r.Status, r.Conclusion = 3, "in_progress", ""
	e.put(runsDoc("o/r", r))
	if got := roll(); got.Runs[0].Attempt != 3 || got.Runs[0].Status != "in_progress" {
		t.Fatalf("attempt 3 is in the mirror: %+v", got)
	}
}
