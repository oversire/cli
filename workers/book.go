// Package workers runs the supervised workers of the daemon, for example the intake Enhance workers of the app.
// A journal file keeps each worker, so its state survives a restart of the daemon.
package workers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// DefaultRuntime runs a worker with no runtime in its WorkerStart.
const DefaultRuntime = "orca"

// ErrNotFound: the book has no worker with the id.
var ErrNotFound = errors.New("no such worker")

// ErrBadStart: the WorkerStart has no spec, no agent or no absolute path.
var ErrBadStart = errors.New("a worker needs a spec, an agent and an absolute path")

// Supervisors gives the runtime that runs workers (runtime.Registry).
type Supervisors interface {
	Supervisor(id string) (runtime.Supervisor, bool)
}

// Allow checks an order against the local allow list (allow.File).
type Allow interface {
	Check(o wire.WorkOrder) error
}

type entry struct {
	Worker wire.Worker          `json:"worker"`
	Handle runtime.WorkerHandle `json:"handle"`
	// Settled is the done or ended state. It does not change, so the book does not ask the runtime again.
	Settled *wire.WorkerState `json:"settled,omitempty"`
}

// Book keeps the workers of the daemon in the journal file at path.
type Book struct {
	sups  Supervisors
	allow Allow
	path  string
	clock func() time.Time

	mu   sync.Mutex
	byID map[string]*entry
}

// NewBook reads the journal. A missing or bad file gives an empty book.
func NewBook(sups Supervisors, allow Allow, path string, clock func() time.Time) *Book {
	b := &Book{sups: sups, allow: allow, path: path, clock: clock, byID: map[string]*entry{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &b.byID)
	}
	return b
}

// Start starts a worker. A caller that is not trusted (a daemon, or no caller) must pass the allow list.
func (b *Book) Start(ctx context.Context, w wire.WorkerStart) (wire.Worker, error) {
	if w.Spec == "" || w.Agent == "" || !filepath.IsAbs(w.Path) {
		return wire.Worker{}, ErrBadStart
	}
	if w.Runtime == "" {
		w.Runtime = DefaultRuntime
	}
	sup, ok := b.sups.Supervisor(w.Runtime)
	if !ok {
		return wire.Worker{}, fmt.Errorf("%w: %s runs no workers", runtime.ErrNoRuntime, w.Runtime)
	}
	if c, _ := server.CallerFrom(ctx); !c.Trusted() {
		o := wire.WorkOrder{Runtime: w.Runtime, Payload: wire.LaunchRequest{Repo: w.Path, Agent: w.Agent}}
		if err := b.allow.Check(o); err != nil {
			return wire.Worker{}, &refused{err}
		}
	}
	h, err := sup.StartWorker(ctx, w)
	if err != nil {
		return wire.Worker{}, err
	}
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	worker := wire.Worker{ID: "wk-" + hex.EncodeToString(id), Runtime: w.Runtime, Title: w.Title, Path: w.Path, Agent: w.Agent,
		Terminal: h.Terminal, StartedAt: b.clock().UTC()}
	b.mu.Lock()
	b.byID[worker.ID] = &entry{Worker: worker, Handle: h}
	b.save()
	b.mu.Unlock()
	return worker, nil
}

// State asks the runtime until the worker settles. Then the journal gives the state.
func (b *Book) State(ctx context.Context, id string) (wire.WorkerState, error) {
	e, sup, err := b.get(id)
	if err != nil {
		return wire.WorkerState{}, err
	}
	if e.Settled != nil {
		return *e.Settled, nil
	}
	st, err := sup.WorkerState(ctx, e.Handle)
	if err != nil || st.State == wire.WorkerRunning {
		return st, err
	}
	b.mu.Lock()
	if cur := b.byID[id]; cur != nil {
		cur.Settled = &st
		b.save()
	}
	b.mu.Unlock()
	return st, nil
}

func (b *Book) Stop(ctx context.Context, id string) error {
	e, sup, err := b.get(id)
	if err != nil {
		return err
	}
	return sup.StopWorker(ctx, e.Handle)
}

// Release closes the terminal of the worker and removes it from the book. The worker leaves the book also when the runtime fails.
func (b *Book) Release(ctx context.Context, id string) error {
	e, sup, err := b.get(id)
	if err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.byID, id)
	b.save()
	b.mu.Unlock()
	return sup.ReleaseWorker(ctx, e.Handle)
}

// Terminals are the terminals of the workers in the book.
func (b *Book) Terminals() map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]bool{}
	for _, e := range b.byID {
		if e.Handle.Terminal != "" {
			out[e.Handle.Terminal] = true
		}
	}
	return out
}

func (b *Book) get(id string) (entry, runtime.Supervisor, error) {
	b.mu.Lock()
	e := b.byID[id]
	b.mu.Unlock()
	if e == nil {
		return entry{}, nil, ErrNotFound
	}
	sup, ok := b.sups.Supervisor(e.Worker.Runtime)
	if !ok {
		return entry{}, nil, fmt.Errorf("%w: %s", runtime.ErrNoRuntime, e.Worker.Runtime)
	}
	return *e, sup, nil
}

// save needs b.mu.
func (b *Book) save() { _ = config.WriteJSON(b.path, b.byID) }

// refused is a start that the allow list refuses.
type refused struct{ error }

func (r *refused) Unwrap() error { return r.error }
