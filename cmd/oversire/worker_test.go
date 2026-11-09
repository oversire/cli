package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

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
	d := &waitDaemon{waits: []worker.WakeReason{worker.WakeNone}, header: hdr}
	fakeDaemon(t, c, d)
	err := c.worker(context.Background(), []string{"wait", "OVERSIRE-12", "--timeout", "1s"})
	var e *exitError
	if !errors.As(err, &e) || e.code != 3 || !errors.Is(err, errSilent) {
		t.Fatalf("err %v", err)
	}
	got := out.String()
	if got != "work:OVERSIRE-12 | codex | working | no new turns\n" || len(d.puts) != 0 {
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
