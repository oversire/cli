package capture

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

// RunbookRows writes plan.started and the runbook.step_* rows from the changes of the runs (runbook.Engine notify).
// The ids are fixed, so the store drops a row that comes again after a restart.
type RunbookRows struct {
	w    *Writer
	mu   sync.Mutex
	seen map[string]bool // run, run/step/round/started, run/step/round/ended
}

func NewRunbookRows(w *Writer) *RunbookRows {
	return &RunbookRows{w: w, seen: map[string]bool{}}
}

// OnChange writes a row for each new run, step start and step end.
func (r *RunbookRows) OnChange(run wire.RunbookRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	book := cmpOr(run.Runbook.Name, run.Runbook.ID)
	base := wire.EventDraft{Item: run.Item.Key, Repo: run.Repo, Machine: run.Machine,
		ActorType: wire.ActorSystem, Actor: "oversire", Via: "runbook"}
	if r.once(run.ID) {
		d := base
		d.ID, d.Kind, d.At = ID("runbook", run.ID, "started"), wire.KindPlanStarted, run.Started
		d.Summary = "Runbook " + book
		d.Payload = payload(map[string]any{"runbook": book, "run": run.ID})
		r.w.Add("", d)
	}
	for _, s := range run.Steps {
		name := s.Step
		if st := run.Runbook.Step(s.Step); st != nil && st.Name != "" {
			name = st.Name
		}
		round := fmt.Sprint(s.Round)
		p := map[string]any{"run": run.ID, "runbook": book, "step": s.Step, "index": max(run.Runbook.Index(s.Step), 0)}
		d := base
		d.Session = s.DaemonSession
		if (s.Started != nil || s.State.Live()) && r.once(ID(run.ID, s.Step, round, "started")) {
			d.ID, d.Kind, d.At = ID("runbook", run.ID, s.Step, round, "started"), wire.KindRunbookStepStarted, timeOr(s.Started, run.Updated)
			d.Summary = "Step " + name
			d.Payload = payload(p)
			r.w.Add(s.DaemonSession, d)
		}
		if (s.Ended != nil || (!s.State.Live() && s.State != wire.StepWaits)) && r.once(ID(run.ID, s.Step, round, "ended")) {
			d.ID, d.Kind, d.At = ID("runbook", run.ID, s.Step, round, "ended"), wire.KindRunbookStepEnded, timeOr(s.Ended, run.Updated)
			d.Summary = fmt.Sprintf("Step %s: %s", name, s.State)
			if s.Note != "" {
				d.Summary += ". " + s.Note
			}
			p["state"] = string(s.State)
			if s.Started != nil && s.Ended != nil {
				p["durationSec"] = int(s.Ended.Sub(*s.Started).Seconds())
			}
			d.Payload = payload(p)
			r.w.Add(s.DaemonSession, d)
		}
	}
}

// once is true the first time it sees key.
func (r *RunbookRows) once(key string) bool {
	if r.seen[key] {
		return false
	}
	r.seen[key] = true
	return true
}

func timeOr(t *time.Time, or time.Time) time.Time {
	if t != nil {
		return *t
	}
	return or
}

func payload(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}
