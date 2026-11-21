package ci

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/membercall"
	"github.com/oversire/cli/wire"
)

func logView(complete bool, n int) wire.CILogView {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	return wire.CILogView{Repo: "o/r", JobID: "70001", Lines: lines, Complete: complete}
}

// logAPI has a tracked repo o/r with the job 70001 of run 9001.
func logAPI(t *testing.T, status string) *api {
	t.Helper()
	a := seeded(t)
	a.put(jobsDoc("o/r", "9001", 1, false, wire.CIJob{ID: "70001", Name: "go test", Status: status}))
	return a
}

func TestLogMissThenHit(t *testing.T) {
	a := logAPI(t, "completed")
	a.calls.reply = okData(logView(true, 3))
	var v wire.CILogView
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	if v.Cached || !v.Complete || len(v.Lines) != 3 || v.Run != "9001" || len(a.calls.actions()) != 1 {
		t.Fatalf("%+v", v)
	}
	if string(a.calls.calls[0].Args) != `{"jobId":"70001","lines":2000,"repo":"o/r"}` {
		t.Fatalf("args: %s", a.calls.calls[0].Args)
	}
	a.clk.add(time.Hour)
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	if !v.Cached || len(a.calls.actions()) != 1 {
		t.Fatalf("%+v %v", v, a.calls.actions())
	}
	a.setOnline(false)
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	if !v.Cached || !v.Stale {
		t.Fatalf("app closed: %+v", v)
	}
}

func TestLogSingleFlight(t *testing.T) {
	a := logAPI(t, "completed")
	data := okData(logView(true, 3))
	a.calls.reply = func(c wire.MemberCall) (wire.AppCallResult, error) {
		time.Sleep(150 * time.Millisecond)
		return data(c)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.logs.Get(context.Background(), "o/r", "70001", 50, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(a.calls.actions()); n != 1 {
		t.Fatalf("%d calls", n)
	}
}

func TestLogRunningJobRefetch(t *testing.T) {
	a := logAPI(t, "in_progress")
	a.calls.reply = okData(logView(false, 3))
	var v wire.CILogView
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	a.clk.add(10 * time.Second)
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	if !v.Cached || v.Complete || len(a.calls.actions()) != 1 {
		t.Fatalf("at +10 s: %+v", v)
	}
	a.clk.add(15 * time.Second)
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r", 200, &v)
	if v.Cached || len(a.calls.actions()) != 2 {
		t.Fatalf("at +25 s: %+v %v", v, a.calls.actions())
	}
}

func TestLogLines(t *testing.T) {
	a := logAPI(t, "completed")
	a.calls.reply = okData(logView(true, 500))
	var v wire.CILogView
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r&lines=40", 200, &v)
	if len(v.Lines) != 40 || !v.Truncated || v.Lines[39] != "line 500" || v.Lines[0] != "line 461" {
		t.Fatalf("%d %+v", len(v.Lines), v.Truncated)
	}
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r&lines=2000", 200, &v)
	if len(v.Lines) != 500 || v.Truncated {
		t.Fatalf("%d %v", len(v.Lines), v.Truncated)
	}
	a.errText("/v1/ci/jobs/70001/log?repo=o/r&lines=2001", 400)
	a.errText("/v1/ci/jobs/70001/log?repo=o/r&lines=0", 400)
}

func TestLogMissAppClosed(t *testing.T) {
	a := logAPI(t, "completed")
	a.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrNoSource }
	if e := a.errText("/v1/ci/jobs/70001/log?repo=o/r", 503); e != "The OverSire app on mba is not open." {
		t.Fatal(e)
	}
}

func TestLogNoCapability(t *testing.T) {
	a := newAPI(t)
	a.put(mdoc(wire.MirrorCIState, "home", wire.CIStateDoc{AsOf: t0, Source: wire.CISourcePoll,
		Repos: []wire.CIRepoState{{Repo: "o/r", CI: "GitHub Actions", Capabilities: []string{"rerun"}}}}))
	if e := a.errText("/v1/ci/jobs/1/log?repo=o/r", 422); e != "GitHub Actions has no logs." {
		t.Fatal(e)
	}
	if len(a.calls.actions()) != 0 {
		t.Fatal("a call went out")
	}
}

func TestLogFreshOnce(t *testing.T) {
	a := logAPI(t, "completed")
	a.calls.reply = okData(logView(true, 3))
	var v wire.CILogView
	a.getJSON("/v1/ci/jobs/70001/log?repo=o/r&fresh=1", 200, &v)
	if v.Cached {
		t.Fatal("fresh read from the cache")
	}
	a.errText("/v1/ci/jobs/70001/log?repo=o/r&fresh=1", 429)
	if len(a.calls.actions()) != 1 {
		t.Fatalf("%v", a.calls.actions())
	}
}

func TestLogAppFailed(t *testing.T) {
	a := logAPI(t, "in_progress")
	a.calls.reply = func(c wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallFailed, Summary: "GitHub gives a log after the job ends."}, nil
	}
	if e := a.errText("/v1/ci/jobs/70001/log?repo=o/r", 502); e != "GitHub gives a log after the job ends." {
		t.Fatal(e)
	}
	// A failed call leaves no cache and no flight.
	if _, ok, _ := a.book.Log("o/r", "70001"); ok {
		t.Fatal("cached")
	}
	if len(a.logs.flights) != 0 {
		t.Fatal("flight stays")
	}
}
