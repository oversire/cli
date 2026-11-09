package capture

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

type book map[string]wire.DaemonSession

func (b book) Get(id string) (wire.DaemonSession, bool) { s, ok := b[id]; return s, ok }

func open(t *testing.T) (*events.Store, *Writer) {
	t.Helper()
	st, err := events.Open(filepath.Join(t.TempDir(), "events.db"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runs := func(run string) string {
		if run == "r1" {
			return "OVERSIRE-9"
		}
		return ""
	}
	return st, NewWriter(st, events.NewResolver(st, runs), nil, "mini", "me@example.com", nil)
}

func item(t *testing.T, st *events.Store, key string) []wire.Event {
	t.Helper()
	p, err := st.Query(context.Background(), wire.EventQuery{Item: key, Tree: true})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := wire.LoadEventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Events {
		if pr := cat.Problems(e.EventDraft); len(pr) > 0 {
			t.Errorf("%s: %v", e.ID, pr)
		}
	}
	return p.Events
}

func kinds(rows []wire.Event) []string {
	var out []string
	for _, e := range rows {
		out = append(out, e.Kind)
	}
	return out
}

func TestWriterFillsTheRow(t *testing.T) {
	st, w := open(t)
	if _, err := st.SetLinks(context.Background(), []wire.ItemLink{{Item: "OVERSIRE-380", Parent: "OVERSIRE-377"}}); err != nil {
		t.Fatal(err)
	}
	w.sessions = book{"s1": {ID: "s1", Key: "work:OVERSIRE-380", Harness: "claude", Model: "opus", Repo: "oversire/cli", Cwd: "/x"}}
	w.Add("s1", wire.EventDraft{ID: "t:1", Kind: wire.KindTurnStarted, ActorType: wire.ActorAgent, Via: "hook", Summary: "Turn 1 started",
		Payload: json.RawMessage(`{"turn":1}`)})
	w.Add("", wire.EventDraft{ID: "t:2", Kind: wire.KindPromptSubmitted, Item: "OVERSIRE-380", ActorType: wire.ActorHuman, Via: "hook", Summary: "hi", Private: true,
		Payload: json.RawMessage(`{"turn":1}`)})
	delete(w.sessions.(book), "s1")
	w.Add("s1", wire.EventDraft{ID: "t:3", Kind: wire.KindSessionEnded, ActorType: wire.ActorSystem, Via: "hook", Summary: "Session ended"})
	w.Add("", wire.EventDraft{ID: "t:bad", Kind: "nodot", ActorType: wire.ActorHuman, Via: "hook"})
	w.Flush(context.Background())

	rows := item(t, st, "OVERSIRE-377")
	if len(rows) != 3 {
		t.Fatalf("rows %v", kinds(rows))
	}
	if e := rows[2]; e.Item != "OVERSIRE-380" || e.Session != "work:OVERSIRE-380" || e.Actor != "oversire" {
		t.Errorf("end row of a dropped session %+v", e.EventDraft)
	}
	a := rows[0]
	if a.Item != "OVERSIRE-380" || a.ParentItem != "OVERSIRE-377" || a.Session != "work:OVERSIRE-380" || a.Repo != "oversire/cli" ||
		a.Machine != "mini" || a.Actor != "claude/opus" || a.OnBehalf != "me@example.com" || a.At.IsZero() {
		t.Errorf("filled %+v", a.EventDraft)
	}
	if rows[1].Actor != "me@example.com" {
		t.Errorf("human actor %q", rows[1].Actor)
	}
}

func TestSessionRows(t *testing.T) {
	st, w := open(t)
	r := NewSessionRows(w)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s := wire.DaemonSession{ID: "s2", Key: "work:OVERSIRE-12", Harness: "codex", Runtime: "orca", Sources: []string{"orca"}, StartedAt: &t0}
	step := func(min int, phase wire.SessionPhase, reason wire.SessionReason, action string) {
		s.State, s.Reason, s.StateSince = phase, reason, t0.Add(time.Duration(min)*time.Minute)
		r.OnSend(s, cmpOr(action, string(phase)), s.StateSince)
	}
	step(0, wire.PhaseWorking, "", "")
	step(5, wire.PhaseWaiting, wire.ReasonPermission, "")
	step(7, wire.PhaseWorking, "", "")
	step(9, wire.PhaseDone, "", "")
	step(10, wire.PhaseDone, "", "removed")
	hooked := s
	hooked.ID, hooked.Sources = "s3", []string{string(wire.SessionSourceHook)}
	r.OnSend(hooked, string(wire.PhaseWorking), t0)
	w.Flush(context.Background())

	got := kinds(item(t, st, "OVERSIRE-12"))
	want := []string{wire.KindSessionStarted, wire.KindTurnStarted, wire.KindWaitStarted, wire.KindWaitEnded, wire.KindTurnEnded, wire.KindSessionEnded}
	if len(got) != len(want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds %v, want %v", got, want)
		}
	}
}

func TestRunbookRows(t *testing.T) {
	st, w := open(t)
	r := NewRunbookRows(w)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(4 * time.Minute)
	run := wire.RunbookRun{ID: "r1", Item: wire.ItemRef{Key: "OVERSIRE-9"}, Machine: "mini", Started: t0, Updated: t0,
		Runbook: wire.Runbook{ID: "ship", Name: "Ship", Steps: []wire.RunbookStep{{ID: "plan", Name: "Plan"}, {ID: "code"}}},
		Steps:   []wire.StepRun{{Step: "plan", Round: 1, State: wire.StepWaits}}}
	r.OnChange(run)
	run.Steps[0].State, run.Steps[0].Started = wire.StepWorking, &t0
	r.OnChange(run)
	r.OnChange(run)
	run.Steps[0].State, run.Steps[0].Ended = wire.StepPass, &t1
	run.Steps = append(run.Steps, wire.StepRun{Step: "code", Round: 1, State: wire.StepSkipped})
	r.OnChange(run)
	w.Flush(context.Background())

	rows := item(t, st, "OVERSIRE-9")
	got := kinds(rows)
	want := []string{wire.KindPlanStarted, wire.KindRunbookStepStarted, wire.KindRunbookStepEnded, wire.KindRunbookStepEnded}
	if len(got) != len(want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	var p struct {
		State       string `json:"state"`
		DurationSec int    `json:"durationSec"`
		Index       int    `json:"index"`
	}
	if err := json.Unmarshal(rows[2].Payload, &p); err != nil || p.State != "pass" || p.DurationSec != 240 {
		t.Errorf("ended payload %s", rows[2].Payload)
	}
	if err := json.Unmarshal(rows[3].Payload, &p); err != nil || p.Index != 1 {
		t.Errorf("skipped payload %s", rows[3].Payload)
	}
}

func TestSessionStartedNamesTheAgentAndTheEnvKeys(t *testing.T) {
	st, w := open(t)
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := wire.DaemonSession{ID: "s4", Key: "work:OVERSIRE-13", Harness: "claude", Runtime: "oversire", Profile: "claude-glm",
		EnvKeys: []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"}, Sources: []string{"wire"}, StartedAt: &t0, State: wire.PhaseWorking, StateSince: t0}
	NewSessionRows(w).OnSend(s, string(wire.PhaseWorking), t0)
	w.Flush(context.Background())
	rows := item(t, st, "OVERSIRE-13")
	if len(rows) == 0 || rows[0].Kind != wire.KindSessionStarted {
		t.Fatalf("rows %v", kinds(rows))
	}
	var pl struct {
		Agent   string   `json:"agent"`
		Profile string   `json:"profile"`
		EnvKeys []string `json:"envKeys"`
	}
	if err := json.Unmarshal(rows[0].Payload, &pl); err != nil || pl.Agent != "claude-glm" || pl.Profile != "claude-glm" || len(pl.EnvKeys) != 2 || pl.EnvKeys[0] != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("payload %s, %v", rows[0].Payload, err)
	}
}

func TestWorkerRowsWriteEachChangeOnce(t *testing.T) {
	st, w := open(t)
	r := NewWorkerRows(w)
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	rec := worker.Record{Session: "w1", Key: "work:OVERSIRE-12", Harness: "codex", Model: "gpt-5", Overseer: "ov1"}
	started := worker.Change{Kind: wire.KindWorkerStarted, Record: rec, Actor: worker.ActorOverseer, Session: "ov1", At: at,
		Payload: map[string]any{"session": "w1", "harness": "codex", "model": "gpt-5", "reused": false}}
	r.OnChange(started)
	r.OnChange(started)
	r.OnChange(worker.Change{Kind: wire.KindWorkerAnswered, Record: rec, Actor: worker.ActorOverseer, Session: "ov1", At: at.Add(time.Minute),
		Payload: map[string]any{"request": "permission", "allow": true}})
	r.OnChange(worker.Change{Kind: wire.KindWorkerNudged, Record: rec, Actor: worker.ActorOverseer, Session: "ov1", At: at.Add(2 * time.Minute),
		Payload: map[string]any{"how": "interrupt"}})
	r.OnChange(worker.Change{Kind: wire.KindWorkerEscalated, Record: rec, Actor: worker.ActorSystem, Session: "ov1", At: at.Add(3 * time.Minute),
		Payload: map[string]any{"reason": "budget", "question": "Answer to give it 3 h more."}})
	r.OnChange(worker.Change{Kind: wire.KindWorkerDone, Record: rec, Actor: worker.ActorWorker, Session: "w1", At: at.Add(4 * time.Minute),
		Payload: map[string]any{"verdict": "done", "note": "Tests pass."}})
	w.Flush(context.Background())

	rows := item(t, st, "OVERSIRE-12")
	want := []string{wire.KindWorkerStarted, wire.KindWorkerAnswered, wire.KindWorkerNudged, wire.KindWorkerEscalated, wire.KindWorkerDone}
	if got := kinds(rows); len(got) != len(want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for i, k := range want {
		if rows[i].Kind != k {
			t.Fatalf("kinds %v, want %v", kinds(rows), want)
		}
	}
	checks := []struct {
		actorType               wire.ActorType
		actor, session, summary string
	}{
		{wire.ActorAgent, "claude", "ov1", "Worker started: codex"},
		{wire.ActorAgent, "claude", "ov1", "Worker permission answered"},
		{wire.ActorAgent, "claude", "ov1", "Worker interrupted"},
		{wire.ActorSystem, "oversire", "ov1", "Worker escalated to you: time budget"},
		{wire.ActorAgent, "codex/gpt-5", "w1", "Worker done"},
	}
	for i, c := range checks {
		row := rows[i]
		if row.ActorType != c.actorType || row.Actor != c.actor || row.Session != c.session || row.Summary != c.summary {
			t.Errorf("row %d %s: %s %s %s %q", i, row.Kind, row.ActorType, row.Actor, row.Session, row.Summary)
		}
	}
	if string(rows[4].Payload) == "" || !strings.Contains(string(rows[4].Payload), "Tests pass.") || strings.Contains(rows[4].Summary, "Tests") {
		t.Errorf("done payload %s summary %q", rows[4].Payload, rows[4].Summary)
	}
}
