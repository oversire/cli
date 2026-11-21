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
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// writeEnv serves the routes of an api with the caller that the test sets.
type writeEnv struct {
	*api
	mu     sync.Mutex
	caller *server.Caller
	hs     *httptest.Server
}

// writesAPI tracks o/r with run 9, attempt 2, failed.
func writesAPI(t *testing.T) *writeEnv {
	t.Helper()
	a := newAPI(t)
	r := run("9", "test", "completed", "failure", sha1, "main")
	r.Attempt = 2
	a.put(state("o/r"), runsDoc("o/r", r), jobsDoc("o/r", "9", 2, true))
	w := &writeEnv{api: a}
	mux := a.mux()
	w.hs = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		w.mu.Lock()
		c := w.caller
		w.mu.Unlock()
		if c != nil {
			req = req.WithContext(server.WithCaller(req.Context(), *c))
		}
		mux.ServeHTTP(rw, req)
	}))
	t.Cleanup(w.hs.Close)
	return w
}

func (w *writeEnv) as(c server.Caller) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.caller = &c
}

func (w *writeEnv) post(path string, body any) (int, []byte) {
	w.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(w.hs.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (w *writeEnv) postErr(path string, body any, want int) string {
	w.t.Helper()
	code, b := w.post(path, body)
	if code != want {
		w.t.Fatalf("POST %s: %d %s, want %d", path, code, b, want)
	}
	var e struct{ Error string }
	_ = json.Unmarshal(b, &e)
	return e.Error
}

func (w *writeEnv) last() (wire.MemberCall, map[string]any) {
	w.t.Helper()
	w.calls.mu.Lock()
	defer w.calls.mu.Unlock()
	if len(w.calls.calls) == 0 {
		w.t.Fatal("no member call")
	}
	c := w.calls.calls[len(w.calls.calls)-1]
	var args map[string]any
	if err := json.Unmarshal(c.Args, &args); err != nil {
		w.t.Fatal(err)
	}
	return c, args
}

func actionResult(noop bool, attempt int) wire.CIActionResult {
	return wire.CIActionResult{OK: true, Noop: noop, Summary: "Run 9 runs again.", URL: "https://github.com/o/r/actions/runs/9",
		Run: &wire.CIRunView{ID: "9", Attempt: attempt, Workflow: "test", Status: "completed", Conclusion: "failure"}}
}

func TestRerunOnHomeSendsMemberCall(t *testing.T) {
	w := writesAPI(t)
	w.as(server.Caller{Loopback: true})
	w.calls.reply = okData(actionResult(false, 2))
	code, b := w.post("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r", FailedOnly: true, Agent: "claude"})
	var res wire.CIActionResult
	if code != 200 || json.Unmarshal(b, &res) != nil || !res.OK || res.Summary != "Run 9 runs again." {
		t.Fatalf("%d %s", code, b)
	}
	c, args := w.last()
	if c.Action != "ci.rerun" || args["run"] != "9" || args["failedOnly"] != true || args["repo"] != "o/r" || args["agent"] != nil {
		t.Fatalf("%s %v", c.Action, args)
	}
	if c.From.Machine != "mbp" || c.CLI == nil || c.CLI.Agent != "claude" {
		t.Fatalf("%+v %+v", c.From, c.CLI)
	}
}

func TestPathIDWinsOverBody(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = okData(actionResult(true, 2))
	if code, b := w.post("/v1/ci/runs/9/cancel", wire.CIActionRequest{Repo: "o/r", Run: "1"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if c, args := w.last(); c.Action != "ci.cancel" || args["run"] != "9" {
		t.Fatalf("%s %v", c.Action, args)
	}
}

func TestRerunJobNeedsRun(t *testing.T) {
	w := writesAPI(t)
	if e := w.postErr("/v1/ci/jobs/5/rerun", wire.CIActionRequest{Repo: "o/r"}, 400); e != "Give the run of job 5." {
		t.Fatal(e)
	}
	w.calls.reply = okData(actionResult(false, 2))
	if code, b := w.post("/v1/ci/jobs/5/rerun", wire.CIActionRequest{Repo: "o/r", Run: "9", Job: "7"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if c, args := w.last(); c.Action != "ci.rerunJob" || args["run"] != "9" || args["job"] != "5" {
		t.Fatalf("%s %v", c.Action, args)
	}
}

func TestNoRepoIs400(t *testing.T) {
	w := writesAPI(t)
	if e := w.postErr("/v1/ci/runs/9/rerun", wire.CIActionRequest{}, 400); e != "Give the repo as owner/name." {
		t.Fatal(e)
	}
	if e := w.postErr("/v1/ci/dispatch", wire.CIActionRequest{Repo: "o/r", Workflow: "ci.yml"}, 400); e != "Give the workflow and the ref." {
		t.Fatal(e)
	}
	if code, _ := w.post("/v1/ci/runs/9/rerun", map[string]any{"repo": "o/r", "bogus": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	if n := len(w.calls.actions()); n != 0 {
		t.Fatalf("%d calls", n)
	}
}

func TestUntrackedRepoIs404(t *testing.T) {
	w := writesAPI(t)
	if e := w.postErr("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/x"}, 404); e != "OverSire does not track o/x. Add the repo in Settings › Code hosts." {
		t.Fatal(e)
	}
	if n := len(w.calls.actions()); n != 0 {
		t.Fatalf("%d calls", n)
	}
}

func appStatus(status wire.AppCallStatus, summary string) func(wire.MemberCall) (wire.AppCallResult, error) {
	return func(c wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{ID: c.ID, Status: status, Summary: summary}, nil
	}
}

func TestNotAllowedIs422(t *testing.T) {
	w := writesAPI(t)
	text := "Agents may not cancel runs. Turn it on in Settings › Agents › CI."
	w.calls.reply = appStatus(wire.AppCallNotAllowed, text)
	if e := w.postErr("/v1/ci/runs/9/cancel", wire.CIActionRequest{Repo: "o/r", Agent: "claude s1"}, 422); e != text {
		t.Fatal(e)
	}
}

func TestFailedIs502(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = appStatus(wire.AppCallFailed, "GitHub said no.")
	if e := w.postErr("/v1/ci/runs/9/approve", wire.CIActionRequest{Repo: "o/r", Environments: []string{"prod"}}, 502); e != "GitHub said no." {
		t.Fatal(e)
	}
	if _, args := w.last(); args["environments"].([]any)[0] != "prod" {
		t.Fatal(args)
	}
}

func (w *writeEnv) rollup9() wire.CIRollup {
	w.t.Helper()
	var roll wire.CIRollup
	w.getJSON("/v1/ci/status?repo=o/r&ref=9", 200, &roll)
	return roll
}

func TestNoopIs200(t *testing.T) {
	w := writesAPI(t)
	before := w.rollup9()
	w.calls.reply = okData(actionResult(true, 2))
	code, b := w.post("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r"})
	var res wire.CIActionResult
	if code != 200 || json.Unmarshal(b, &res) != nil || !res.Noop {
		t.Fatalf("%d %s", code, b)
	}
	if after := w.rollup9(); after.Rev != before.Rev || after.State != wire.CIFailure {
		t.Fatalf("%+v", after)
	}
}

func TestRerunExpectsNextAttempt(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = okData(actionResult(false, 2))
	if code, b := w.post("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	roll := w.rollup9()
	if roll.Done || roll.State != wire.CIPending || len(roll.Runs) != 1 || roll.Runs[0].Status != "queued" || roll.Runs[0].Attempt != 3 || roll.Runs[0].Conclusion != "" {
		t.Fatalf("%+v", roll)
	}
	var ans wire.CIRunAnswer
	w.getJSON("/v1/ci/runs/9?repo=o/r", 200, &ans)
	if ans.Run.Attempt != 2 || ans.Run.Conclusion != "failure" {
		t.Fatalf("the run changed: %+v", ans.Run)
	}
}

func TestNoSourceIs503(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrNoSource }
	if e := w.postErr("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r"}, 503); e != "The OverSire app on mba is not open." {
		t.Fatal(e)
	}
}

func TestBusyIs429(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) { return wire.AppCallResult{}, membercall.ErrBusy }
	if e := w.postErr("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r"}, 429); e != "mba is busy. Try again." {
		t.Fatal(e)
	}
}

func TestTimeoutIs504(t *testing.T) {
	w := writesAPI(t)
	w.calls.reply = func(wire.MemberCall) (wire.AppCallResult, error) {
		return wire.AppCallResult{}, context.DeadlineExceeded
	}
	if e := w.postErr("/v1/ci/dispatch", wire.CIActionRequest{Repo: "o/r", Workflow: "ci.yml", Ref: "main"}, 504); !strings.Contains(e, "did not answer") {
		t.Fatal(e)
	}
}

func TestMemberCallerFromCredential(t *testing.T) {
	w := writesAPI(t)
	w.as(server.Caller{DeviceID: "dev-1", Name: "Fritz's MBA", Kind: string(wire.DeviceApp), Machine: "mba"})
	w.calls.reply = okData(actionResult(false, 2))
	if code, b := w.post("/v1/ci/runs/9/rerun", wire.CIActionRequest{Repo: "o/r"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if c, _ := w.last(); c.From != (wire.MemberCaller{Machine: "mba", Name: "Fritz's MBA", Device: "dev-1"}) {
		t.Fatalf("%+v", c.From)
	}
}

func TestDaemonCredentialAcceptedOnHome(t *testing.T) {
	w := writesAPI(t)
	w.as(server.Caller{DeviceID: "d-mini", Name: "mini", Kind: string(wire.DeviceDaemon), Machine: "mini"})
	w.calls.reply = okData(actionResult(false, 2))
	if code, b := w.post("/v1/ci/runs/9/cancel", wire.CIActionRequest{Repo: "o/r", Agent: "codex s2"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if c, _ := w.last(); c.From.Machine != "mini" || c.CLI.Agent != "codex s2" {
		t.Fatalf("%+v %+v", c.From, c.CLI)
	}
}

func TestForwardPostsBody(t *testing.T) {
	home := writesAPI(t)
	home.calls.reply = okData(actionResult(false, 2))
	var mu sync.Mutex
	var path, body, ctype string
	hs := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		path, body, ctype = r.URL.Path, string(b), r.Header.Get("Content-Type")
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		home.mux().ServeHTTP(rw, r)
	}))
	t.Cleanup(hs.Close)
	self := func() wire.GroupMember { return wire.GroupMember{Machine: "mba"} }
	fwd := NewForward(homeIs{machine: "mba", home: "mbp", base: hs.URL, token: "tok"}, &http.Client{Timeout: 5 * time.Second})
	mine := &fakeCaller{}
	routes := NewRoutes(home.x, nil, mine, NewLimiter(time.Second, home.clk.now), fwd, self)
	mux := http.NewServeMux()
	for _, r := range routes.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	ms := httptest.NewServer(mux)
	t.Cleanup(ms.Close)
	sent := `{"repo":"o/r","failedOnly":true}`
	resp, err := http.Post(ms.URL+"/v1/ci/runs/9/rerun", "application/json", strings.NewReader(sent))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	mu.Lock()
	defer mu.Unlock()
	if path != "/v1/ci/runs/9/rerun" || body != sent || ctype != "application/json" {
		t.Fatalf("home got %s %q %q", path, body, ctype)
	}
	var res wire.CIActionResult
	if resp.StatusCode != 200 || json.Unmarshal(got, &res) != nil || res.Summary != "Run 9 runs again." {
		t.Fatalf("%d %s", resp.StatusCode, got)
	}
	if len(mine.actions()) != 0 || len(home.calls.actions()) != 1 {
		t.Fatalf("member %v, home %v", mine.actions(), home.calls.actions())
	}
}
