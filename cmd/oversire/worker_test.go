package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

func init() { waitFloor = 5 * time.Millisecond }

// workerDaemon serves the read and cursor routes of a worker. status is the code of the read.
type workerDaemon struct {
	mu     sync.Mutex
	status int
	read   worker.Read
	query  string
	puts   []string
	cursor []worker.Cursor
}

func (w *workerDaemon) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/workers/OVERSIRE-12/read":
		w.query = r.URL.RawQuery
		if w.status != 0 {
			rw.WriteHeader(w.status)
			json.NewEncoder(rw).Encode(map[string]string{"error": "The read failed."})
			return
		}
		json.NewEncoder(rw).Encode(w.read)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/workers/OVERSIRE-12/cursors/"):
		var c worker.Cursor
		json.NewDecoder(r.Body).Decode(&c)
		w.puts = append(w.puts, r.URL.Path)
		w.cursor = append(w.cursor, c)
		rw.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(rw, r)
	}
}

func sampleRead() worker.Read {
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	return worker.Read{
		Header: worker.Header{Session: "s1", Key: "work:OVERSIRE-12", Harness: "codex", State: wire.PhaseWorking, First: 4, Last: 5},
		Turns: []wire.SessionTurn{
			{Seq: 4, Role: wire.RoleUser, At: at, Text: "Run the tests.", Origin: wire.OriginOverseer},
			{Seq: 5, Role: wire.RoleAssistant, At: at, Text: "I run them now."},
		},
		Next: 5,
	}
}

func TestWorkerReadPrintsTheFoldAndCommitsTheCursor(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12", "--reader", "main", "--limit", "50"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "work:OVERSIRE-12 | codex | working") || !strings.Contains(got, "turns 4-5") ||
		!strings.Contains(got, "[overseer] Run the tests.\n") || !strings.Contains(got, "I run them now.\n") {
		t.Fatalf("output:\n%s", got)
	}
	if d.query != "limit=50&reader=main" {
		t.Fatalf("query %q", d.query)
	}
	if len(d.puts) != 1 || d.puts[0] != "/v1/workers/OVERSIRE-12/cursors/main" || d.cursor[0].Seq != 5 {
		t.Fatalf("puts %v %v", d.puts, d.cursor)
	}
}

func TestWorkerReadUsesTheOverseerReaderByDefault(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12"}); err != nil {
		t.Fatal(err)
	}
	if d.query != "reader=overseer" || len(d.puts) != 1 || d.puts[0] != "/v1/workers/OVERSIRE-12/cursors/overseer" {
		t.Fatalf("%q %v", d.query, d.puts)
	}
}

func TestWorkerReadJSONPrintsTheRawTurns(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12", "--json"}); err != nil {
		t.Fatal(err)
	}
	var turns []wire.SessionTurn
	if err := json.Unmarshal(out.Bytes(), &turns); err != nil || len(turns) != 2 || turns[0].Origin != wire.OriginOverseer || turns[1].Seq != 5 {
		t.Fatalf("%v %s", err, out)
	}
	if len(d.puts) != 1 {
		t.Fatalf("puts %v", d.puts)
	}
}

func TestWorkerReadDoesNotCommitOnError(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{status: http.StatusInternalServerError}
	fakeDaemon(t, c, d)
	err := c.worker(context.Background(), []string{"read", "OVERSIRE-12"})
	var de *daemonError
	if !errors.As(err, &de) || de.Status != 500 || len(d.puts) != 0 || out.Len() != 0 {
		t.Fatalf("%v %v %q", err, d.puts, out)
	}
}

func TestWorkerUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	for _, args := range [][]string{nil, {"nope"}, {"read"}, {"read", "A", "B"}, {"read", "A", "--limit", "-1"}, {"read", "A", "--reader", ""}} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// waitDaemon serves wait, read, cursor and brief routes. waits gives the reasons of the wait calls in order; the last one repeats.
type waitDaemon struct {
	workerDaemon
	waits  []worker.WakeReason
	calls  int
	maxes  []string
	brief  worker.Brief
	header worker.Header
}

func (w *waitDaemon) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/workers/OVERSIRE-12/wait":
		w.mu.Lock()
		reason := w.waits[min(w.calls, len(w.waits)-1)]
		w.calls++
		w.maxes = append(w.maxes, r.URL.Query().Get("max"))
		w.mu.Unlock()
		json.NewEncoder(rw).Encode(worker.Wake{Reason: reason, Header: w.header})
	case r.URL.Path == "/v1/workers/OVERSIRE-12/brief":
		json.NewEncoder(rw).Encode(w.brief)
	default:
		w.workerDaemon.ServeHTTP(rw, r)
	}
}

