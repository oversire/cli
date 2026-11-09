package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// The limits that Run checks.
const (
	DefaultTick   = 30 * time.Second
	FallbackAfter = 10 * time.Minute // an ask with no action of the overseer goes to the user after this time
	budgetAsk     = "The worker used its time budget. Answer to give it 3 h more."
)

// Run checks the workers on each tick until ctx is done. A worker that waits too long for its overseer asks the user.
// A worker that uses its time budget stops and asks the user.
func (e *Engine) Run(ctx context.Context) error {
	e.tick(ctx)
	t := time.NewTicker(e.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.tick(ctx)
		}
	}
}

func (e *Engine) tick(ctx context.Context) {
	e.mu.Lock()
	recs, err := e.store.List()
	e.mu.Unlock()
	if err != nil {
		e.Log.Warn("worker records", "err", err)
		return
	}
	for _, rec := range recs {
		if rec.State == "" || rec.State == StateStopped { // not started by Start, or ended
			continue
		}
		ses, ok := e.sessions.Get(rec.Session)
		if !ok || ses.State == wire.PhaseDone || ses.State == wire.PhaseFailed {
			continue
		}
		if err := e.check(ctx, ses, rec); err != nil {
			e.Log.Warn("worker check", "session", rec.Session, "err", err)
		}
	}
}

// check posts an open escalation again after a restart, closes an answered escalation, and
// opens the fallback or the budget escalation.
func (e *Engine) check(ctx context.Context, ses wire.DaemonSession, rec Record) error {
	if esc := rec.Escalation; esc != nil {
		e.mu.Lock()
		posted := e.posted[ses.ID]
		e.mu.Unlock()
		if !posted {
			// The runtime keeps an escalation only in memory.
			if err := e.watch.Escalate(ctx, ses, esc.runtimeQuestion()); err != nil && !errors.Is(err, runtime.ErrConflict) {
				return err
			}
			e.mu.Lock()
			e.posted[ses.ID] = true
			e.mu.Unlock()
			return nil
		}
	}
	insp, ok := e.inspect(ctx, ses)
	if !ok {
		return nil
	}
	q := insp.Session.Question
	if esc := rec.Escalation; esc != nil {
		open := insp.Session.Permission != nil || q != nil
		if esc.ID != "" {
			open = q != nil && q.ID == esc.ID
		}
		if open {
			return nil
		}
		e.mu.Lock()
		delete(e.posted, ses.ID)
		e.mu.Unlock()
		_, err := e.update(ses, func(r *Record) {
			r.Escalation = nil
			if esc.Reason == ReasonBudget {
				r.Budget = cmpBudget(r.Budget) + DefaultBudget
			}
		})
		return err
	}
	now := e.now()
	if asked := insp.Session.Permission != nil || q != nil && !strings.HasPrefix(q.ID, escalationPrefix); asked {
		at := insp.AskedAt
		if rec.LastAction.After(at) {
			at = rec.LastAction
		}
		if at.IsZero() {
			at = rec.StartedAt
		}
		if now.Sub(at) >= FallbackAfter {
			return e.escalate(ctx, ses, "", ReasonFallback)
		}
	}
	if rec.State == StateWorking && now.Sub(rec.StartedAt) >= cmpBudget(rec.Budget) {
		if err := e.escalate(ctx, ses, budgetAsk, ReasonBudget); err != nil {
			return err
		}
		return e.interrupt(ctx, ses)
	}
	return nil
}
