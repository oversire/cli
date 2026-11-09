package capture

import (
	"fmt"
	"sync"

	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

// WorkerRows writes the worker.* rows from the changes of the worker engine (worker.Engine notify).
// The id has the time of the change, so a change that comes again does not write a second row.
type WorkerRows struct {
	w    *Writer
	mu   sync.Mutex
	seen map[string]bool
}

func NewWorkerRows(w *Writer) *WorkerRows {
	return &WorkerRows{w: w, seen: map[string]bool{}}
}

// OnChange writes one row for the change.
func (r *WorkerRows) OnChange(c worker.Change) {
	d := wire.EventDraft{
		ID:   ID("worker", c.Record.Session, c.Kind, fmt.Sprint(c.At.UnixNano())),
		Kind: c.Kind, Item: c.Record.Item(), Session: c.Session, At: c.At, Via: "cli",
		Summary: workerSummary(c), Payload: payload(c.Payload),
	}
	switch c.Actor {
	case worker.ActorSystem:
		d.ActorType, d.Actor = wire.ActorSystem, "oversire"
	case worker.ActorWorker:
		d.ActorType, d.Actor = wire.ActorAgent, AgentActor(c.Record.Harness, c.Record.Model)
	default:
		d.ActorType, d.Actor = wire.ActorAgent, AgentActor("claude", "")
	}
	r.mu.Lock()
	again := r.seen[d.ID]
	r.seen[d.ID] = true
	r.mu.Unlock()
	if !again {
		r.w.Add(c.Session, d)
	}
}

// workerSummary is a fixed text. The question and the note go only in the payload.
func workerSummary(c worker.Change) string {
	switch c.Kind {
	case wire.KindWorkerStarted:
		return "Worker started: " + c.Record.Harness
	case wire.KindWorkerAnswered:
		return fmt.Sprintf("Worker %v answered", c.Payload["request"])
	case wire.KindWorkerNudged:
		if c.Payload["how"] == "interrupt" {
			return "Worker interrupted"
		}
		return "Worker sent a message"
	case wire.KindWorkerEscalated:
		switch c.Payload["reason"] {
		case string(worker.ReasonFallback):
			return "Worker escalated to you: no action in 10 min"
		case string(worker.ReasonBudget):
			return "Worker escalated to you: time budget"
		}
		return "Worker escalated to you"
	case wire.KindWorkerDone:
		if c.Payload["verdict"] == "blocked" {
			return "Worker blocked"
		}
		return "Worker done"
	}
	return c.Kind
}
