package orca

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// Replies recorded from Orca 1.4.223 on 2026-10-21, cut to the fields that the driver reads.
const (
	coordHandle  = "term_104c03f1-cadf-4275-ad2b-570ff118945e"
	workerHandle = "term_5e5e5e5e-0000-4000-8000-000000000001"

	coordTerminals = `{"ok":true,"result":{"terminals":[
		{"handle":"term_8e5049bf-6ff3-4d95-82df-7af239b270c5","orphaned":true,"title":"oversire-coordinator"},
		{"handle":"` + coordHandle + `","orphaned":false,"worktreeId":"b07c::/w","title":"oversire-coordinator"},
		{"handle":"term_97a9447f-d7ea-4a20-9fd4-e7e05f033240","orphaned":false,"title":"◐ OVERSIRE-178"}],"totalCount":3,"truncated":false}}`
	noCoordTerminals = `{"ok":true,"result":{"terminals":[{"handle":"term_97a9447f-d7ea-4a20-9fd4-e7e05f033240","title":"◐ OVERSIRE-178"}]}}`
	coordCreated     = `{"ok":true,"result":{"terminal":{"handle":"` + coordHandle + `","worktreeId":"","title":"oversire-coordinator","surface":"visible"}}}`
	runBound         = `{"ok":true,"result":{"run":{"id":"run_305c4998aa1e","objective":"OverSire questions","coordinator_handle":"` + coordHandle + `"}}}`
	runUnbound       = `{"ok":true,"result":{"run":null}}`
	runList          = `{"ok":true,"result":{"runs":[
		{"id":"run_f133144ba28e","objective":"OverSire Enhance remote ask test","created_at":"2026-10-20T20:38:28Z"},
		{"id":"run_1111aaaa2222","objective":"OverSire questions","created_at":"2026-10-19T10:00:00Z"},
		{"id":"run_305c4998aa1e","objective":"OverSire questions","created_at":"2026-10-21T02:29:30Z"},
		{"id":"run_legacy_local","objective":"Legacy orchestration state (inspect only)","created_at":"2026-09-20T23:45:01Z"}],"nextCursor":null}}`
	noRuns     = `{"ok":true,"result":{"runs":[],"nextCursor":null}}`
	runCreated = `{"ok":true,"result":{"run":{"id":"run_9999bbbb0000","objective":"OverSire questions"}}}`

	workerStarted = `{"ok":true,"result":{"stage":"ready","task":{"id":"task_aa11bb22cc33"},"dispatch":{"id":"ctx_ee44ff556677","taskId":"task_aa11bb22cc33","dispatchId":"ctx_ee44ff556677"},"agentTerminalHandle":"` + workerHandle + `"}}`
	workerRunning = `{"ok":true,"result":{"worker":{"dispatchId":"ctx_ee44ff556677","workerState":"running","dispatchStatus":"dispatched"}}}`
	workerStopped = `{"ok":true,"result":{"worker":{"dispatchId":"ctx_ee44ff556677","workerState":"stopped","dispatchStatus":"failed"}}}`
	replied       = `{"ok":true,"result":{"message":{"id":"msg_e5bc6764ce37","type":"status","thread_id":"msg_r1r1r1r1r1r1"},"mutation":{"replayed":false}}}`
	fenced        = `{"ok":false,"error":{"code":"consumer_fenced","message":"The terminal is not the consumer of the Run."}}`
)

var handle = runtime.WorkerHandle{Dispatch: "ctx_ee44ff556677", Task: "task_aa11bb22cc33", Terminal: workerHandle}

func ask(id string, round int, at string) string {
	body := `{\"round\":` + string(rune('0'+round)) + `,\"questions\":[{\"id\":\"q1\",\"type\":\"single\",\"text\":\"Which sync?\",\"options\":[\"Plane\",\"GitHub\"]}]}`
	return `{"id":"` + id + `","run_id":"run_305c4998aa1e","from_handle":"` + workerHandle + `","to_handle":"run:run_305c4998aa1e","subject":"Question","body":"` + body + `","type":"question","thread_id":null,"payload":null,"created_at":"` + at + `"}`
}

