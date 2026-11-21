package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/membercall"
	"github.com/oversire/cli/wire"
)

const runURL9 = "https://github.com/o/r/actions/runs/9002"

type fakeOutputs struct {
	mu   sync.Mutex
	recs map[string]json.RawMessage
}

func (f *fakeOutputs) Record(collection, id string) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if collection != "ai-outputs" {
		return nil, false, nil
	}
	b, ok := f.recs[id]
	return b, ok, nil
}

func (f *fakeOutputs) put(id, cause string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recs == nil {
		f.recs = map[string]json.RawMessage{}
	}
	f.recs[id] = json.RawMessage(`{"body":{"cause":"` + cause + `","fix":"Pin the version.","confidence":"high","at":"2026-01-02T03:04:05.123Z"}}`)
}

type explainEnv struct {
	*api
	outs *fakeOutputs
	e    *Explainer
	hs   *httptest.Server
}

// explainAPI tracks o/r with a failed run 9002 and a running run 9001.
func explainAPI(t *testing.T) *explainEnv {
	t.Helper()
	a := newAPI(t)
	a.put(state("o/r"), runsDoc("o/r", run("9001", "test", "in_progress", "", sha1, "main"), run("9002", "test", "completed", "failure", sha1, "main"),
		run("9003", "test", "completed", "success", sha1, "main")))
	x := &explainEnv{api: a, outs: &fakeOutputs{}}
	self := func() wire.GroupMember { return wire.GroupMember{Machine: "mbp"} }
	x.e = NewExplainer(a.x, x.outs, a.calls, NewLimiter(10*time.Minute, a.clk.now), self)
	mux := http.NewServeMux()
	for _, r := range x.e.Routes(NewForward(homeIs{machine: "mbp", home: "mbp"}, http.DefaultClient)) {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	x.hs = httptest.NewServer(mux)
	t.Cleanup(x.hs.Close)
	return x
}

func (x *explainEnv) post(id string, req wire.CIExplainRequest) (int, []byte) {
	x.t.Helper()
	b, _ := json.Marshal(req)
	resp, err := http.Post(x.hs.URL+"/v1/ci/runs/"+id+"/explain", "application/json", bytes.NewReader(b))
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (x *explainEnv) errText(id string, req wire.CIExplainRequest, want int) string {
	x.t.Helper()
	code, b := x.post(id, req)
	if code != want {
		x.t.Fatalf("%d %s, want %d", code, b, want)
	}
	var e struct{ Error string }
	_ = json.Unmarshal(b, &e)
	return e.Error
}

func aiReply(cause string) func(wire.MemberCall) (wire.AppCallResult, error) {
	return okData(wire.CIExplanation{Repo: "o/r", Run: "9002", Attempt: 1, Cause: cause, Fix: "Fix it.", Confidence: "medium", At: t0, Source: "ai"})
}

func TestExplainCacheHit(t *testing.T) {
	x := explainAPI(t)
	x.outs.put("ci-explanation:"+runURL9+"#1", "Cached cause.")
	x.setOnline(false)
	var got wire.CIExplanation
	code, b := x.post("9002", wire.CIExplainRequest{Repo: "o/r"})
	if code != 200 || json.Unmarshal(b, &got) != nil {
		t.Fatalf("%d %s", code, b)
	}
	if got.Source != "cache" || got.Cause != "Cached cause." || got.Fix != "Pin the version." || got.Confidence != "high" || got.Run != "9002" || got.Attempt != 1 ||
		got.At.IsZero() || len(x.calls.actions()) != 0 {
		t.Fatalf("%+v %v", got, x.calls.actions())
	}
}

func TestExplainMissCallsApp(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = aiReply("Test timed out.")
	var got wire.CIExplanation
	code, b := x.post("9002", wire.CIExplainRequest{Repo: "o/r", Job: "build", Agent: "claude"})
	if code != 200 || json.Unmarshal(b, &got) != nil || got.Source != "ai" || got.Cause != "Test timed out." {
		t.Fatalf("%d %s", code, b)
	}
	if len(x.calls.calls) != 1 {
		t.Fatalf("%v", x.calls.actions())
	}
	c := x.calls.calls[0]
	var args map[string]any
	_ = json.Unmarshal(c.Args, &args)
	if c.Action != "ci.explain" || c.CLI == nil || c.CLI.Agent != "claude" || args["repo"] != "o/r" || args["runId"] != "9002" || args["attempt"] != float64(1) ||
		args["job"] != "build" || args["force"] != nil {
		t.Fatalf("%+v %v", c, args)
	}
}

func TestExplainSingleFlight(t *testing.T) {
	x := explainAPI(t)
	started, release := make(chan struct{}, 4), make(chan struct{})
	x.calls.reply = func(c wire.MemberCall) (wire.AppCallResult, error) {
		started <- struct{}{}
		<-release
		return aiReply("Once.")(c)
	}
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = x.post("9002", wire.CIExplainRequest{Repo: "o/r"})
		}()
	}
	<-started
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Fatalf("%v", codes)
		}
	}
	if n := len(x.calls.actions()); n != 1 {
		t.Fatalf("%d calls", n)
	}
}

