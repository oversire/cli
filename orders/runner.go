// Package orders runs work orders at most one time, from the hub or straight from an app (Direct).
package orders

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Runtimes runs orders (runtime.Registry).
type Runtimes interface {
	Has(id string) bool
	Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error)
}

// Allow checks orders against the local allow list (allow.File).
type Allow interface {
	Check(o wire.WorkOrder) error
}

// Journal keeps the finished orders (hub.Journal).
type Journal interface {
	Get(order string) (wire.Result, bool)
	Put(r wire.Result)
	MarkSent(order string)
}

// Outcome tells what Run did with an order.
type Outcome int

const (
	Ran    Outcome = iota // the order ran (or failed its checks) now, and the journal has the result
	Stored                // the journal had the result, the order did not run again
	Busy                  // the order runs already, there is no result yet
)

// Hooks hear the steps of one run. Each field can be nil.
type Hooks struct {
	Claimed  func() // the order is new and runs now
	Accepted func() // the order passed the expiry, runtime and allow list checks, the runtime starts next
}

// Runner is the one path for each order: journal, running set, expiry, runtime, allow list, runtime call.
// An order with a loopback caller in its context skips the allow list: the user of this machine gave it.
type Runner struct {
	runtimes Runtimes
	allow    Allow
	journal  Journal
	now      func() time.Time

	mu      sync.Mutex
	running map[string]bool
}

func NewRunner(runtimes Runtimes, allow Allow, journal Journal, now func() time.Time) *Runner {
	return &Runner{runtimes: runtimes, allow: allow, journal: journal, now: now, running: map[string]bool{}}
}

// Result gives the stored result of an order.
func (r *Runner) Result(order string) (wire.Result, bool) { return r.journal.Get(order) }

// MarkSent keeps the hub client from sending the result of a direct order to the hub.
func (r *Runner) MarkSent(order string) { r.journal.MarkSent(order) }

// Running is true while the order runs.
func (r *Runner) Running(order string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[order]
}

// Run runs o at most one time. A finished order gives its stored result.
func (r *Runner) Run(ctx context.Context, o wire.WorkOrder, h Hooks) (wire.Result, Outcome) {
	r.mu.Lock()
	// The journal first: a finished order is in it before it leaves running.
	if res, ok := r.journal.Get(o.ID); ok {
		r.mu.Unlock()
		return res, Stored
	}
	if r.running[o.ID] {
		r.mu.Unlock()
		return wire.Result{Order: o.ID}, Busy
	}
	r.running[o.ID] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, o.ID)
		r.mu.Unlock()
	}()
	call(h.Claimed)

	res := r.do(ctx, o, h)
	r.journal.Put(res)
	return res, Ran
}

func (r *Runner) do(ctx context.Context, o wire.WorkOrder, h Hooks) wire.Result {
	if !o.ExpiresAt.After(r.now()) {
		return wire.Result{Order: o.ID, State: wire.ResultExpired}
	}
	reason := ""
	if !r.runtimes.Has(o.Runtime) {
		reason = fmt.Sprintf("This runner has no runtime %s.", o.Runtime)
	} else if c, ok := server.CallerFrom(ctx); !ok || !c.Loopback {
		if err := r.allow.Check(o); err != nil {
			reason = err.Error()
		}
	}
	if reason != "" {
		return failed(o.ID, reason)
	}
	call(h.Accepted)
	out, err := r.runtimes.Do(ctx, o)
	if err != nil {
		return failed(o.ID, err.Error())
	}
	return wire.Result{Order: o.ID, State: wire.ResultDone, Result: out}
}

// LogResult writes the end of an order that ran. An order that failed before the runtime started is refused.
func LogResult(log *slog.Logger, r wire.Result, accepted bool, took time.Duration) {
	log = log.With("ms", took.Milliseconds())
	switch {
	case r.State == wire.ResultExpired:
		log.Info("order expired")
	case r.State == wire.ResultFailed && !accepted:
		log.Warn("order refused", "reason", r.Result.Error)
	case r.State == wire.ResultFailed:
		log.Warn("order failed", "err", r.Result.Error)
	default:
		log.Info("order done")
	}
}

func failed(order, reason string) wire.Result {
	return wire.Result{Order: order, State: wire.ResultFailed, Result: &wire.WorkOrderResult{Error: reason}}
}

func call(f func()) {
	if f != nil {
		f()
	}
}