func replyRow(id, to, body string) string {
	b, _ := json.Marshal(body)
	return `{"id":"` + id + `","run_id":"run_305c4998aa1e","from_handle":"` + coordHandle + `","to_handle":"` + workerHandle + `","subject":"Re: Question","body":` + string(b) + `,"type":"status","thread_id":"` + to + `","created_at":"2026-10-21T02:31:00Z"}`
}

const doneRow = `{"id":"msg_d0d0d0d0d0d0","run_id":"run_305c4998aa1e","from_handle":"` + workerHandle + `","to_handle":"run:run_305c4998aa1e","subject":"Enhanced","body":"{\"verdict\":\"clear\",\"description\":\"## Goal\\nSync.\"}","type":"worker_done","thread_id":null,"payload":"{\"taskId\":\"task_aa11bb22cc33\",\"dispatchId\":\"ctx_ee44ff556677\",\"outcome\":\"succeeded\"}","created_at":"2026-10-21T02:40:00Z"}`

func inbox(rows ...string) string {
	return `{"ok":true,"result":{"messages":[` + strings.Join(rows, ",") + `]}}`
}

func workerSetup() (*Orca, *replay) {
	o, r := setup()
	o.now = func() time.Time { return time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC) }
	r.on("terminal list", coordTerminals)
	r.on("orchestration run-current", runBound)
	return o, r
}

func TestStartWorkerInTheCoordinatorRun(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration worker-start", workerStarted)
	h, err := o.StartWorker(context.Background(), wire.WorkerStart{Spec: "Enhance it", Title: "Enhance TREBI-9", Path: "/w", Agent: "claude"})
	if err != nil || h != handle {
		t.Fatal(h, err)
	}
	want := []string{"orchestration", "worker-start", "--spec", "Enhance it", "--task-title", "Enhance TREBI-9", "--worktree", "path:/w",
		"--agent", "claude", "--run", "run_305c4998aa1e", "--from", coordHandle, "--json"}
	if got := r.args("orchestration worker-start"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatal(got)
	}
	if _, err := o.StartWorker(context.Background(), wire.WorkerStart{Spec: "x", Path: "/w", Agent: "claude; rm"}); err == nil {
		t.Fatal("a bad agent passed")
	}
	// The second start uses the coordinator again.
	if len(r.args("terminal list")) != 1 {
		t.Fatal(r.args("terminal list"))
	}
}

func TestCoordinatorUsesTheNewestRun(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration run-current", runUnbound)
	r.on("orchestration run-list", runList)
	r.on("orchestration run-use", `{"ok":true,"result":{}}`)
	c, err := o.coordinatorFor(context.Background())
	if err != nil || c != (coordinator{terminal: coordHandle, run: "run_305c4998aa1e"}) {
		t.Fatal(c, err)
	}
	if len(r.args("orchestration run-create")) != 0 {
		t.Fatal("made a new Run")
	}
}

func TestCoordinatorIsMadeWhenMissing(t *testing.T) {
	o, r := workerSetup()
	r.on("terminal list", noCoordTerminals)
	r.on("terminal create", coordCreated)
	r.on("orchestration run-current", runUnbound)
	r.on("orchestration run-list", noRuns)
	r.on("orchestration run-create", runCreated)
	c, err := o.coordinatorFor(context.Background())
	if err != nil || c != (coordinator{terminal: coordHandle, run: "run_9999bbbb0000"}) {
		t.Fatal(c, err)
	}
	if got := r.args("orchestration run-create"); !slices.Equal(got[0][:4], []string{"orchestration", "run-create", "--objective", "OverSire questions"}) {
		t.Fatal(got)
	}
}

