package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func TestWaitReturnsOnChange(t *testing.T) {
	a := seeded(t)
	var first wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	done := make(chan wire.CIRollup)
	go func() {
		var r wire.CIRollup
		a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=30&since="+first.Rev, 200, &r)
		done <- r
	}()
	time.Sleep(100 * time.Millisecond)
	a.put(runsDoc("o/r", run("9001", "test", "completed", "success", sha1, "main")))
	select {
	case r := <-done:
		if r.Rev == first.Rev || r.State != wire.CISuccess || !r.Done {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end")
	}
}

func TestWaitIgnoresOtherDocs(t *testing.T) {
	a := seeded(t)
	var first wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	done := make(chan wire.CIRollup)
	go func() {
		var r wire.CIRollup
		a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=1&since="+first.Rev, 200, &r)
		done <- r
	}()
	time.Sleep(100 * time.Millisecond)
	a.put(runsDoc("o/r", run("9001", "test", "in_progress", "", sha1, "main"), run("5", "test", "completed", "success", "other", "dev")))
	start := time.Now()
	r := <-done
	if r.Rev != first.Rev || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("an unrelated change ended the wait: %+v", r)
	}
}

func TestWaitTimeout(t *testing.T) {
	a := seeded(t)
	var first, r wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	start := time.Now()
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=1&since="+first.Rev, 200, &r)
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second || r.Rev != first.Rev || r.Done {
		t.Fatalf("%v %+v", d, r)
	}
}

func TestWaitFirstCallAnswersAtOnce(t *testing.T) {
	a := seeded(t)
	var r wire.CIRollup
	start := time.Now()
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=30", 200, &r)
	if time.Since(start) > time.Second || r.Rev == "" {
		t.Fatalf("%+v", r)
	}
}

func TestWaitFailFast(t *testing.T) {
	a := newAPI(t)
	a.put(state("o/r"), runsDoc("o/r", run("1", "test", "completed", "failure", sha1, "main"), run("2", "lint", "in_progress", "", sha1, "main")))
	var first, r wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	if first.Done {
		t.Fatal("done without failFast")
	}
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=30&failFast=1&since="+first.Rev, 200, &r)
	if !r.Done || r.State != wire.CIFailure {
		t.Fatalf("%+v", r)
	}
}

func TestWaitSettleNone(t *testing.T) {
	a := newAPI(t)
	a.put(state("o/r"))
	var r wire.CIRollup
	started := a.clk.now().Add(-2 * time.Second).Unix()
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=30&settle=1&started="+itoa(started), 200, &r)
	if r.State != wire.CINone || !r.Done {
		t.Fatalf("%+v", r)
	}
}

func TestWaitSettleTimer(t *testing.T) {
	a := newAPI(t)
	a.put(state("o/r"))
	// The fake clock stands still. The wait has to end by its deadline, not by the settle timer.
	var first, r wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&timeout=1&settle=600&since="+first.Rev, 200, &r)
	if r.Done {
		t.Fatalf("%+v", r)
	}
}

func TestWaitWorkflowFilter(t *testing.T) {
	a := newAPI(t)
	a.put(state("o/r"), runsDoc("o/r", run("1", "test", "completed", "success", sha1, "main"), run("2", "lint", "in_progress", "", sha1, "main")))
	var r wire.CIRollup
	a.getJSON("/v1/ci/wait?repo=o/r&ref=main&workflow=test", 200, &r)
	if len(r.Runs) != 1 || !r.Done || r.State != wire.CISuccess {
		t.Fatalf("%+v", r)
	}
}

func TestWaitBadTimeout(t *testing.T) {
	a := seeded(t)
	for _, q := range []string{"timeout=301", "timeout=0", "settle=601", "started=x"} {
		a.errText("/v1/ci/wait?repo=o/r&ref=main&"+q, 400)
	}
}

func TestWaitClientGone(t *testing.T) {
	a := seeded(t)
	var first wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &first)
	runtime.GC()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", a.srv.URL+"/v1/ci/wait?repo=o/r&ref=main&timeout=300&since="+first.Rev, nil)
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-errc
	http.DefaultClient.CloseIdleConnections()
	a.srv.CloseClientConnections()
	for i := 0; i < 50 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: %d before, %d after", before, n)
	}
}

type putGroup struct{}

func (putGroup) Snapshot() (wire.Group, bool) { return wire.Group{ID: "g", Home: "mbp"}, true }
func (putGroup) Self() wire.GroupMember       { return wire.GroupMember{Machine: "mbp"} }

type putHolder struct{}

func (putHolder) Get(string) (wire.Lease, bool) {
	return wire.Lease{Name: "automations", Holder: "app-1", HolderName: "mbp", Epoch: 1, Until: time.Now().Add(time.Hour)}, true
}

// A mirror put wakes the index at once, with no wait for the tick.
func TestRoutesWakeOnPut(t *testing.T) {
	a := seeded(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.x.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	mr := mirror.NewRoutes(a.feed, putGroup{}, putHolder{}, putGroup{}.Self, nil, func(any) { a.x.Wake() })
	mux := http.NewServeMux()
	for _, r := range mr.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
	}))
	t.Cleanup(srv.Close)

	var before wire.CIRollup
	a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &before)
	body, _ := json.Marshal(wire.MirrorPut{Epoch: a.feed.Head().Epoch, Source: "app-1", SourceName: "mbp",
		Docs: []wire.MirrorDoc{runsDoc("o/r", run("9001", "test", "completed", "success", sha1, "main"))}})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/mirror/docs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put: %v %v", resp, err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var after wire.CIRollup
		a.getJSON("/v1/ci/status?repo=o/r&ref=main", 200, &after)
		if after.Rev != before.Rev {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the status did not change after the put")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWaitUnknownRunWaitsForPoll(t *testing.T) {
	a := seeded(t)
	var first wire.CIRollup
	a.getJSON("/v1/ci/wait?repo=o/r&ref=9002&timeout=30", 200, &first)
	if first.State != wire.CINone || first.Done {
		t.Fatalf("%+v", first)
	}
	done := make(chan wire.CIRollup)
	go func() {
		var r wire.CIRollup
		a.getJSON("/v1/ci/wait?repo=o/r&ref=9002&timeout=30&since="+first.Rev, 200, &r)
		done <- r
	}()
	time.Sleep(100 * time.Millisecond)
	a.put(runsDoc("o/r", run("9001", "test", "in_progress", "", sha1, "main"), run("9002", "release", "completed", "success", "b2", "v1")))
	select {
	case r := <-done:
		if r.State != wire.CISuccess || !r.Done || r.SHA != "b2" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end")
	}
}

func TestWaitUnknownRunAfterSettle(t *testing.T) {
	a := seeded(t)
	started := a.clk.now().Add(-2 * time.Second).Unix()
	if e := a.errText("/v1/ci/wait?repo=o/r&ref=9002&timeout=30&settle=1&started="+itoa(started), 404); e != "OverSire has no run 9002 in o/r." {
		t.Fatal(e)
	}
}
