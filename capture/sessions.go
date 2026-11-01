package capture

import (
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

// SessionRows makes the rows of the sessions that have no hooks (the Orca screen, the oversire runtime) from the changes of
// the session book: session.started and ended, turn.started and ended, wait.started and ended. A session with hooks gets
// its rows from the hook calls.
type SessionRows struct {
	w  *Writer
	mu sync.Mutex
	by map[string]*track
}

type track struct {
	state          wire.SessionPhase
	turns          int
	turnID, waitID string
	waitReason     string
	turnAt, waitAt time.Time
}

func NewSessionRows(w *Writer) *SessionRows {
	return &SessionRows{w: w, by: map[string]*track{}}
}

// OnSend takes a change of the book (sessions.Book.OnSend). It runs under the lock of the book.
func (r *SessionRows) OnSend(s wire.DaemonSession, action string, at time.Time) {
	if slices.Contains(s.Sources, string(wire.SessionSourceHook)) || action == "question" {
		return
	}
	r.mu.Lock()
	rows := r.rows(s, action, at)
	r.mu.Unlock()
	for _, d := range rows {
		r.w.AddFor(s, d)
	}
}

func (r *SessionRows) rows(s wire.DaemonSession, action string, at time.Time) []wire.EventDraft {
	t := r.by[s.ID]
	var out []wire.EventDraft
	mk := func(kind string, actor wire.ActorType, summary string, payload map[string]any) wire.EventDraft {
		d := wire.EventDraft{ID: ID("session", s.ID, strconv.FormatInt(s.StateSince.UnixMilli(), 10), kind), At: at, Kind: kind,
			ActorType: actor, Via: "daemon", Summary: summary}
		if actor == wire.ActorAgent {
			d.Actor = AgentActor(s.Harness, s.Model)
		}
		if payload != nil {
			d.Payload, _ = json.Marshal(payload)
		}
		return d
	}
	if t == nil {
		if action == "removed" {
			return nil
		}
		t = &track{}
		r.by[s.ID] = t
		started := at
		if s.StartedAt != nil {
			started = *s.StartedAt
		}
		pl := map[string]any{"harness": cmpOr(s.Harness, "agent"), "runtime": s.Runtime}
		if s.Model != "" {
			pl["model"] = s.Model
		}
		if s.Cwd != "" {
			pl["cwd"] = s.Cwd
		}
		if s.Profile != "" {
			pl["profile"] = s.Profile
		}
		d := mk(wire.KindSessionStarted, wire.ActorAgent, "Session started", pl)
		d.ID, d.At = ID("session", s.ID, strconv.FormatInt(started.UnixMilli(), 10), "started"), started
		out = append(out, d)
	}
	endWait := func(actor wire.ActorType) {
		if t.waitID == "" {
			return
		}
		d := mk(wire.KindWaitEnded, actor, "Wait ended: "+t.waitReason, map[string]any{"reason": t.waitReason, "waitedSec": secs(at.Sub(t.waitAt))})
		d.Corr, t.waitID = t.waitID, ""
		out = append(out, d)
	}
	startWait := func(reason string) {
		if t.waitID != "" {
			return
		}
		d := mk(wire.KindWaitStarted, wire.ActorAgent, "Waits for "+reason, map[string]any{"reason": reason})
		d.Corr = d.ID
		t.waitID, t.waitReason, t.waitAt = d.ID, reason, at
		out = append(out, d)
	}
	endTurn := func(interrupted bool) {
		if t.turnID == "" {
			return
		}
		pl := map[string]any{"turn": t.turns, "tools": 0, "durationSec": secs(at.Sub(t.turnAt))}
		if interrupted {
			pl["interrupted"] = true
		}
		d := mk(wire.KindTurnEnded, wire.ActorAgent, "Turn "+strconv.Itoa(t.turns)+" ended", pl)
		d.Corr, t.turnID = t.turnID, ""
		out = append(out, d)
	}
	switch wire.SessionPhase(action) {
	case wire.PhaseWorking:
		endWait(wire.ActorHuman)
		if t.turnID == "" {
			t.turns++
			d := mk(wire.KindTurnStarted, wire.ActorAgent, "Turn "+strconv.Itoa(t.turns)+" started", map[string]any{"turn": t.turns, "steer": t.turns > 1})
			d.Corr = d.ID
			t.turnID, t.turnAt = d.ID, at
			out = append(out, d)
		}
	case wire.PhaseWaiting:
		switch s.Reason {
		case wire.ReasonQuestion:
			startWait("question")
		case wire.ReasonPermission:
			startWait("permission")
		default:
			startWait("input")
		}
	case wire.PhaseDone, wire.PhaseFailed:
		endWait(wire.ActorAgent)
		endTurn(s.Reason == wire.ReasonInterrupted)
		if s.Reason == wire.ReasonQuestion {
			startWait("question")
		}
	}
	if action == "removed" {
		endWait(wire.ActorSystem)
		endTurn(false)
		out = append(out, mk(wire.KindSessionEnded, wire.ActorSystem, "Session ended", map[string]any{"turns": t.turns}))
		delete(r.by, s.ID)
	}
	t.state = s.State
	return out
}

func secs(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d.Round(time.Second) / time.Second)
}