func TestWorkerWaitPrintsTheReadOnWake(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &waitDaemon{workerDaemon: workerDaemon{read: sampleRead()}, waits: []worker.WakeReason{worker.WakeNone, worker.WakeInput}}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"wait", "OVERSIRE-12", "--reader", "main"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "work:OVERSIRE-12 | codex | working") || !strings.Contains(got, "[overseer] Run the tests.\n") || strings.Contains(got, "Stalled") {
		t.Fatalf("output:\n%s", got)
	}
	if d.calls != 2 || d.maxes[0] != "25s" || len(d.puts) != 1 || d.puts[0] != "/v1/workers/OVERSIRE-12/cursors/main" || d.cursor[0].Seq != 5 {
		t.Fatalf("calls %d %v puts %v", d.calls, d.maxes, d.puts)
	}
}

func TestWorkerWaitSaysAStall(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &waitDaemon{workerDaemon: workerDaemon{read: sampleRead()}, waits: []worker.WakeReason{worker.WakeStall}}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"wait", "--timeout", "1m", "OVERSIRE-12"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Stalled: ") || len(d.puts) != 1 {
		t.Fatalf("output:\n%s", out)
	}
}

func TestWorkerWaitExits3OnTimeout(t *testing.T) {
	c, out, _ := testCLI(t, "")
	hdr := worker.Header{Session: "s1", Key: "work:OVERSIRE-12", Harness: "codex", State: wire.PhaseWorking}
	read := worker.Read{Header: hdr, Turns: []wire.SessionTurn{{Seq: 4}, {Seq: 9}}, Next: 9}
	read.Header.First, read.Header.Last = 4, 9
	d := &waitDaemon{workerDaemon: workerDaemon{read: read}, waits: []worker.WakeReason{worker.WakeNone}, header: hdr}
	fakeDaemon(t, c, d)
	err := c.worker(context.Background(), []string{"wait", "OVERSIRE-12", "--timeout", "1s"})
	var e *exitError
	if !errors.As(err, &e) || e.code != 3 || !errors.Is(err, errSilent) {
		t.Fatalf("err %v", err)
	}
	got := out.String()
	if got != "work:OVERSIRE-12 | codex | working | turns 4-9\n" || len(d.puts) != 0 {
		t.Fatalf("output %q puts %v", got, d.puts)
	}
}

func TestWorkerWaitRefusesBadUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	fakeDaemon(t, c, &waitDaemon{waits: []worker.WakeReason{worker.WakeInput}})
	for _, args := range [][]string{{"wait"}, {"wait", "X", "--timeout", "10m"}, {"wait", "X", "--timeout", "0s"}, {"brief"}} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestWorkerBriefPrintsTheSections(t *testing.T) {
	c, out, _ := testCLI(t, "")
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	d := &waitDaemon{brief: worker.Brief{
		Header: worker.Header{Key: "work:OVERSIRE-12", State: wire.PhaseWaiting, Reason: wire.ReasonPermission, Request: "permission Bash npm test",
			Repos: []wire.ReviewRepo{{Name: "cli", Added: 10, Removed: 2, Files: 3}}},
		Plan:   "Build the thing.",
		Notes:  []wire.SessionTurn{{Seq: 3, Role: wire.RoleUser, At: at, Text: "Use sqlite.", Origin: wire.OriginOverseer}},
		Last:   "Part one is done.",
		Failed: []wire.SessionTurn{{Seq: 4, Role: wire.RoleTool, At: at, Tool: &wire.TurnTool{Name: "Bash", Status: wire.ToolFailed, Input: "go test", Summary: "FAIL"}}},
	}}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"brief", "OVERSIRE-12"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"## Plan\nBuild the thing.", "## Decisions\n[overseer] Use sqlite.", "## Last report\nPart one is done.",
		"## Failed tools\n✗ Bash go test\n    FAIL", "## Open request\npermission Bash npm test", "## Diff\ncli +10 -2, 3 files"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestWorkerWaitKeepsAFloorBetweenNoneAnswers(t *testing.T) {
	old := waitFloor
	waitFloor = 100 * time.Millisecond
	t.Cleanup(func() { waitFloor = old })
	c, _, _ := testCLI(t, "")
	d := &waitDaemon{waits: []worker.WakeReason{worker.WakeNone}}
	fakeDaemon(t, c, d)
	err := c.worker(context.Background(), []string{"wait", "OVERSIRE-12", "--timeout", "450ms"})
	var e *exitError
	if !errors.As(err, &e) || e.code != 3 {
		t.Fatalf("err %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls < 2 || d.calls > 6 {
		t.Fatalf("%d calls in 450ms with a 100ms floor", d.calls)
	}
}

// call is one request that a recording daemon saw.
type call struct {
	Method, Path string
	Body         map[string]any
}

// recorder answers a worker route with a fixed status and body, and keeps the calls.
type recorder struct {
	mu     sync.Mutex
	calls  []call
	status int
	reply  any
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	r.calls = append(r.calls, call{req.Method, req.URL.Path, body})
	r.mu.Unlock()
	if r.status != 0 {
		w.WriteHeader(r.status)
		json.NewEncoder(w).Encode(map[string]string{"error": "The worker has an open escalation."})
		return
	}
	if r.reply == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	json.NewEncoder(w).Encode(r.reply)
}

func (r *recorder) only(t *testing.T) call {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatalf("calls %+v", r.calls)
	}
	return r.calls[0]
}

func TestWorkerDoneUsesTheGroupKey(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.wd = filepath.Join(t.TempDir(), ".wt", "oversire-12", "cli")
	rec := &recorder{reply: worker.Record{Session: "s1"}}
	fakeDaemon(t, c, rec)
	if err := c.worker(context.Background(), []string{"done", "--verdict", "blocked", "--note", strings.Repeat("n", 400)}); err != nil {
		t.Fatal(err)
	}
	got := rec.only(t)
	note, _ := got.Body["note"].(string)
	if got.Method != "POST" || got.Path != "/v1/workers/OVERSIRE-12/done" || got.Body["verdict"] != "blocked" || len([]rune(note)) != 300 {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(out.String(), "Reported blocked for OVERSIRE-12.") {
		t.Fatalf("output %q", out)
	}
}

func TestWorkerDoneRefusesBadUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	c.wd = t.TempDir()
	fakeDaemon(t, c, &recorder{})
	for _, args := range [][]string{{"done", "--verdict", "done"}, {"done", "--verdict", "pass"}, {"done"}} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestWorkerStartDefaultsTheDirToTheGroup(t *testing.T) {
	c, out, _ := testCLI(t, "")
	root := t.TempDir()
	c.wd = root
	rec := &recorder{reply: worker.Started{Record: worker.Record{Session: "s1"}}}
	fakeDaemon(t, c, rec)
	if err := c.worker(context.Background(), []string{"start", "OVERSIRE-12", "--harness", "codex", "--session", "ov1", "--budget-time", "2h", "--model", "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	got := rec.only(t)
	if got.Path != "/v1/workers" || got.Body["dir"] != filepath.Join(root, ".wt", "oversire-12") || got.Body["key"] != "OVERSIRE-12" ||
		got.Body["harness"] != "codex" || got.Body["model"] != "gpt-5" || got.Body["overseer"] != "ov1" || got.Body["budget"] != float64(2*time.Hour) {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(out.String(), "Started the worker s1 for OVERSIRE-12") {
		t.Fatalf("output %q", out)
	}
}

func TestWorkerStartReadsThePlanFile(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.wd = t.TempDir()
	if err := os.WriteFile(filepath.Join(c.wd, "plan.md"), []byte("# Plan\nBuild it.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{reply: worker.Started{Record: worker.Record{Session: "s1"}, Reused: true}}
	fakeDaemon(t, c, rec)
	dir := filepath.Join(c.wd, "abs", "dir")
	args := []string{"start", "OVERSIRE-12", "--harness", "codex", "--session", "ov1", "--plan", "plan.md", "--dir", dir}
	if err := c.worker(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	got := rec.only(t)
	if got.Body["plan"] != "# Plan\nBuild it.\n" || got.Body["dir"] != dir || got.Body["budget"] != float64(worker.DefaultBudget) {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(out.String(), "is live. You oversee it now.") {
		t.Fatalf("output %q", out)
	}
}

func TestWorkerStartUsesTheSessionOfTheEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "env-1")
	c, _, _ := testCLI(t, "")
	c.wd = t.TempDir()
	rec := &recorder{reply: worker.Started{}}
	fakeDaemon(t, c, rec)
	if err := c.worker(context.Background(), []string{"start", "--harness", "codex", "OVERSIRE-12"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.only(t); got.Body["overseer"] != "env-1" {
		t.Fatalf("%+v", got)
	}
}

func TestWorkerStartRefusesBadUsage(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	c, _, _ := testCLI(t, "")
	c.wd = t.TempDir()
	fakeDaemon(t, c, &recorder{})
	for _, args := range [][]string{{"start"}, {"start", "K-1"}, {"start", "K-1", "--harness", "codex"}, {"start", "--harness", "codex", "--session", "s"},
		{"start", "K-1", "K-2", "--harness", "codex", "--session", "s"}, {"start", "K-1", "--harness", "codex", "--session", "s", "--budget-time", "0s"}} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestWorkerVerbsCallTheirRoutes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
		body map[string]any
		out  string
	}{
		{[]string{"send", "OVERSIRE-12", "Fix the tests."}, "/v1/workers/OVERSIRE-12/send", map[string]any{"text": "Fix the tests."}, "Sent the text"},
		{[]string{"escalate", "OVERSIRE-12", "Which db?"}, "/v1/workers/OVERSIRE-12/escalate", map[string]any{"question": "Which db?"}, "Asked the user"},
		{[]string{"escalate", "s1"}, "/v1/workers/s1/escalate", map[string]any{"question": ""}, "Showed the open ask"},
		{[]string{"answer", "s1", "--permission", "p-1", "--deny"}, "/v1/workers/s1/answer", map[string]any{"permission": "p-1", "allow": false}, "Answered"},
		{[]string{"answer", "s1", "--permission", "p-1", "--allow"}, "/v1/workers/s1/answer", map[string]any{"permission": "p-1", "allow": true}, "Answered"},
		{[]string{"answer", "s1", "--question", "q-1", "Use sqlite."}, "/v1/workers/s1/answer", map[string]any{"question": "q-1", "text": "Use sqlite."}, "Answered"},
		{[]string{"answer", "s1", "Use sqlite.", "--question", "q-1"}, "/v1/workers/s1/answer", map[string]any{"question": "q-1", "text": "Use sqlite."}, "Answered"},
		{[]string{"interrupt", "s1"}, "/v1/workers/s1/interrupt", nil, "Interrupted"},
		{[]string{"stop", "OVERSIRE-12"}, "/v1/workers/OVERSIRE-12/stop", nil, "Stopped"},
	} {
		c, out, _ := testCLI(t, "")
		rec := &recorder{}
		fakeDaemon(t, c, rec)
		if err := c.worker(context.Background(), tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		got := rec.only(t)
		if got.Method != "POST" || got.Path != tc.path || !strings.Contains(out.String(), tc.out) {
			t.Fatalf("%v: %+v %q", tc.args, got, out)
		}
		for k, v := range tc.body {
			if got.Body[k] != v {
				t.Fatalf("%v: body %+v", tc.args, got.Body)
			}
		}
	}
}

func TestWorkerVerbsRefuseBadUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	rec := &recorder{}
	fakeDaemon(t, c, rec)
	for _, args := range [][]string{
		{"send"}, {"send", "K-1"}, {"send", "K-1", " "}, {"send", "K-1", "a", "b"},
		{"escalate"}, {"escalate", "K-1", "a", "b"},
		{"answer", "K-1"}, {"answer", "K-1", "--permission", "p"}, {"answer", "K-1", "--permission", "p", "--allow", "--deny"},
		{"answer", "K-1", "--question", "q"}, {"answer", "K-1", "--question", "q", "--allow", "text"}, {"answer", "K-1", "--permission", "p", "--question", "q", "--allow"},
		{"interrupt"}, {"interrupt", "a", "b"}, {"stop"},
	} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("calls %+v", rec.calls)
	}
}

func TestWorkerVerbGivesTheDaemonError(t *testing.T) {
	c, out, _ := testCLI(t, "")
	fakeDaemon(t, c, &recorder{status: http.StatusConflict})
	var de *daemonError
	if err := c.worker(context.Background(), []string{"escalate", "K-1", "Q?"}); !errors.As(err, &de) || de.Status != 409 || out.Len() != 0 {
		t.Fatalf("%v %q", err, out)
	}
}