func TestStaleCoordinatorIsFoundAgain(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration reply", fenced, replied)
	if err := o.Reply(context.Background(), "msg_r1r1r1r1r1r1", "Plane"); err != nil {
		t.Fatal(err)
	}
	if len(r.args("orchestration reply")) != 2 || len(r.args("terminal list")) != 2 {
		t.Fatal(r.calls)
	}
}

func TestWorkerTwoRoundsThenDone(t *testing.T) {
	o, r := workerSetup()
	ctx := context.Background()
	r1 := ask("msg_r1r1r1r1r1r1", 1, "2026-10-21T02:31:00Z")
	a1 := replyRow("msg_a1a1a1a1a1a1", "msg_r1r1r1r1r1r1", `{"round":1,"answers":{"q1":"Plane"}}`)
	r2 := ask("msg_r2r2r2r2r2r2", 2, "2026-10-21T02:35:00Z")
	a2 := replyRow("msg_a2a2a2a2a2a2", "msg_r2r2r2r2r2r2", `{"round":2,"answers":{"q1":"GitHub"}}`)
	r.on("orchestration worker-show", workerRunning)

	r.on("orchestration inbox", inbox(r1))
	m, err := o.Mail(ctx)
	if err != nil || len(m.Asks) != 1 || m.Asks[0].ID != "msg_r1r1r1r1r1r1" || m.Asks[0].Reply != nil || !m.Workers[workerHandle] {
		t.Fatalf("%+v %v", m, err)
	}
	r.on("orchestration inbox", inbox(r1))
	if st, err := o.WorkerState(ctx, handle); err != nil || st.State != wire.WorkerRunning {
		t.Fatal(st, err)
	}

	r.on("orchestration inbox", inbox(r2, a1, r1))
	if m, _ = o.Mail(ctx); len(m.Asks) != 1 || m.Asks[0].ID != "msg_r2r2r2r2r2r2" || m.Asks[0].Reply != nil {
		t.Fatalf("round 2: %+v", m)
	}

	r.on("orchestration inbox", inbox(doneRow, a2, r2, a1, r1))
	if m, _ = o.Mail(ctx); len(m.Asks) != 1 || m.Asks[0].Reply == nil || *m.Asks[0].Reply != `{"round":2,"answers":{"q1":"GitHub"}}` {
		t.Fatalf("answered: %+v", m)
	}
	r.on("orchestration inbox", inbox(doneRow, a2, r2, a1, r1))
	st, err := o.WorkerState(ctx, handle)
	if err != nil || st != (wire.WorkerState{State: wire.WorkerDone, Outcome: "succeeded", Body: `{"verdict":"clear","description":"## Goal\nSync."}`}) {
		t.Fatal(st, err)
	}
}

func TestStoppedWorkerWithNoResultEnds(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration inbox", inbox())
	r.on("orchestration worker-show", workerStopped)
	st, err := o.WorkerState(context.Background(), runtime.WorkerHandle{Dispatch: "ctx_ee44ff556677"})
	if err != nil || st != (wire.WorkerState{State: wire.WorkerEnded, Reason: "stopped"}) {
		t.Fatal(st, err)
	}
}

func TestOldAskIsNotOpen(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration inbox", inbox(ask("msg_old", 1, "2026-10-18T02:00:00Z")))
	if m, err := o.Mail(context.Background()); err != nil || len(m.Asks) != 0 || !m.Workers[workerHandle] {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestStopAndReleaseWorker(t *testing.T) {
	o, r := workerSetup()
	r.on("orchestration worker-stop", `{"ok":true,"result":{}}`)
	r.on("orchestration worker-release", `{"ok":true,"result":{}}`)
	ctx := context.Background()
	if err := o.StopWorker(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if err := o.ReleaseWorker(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.args("orchestration worker-release")[0], []string{"orchestration", "worker-release", "--dispatch", "ctx_ee44ff556677", "--json"}) {
		t.Fatal(r.calls)
	}
}
