package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// setPhase changes the state of a session of the book, as the runtime does.
func (b *book) setPhase(id string, state wire.SessionPhase, reason wire.SessionReason) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.byID[id]
	s.State, s.Reason = state, reason
	b.byID[id] = s
}

func waitRig(t *testing.T, history []wire.SessionTurn) *rig {
	t.Helper()
	r := newRig(t, history, live("s1", "work:OVERSIRE-12"))
	r.e.Poll = 5 * time.Millisecond
	return r
}

func ago(t wire.SessionTurn, d time.Duration) wire.SessionTurn {
	t.At = t0.Add(-d)
	return t
}

func TestWaitWakesOnTurnEnd(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	go func() {
		time.Sleep(30 * time.Millisecond)
		r.book.setPhase("s1", wire.PhaseWaiting, wire.ReasonTurnEnd)
	}()
	start := time.Now()
	got, err := r.e.Wait(context.Background(), "OVERSIRE-12", MaxWait)
	if err != nil || got.Reason != WakeInput || got.Header.Session != "s1" || got.Header.State != wire.PhaseWaiting || got.Header.Reason != wire.ReasonTurnEnd {
		t.Fatalf("%+v %v", got, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Wait did not return at the change")
	}
}

func TestWaitWakesOnPermission(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	r.book.tools["s1"] = [2]string{"Bash", "rm -rf x"}
	r.book.setPhase("s1", wire.PhaseWaiting, wire.ReasonPermission)
	got, err := r.e.Wait(context.Background(), "s1", time.Second)
	if err != nil || got.Reason != WakePermission || got.Header.Request != "permission Bash rm -rf x" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitWakesOnQuestion(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	s := r.book.byID["s1"]
	s.Question = "q-7"
	r.book.byID["s1"] = s
	r.book.setPhase("s1", wire.PhaseWaiting, wire.ReasonQuestion)
	got, err := r.e.Wait(context.Background(), "s1", time.Second)
	if err != nil || got.Reason != WakeQuestion || got.Header.Request != "question q-7" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitWakesOnDoneAndFailed(t *testing.T) {
	for _, c := range []struct {
		phase wire.SessionPhase
		want  WakeReason
	}{{wire.PhaseDone, WakeDone}, {wire.PhaseFailed, WakeFailed}} {
		r := waitRig(t, turnsTo(3))
		r.book.setPhase("s1", c.phase, "")
		if got, err := r.e.Wait(context.Background(), "s1", time.Second); err != nil || got.Reason != c.want {
			t.Fatalf("%s: %+v %v", c.phase, got, err)
		}
	}
}

func TestWaitWakesOnStall(t *testing.T) {
	h := turnsTo(3)
	h[2] = ago(h[2], 11*time.Minute)
	r := waitRig(t, h)
	got, err := r.e.Wait(context.Background(), "s1", time.Second)
	if err != nil || got.Reason != WakeStall || got.Header.State != wire.PhaseWorking {
		t.Fatalf("%+v %v", got, err)
	}
	// A turn of 9 min is no stall.
	h[2] = ago(h[2], 9*time.Minute)
	if got, err = r.e.Wait(context.Background(), "s1", 30*time.Millisecond); err != nil || got.Reason != WakeNone {
		t.Fatalf("fresh %+v %v", got, err)
	}
}

func TestWaitSkipsAStallDuringATool(t *testing.T) {
	h := turnsTo(3)
	h[2] = ago(toolTurn(3, wire.ToolRunning), 12*time.Minute)
	r := waitRig(t, h)
	got, err := r.e.Wait(context.Background(), "s1", 50*time.Millisecond)
	if err != nil || got.Reason != WakeNone {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitWakesOnALongTool(t *testing.T) {
	h := turnsTo(3)
	h[2] = ago(toolTurn(3, wire.ToolRunning), 31*time.Minute)
	r := waitRig(t, h)
	got, err := r.e.Wait(context.Background(), "s1", time.Second)
	if err != nil || got.Reason != WakeStall {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitStallsOnASessionWithNoTurns(t *testing.T) {
	r := waitRig(t, nil)
	s := r.book.byID["s1"]
	s.StateSince = t0.Add(-11 * time.Minute)
	r.book.byID["s1"] = s
	if got, err := r.e.Wait(context.Background(), "s1", time.Second); err != nil || got.Reason != WakeStall {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitGivesNoneAtMax(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	start := time.Now()
	got, err := r.e.Wait(context.Background(), "s1", 60*time.Millisecond)
	if err != nil || got.Reason != WakeNone || got.Header.Session != "s1" || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestWaitStopsWithTheContext(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.e.Wait(ctx, "s1", MaxWait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestWaitRefusesBadInput(t *testing.T) {
	r := waitRig(t, turnsTo(3))
	var e *Error
	for _, max := range []time.Duration{60 * time.Second, 0, -time.Second} {
		if _, err := r.e.Wait(context.Background(), "s1", max); !errors.As(err, &e) || e.Kind != Invalid {
			t.Fatalf("max %s: %v", max, err)
		}
	}
	if _, err := r.e.Wait(context.Background(), "nope", time.Second); !errors.As(err, &e) || e.Kind != NotFound {
		t.Fatalf("unknown: %v", err)
	}
}

func userTurn(seq int, o wire.TurnOrigin, text string) wire.SessionTurn {
	return wire.SessionTurn{Seq: seq, Role: wire.RoleUser, At: t0, Text: text, Origin: o}
}

func TestBriefGivesThePlanAndTheDecisions(t *testing.T) {
	h := []wire.SessionTurn{
		userTurn(1, "", "Build the thing."),
		assistant(2),
		userTurn(3, wire.OriginOverseer, "Use sqlite."),
		userTurn(4, wire.OriginSystem, "daemon note"),
		userTurn(5, "", strings.Repeat("x", 2*maxNote)),
		{Seq: 6, Role: wire.RoleAssistant, At: t0, Text: "Done with the first part."},
		toolTurn(7, wire.ToolDone),
	}
	r := waitRig(t, h)
	r.book.tools["s1"] = [2]string{"Bash", "go test"}
	r.book.setPhase("s1", wire.PhaseWaiting, wire.ReasonPermission)
	b, err := r.e.Brief(context.Background(), "OVERSIRE-12")
	if err != nil {
		t.Fatal(err)
	}
	if b.Plan != "Build the thing." || len(b.Notes) != 2 || b.Notes[0].Text != "Use sqlite." || len(b.Notes[1].Text) != maxNote {
		t.Fatalf("%+v", b)
	}
	if b.Last != "Done with the first part." || len(b.Failed) != 0 || b.Header.First != 1 || b.Header.Last != 7 || b.Header.Request != "permission Bash go test" {
		t.Fatalf("%+v", b)
	}
}

func TestBriefGivesTheLastFailedTools(t *testing.T) {
	var h []wire.SessionTurn
	for i := 1; i <= 7; i++ {
		h = append(h, toolTurn(i, wire.ToolFailed))
	}
	b, err := waitRig(t, h).e.Brief(context.Background(), "s1")
	if err != nil || len(b.Failed) != 5 || b.Failed[0].Seq != 3 || b.Failed[4].Seq != 7 || b.Plan != "" || b.Notes == nil {
		t.Fatalf("%+v %v", b, err)
	}
}