func TestExplainForceLimit(t *testing.T) {
	x := explainAPI(t)
	x.outs.put("ci-explanation:"+runURL9+"#1", "Cached cause.")
	x.calls.reply = aiReply("New cause.")
	var got wire.CIExplanation
	code, b := x.post("9002", wire.CIExplainRequest{Repo: "o/r", Force: true})
	if code != 200 || json.Unmarshal(b, &got) != nil || got.Source != "ai" || got.Cause != "New cause." {
		t.Fatalf("%d %s", code, b)
	}
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/r", Force: true}, 429); !strings.Contains(e, "explanation") {
		t.Fatal(e)
	}
	if n := len(x.calls.actions()); n != 1 {
		t.Fatalf("%d calls", n)
	}
	x.clk.add(11 * time.Minute)
	if code, b := x.post("9002", wire.CIExplainRequest{Repo: "o/r", Force: true}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
}

func TestExplainForceFailureFreesWindow(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = func(c wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallFailed, Summary: "The AI failed."}, nil
	}
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/r", Force: true}, 502); e != "The AI failed." {
		t.Fatal(e)
	}
	x.calls.reply = aiReply("Now ok.")
	if code, b := x.post("9002", wire.CIExplainRequest{Repo: "o/r", Force: true}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
}

func TestExplainNotFailed(t *testing.T) {
	x := explainAPI(t)
	for _, id := range []string{"9001", "9003"} {
		if e := x.errText(id, wire.CIExplainRequest{Repo: "o/r"}, 422); e != "Run "+id+" did not fail." {
			t.Fatal(e)
		}
	}
	if n := len(x.calls.actions()); n != 0 {
		t.Fatalf("%d calls", n)
	}
}

func TestExplainNoApp(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrNoSource }
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/r"}, 503); !strings.Contains(e, "not open") {
		t.Fatal(e)
	}
}

func TestExplainTimeout(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{}, context.DeadlineExceeded
	}
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/r"}, 504); !strings.Contains(e, "Ask again in a minute") {
		t.Fatal(e)
	}
}

func TestExplainAppNotAllowed(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = func(c wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{ID: c.ID, Status: wire.AppCallNotAllowed, Summary: "Set an AI engine in Settings › AI."}, nil
	}
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/r"}, 422); e != "Set an AI engine in Settings › AI." {
		t.Fatal(e)
	}
}

func TestExplainEmptyCause(t *testing.T) {
	x := explainAPI(t)
	x.calls.reply = aiReply("")
	x.errText("9002", wire.CIExplainRequest{Repo: "o/r"}, 502)
}

func TestExplainRouteErrors(t *testing.T) {
	x := explainAPI(t)
	if e := x.errText("9002", wire.CIExplainRequest{Repo: "o/x"}, 404); !strings.Contains(e, "does not track o/x") {
		t.Fatal(e)
	}
	x.errText("9002", wire.CIExplainRequest{Repo: "bad"}, 400)
	if e := x.errText("77", wire.CIExplainRequest{Repo: "o/r"}, 404); !strings.Contains(e, "no run 77") {
		t.Fatal(e)
	}
}
