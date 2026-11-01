// Package capture turns what happens on the daemon into rows of the events table (OVERSIRE-380):
// hook calls, session changes, questions, reviews, runbook steps and the queue.
package capture

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/wire"
)

const (
	queueSize = 4096
	batchMax  = 200
)

// Store takes the rows (events.Store).
type Store interface {
	Append(ctx context.Context, drafts []wire.EventDraft) ([]wire.Event, error)
}

// Places gives the item of a row (events.Resolver).
type Places interface {
	Fill(ctx context.Context, item, parent *string, h events.Hint)
}

// Sessions gives a session of the book (sessions.Book).
type Sessions interface {
	Get(id string) (wire.DaemonSession, bool)
}

// Writer completes each draft and writes it to the store in batches. Add never blocks: a full queue drops the row.
type Writer struct {
	store    Store
	places   Places
	sessions Sessions
	machine  string
	human    string
	log      *slog.Logger
	queue    chan queued
}

// queued is a draft and the place hint of its session. The writer goroutine fills the item, off the locks of the callers.
type queued struct {
	d wire.EventDraft
	h events.Hint
}

// NewWriter makes a writer. human is the user of this machine (the git user.email), the actor of the human rows.
func NewWriter(store Store, places Places, sessions Sessions, machine, human string, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	if human == "" {
		human = os.Getenv("USER")
	}
	return &Writer{store: store, places: places, sessions: sessions, machine: machine, human: human, log: log,
		queue: make(chan queued, queueSize)}
}

// Human is the actor of the human rows.
func (w *Writer) Human() string { return w.human }

// Add completes d from the session of the book with that id (it can be "") and queues it.
// Each empty field gets a value: session, repo, item, parent, machine, actor and on_behalf.
func (w *Writer) Add(session string, d wire.EventDraft) {
	if w == nil {
		return
	}
	var s wire.DaemonSession
	if session != "" && w.sessions != nil {
		s, _ = w.sessions.Get(session)
	}
	w.AddFor(s, d)
}

// AddFor is Add with the session in hand. It does not call the book.
func (w *Writer) AddFor(s wire.DaemonSession, d wire.EventDraft) {
	if d.Session == "" {
		d.Session = cmpOr(s.Key, s.ID)
	}
	if d.Repo == "" {
		d.Repo = s.Repo
	}
	if d.Machine == "" {
		d.Machine = w.machine
	}
	if d.At.IsZero() {
		d.At = time.Now()
	}
	switch d.ActorType {
	case wire.ActorHuman:
		if d.Actor == "" {
			d.Actor = w.human
		}
	case wire.ActorAgent:
		if d.Actor == "" {
			d.Actor = AgentActor(s.Harness, s.Model)
		}
		if d.OnBehalf == "" {
			d.OnBehalf = w.human
		}
	case wire.ActorSystem:
		if d.Actor == "" {
			d.Actor = "oversire"
		}
		if d.OnBehalf == "" {
			d.OnBehalf = w.human
		}
	}
	select {
	case w.queue <- queued{d, events.Hint{SessionKey: cmpOr(s.Key, d.Session), Path: s.Cwd, Branch: s.Branch}}:
	default:
		w.log.Warn("capture: the queue is full; a row is dropped", "kind", d.Kind)
	}
}

// AgentActor gives harness/model, or the harness alone.
func AgentActor(harness, model string) string {
	harness = cmpOr(harness, "agent")
	if model == "" {
		return harness
	}
	return harness + "/" + model
}

// Run writes the queue in batches until ctx is done. Then it writes what is left.
func (w *Writer) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.flush(context.Background(), w.drain(nil))
			return nil
		case q := <-w.queue:
			w.flush(ctx, w.drain([]queued{q}))
		}
	}
}

// Flush writes the queue now, for tests.
func (w *Writer) Flush(ctx context.Context) { w.flush(ctx, w.drain(nil)) }

func (w *Writer) drain(batch []queued) []queued {
	for len(batch) < batchMax {
		select {
		case q := <-w.queue:
			batch = append(batch, q)
		default:
			return batch
		}
	}
	return batch
}

func (w *Writer) flush(ctx context.Context, batch []queued) {
	for len(batch) > 0 {
		good := make([]wire.EventDraft, 0, len(batch))
		for _, q := range batch {
			d := q.d
			if w.places != nil {
				w.places.Fill(ctx, &d.Item, &d.ParentItem, q.h)
			}
			if _, err := events.Normalize(d); err != nil {
				w.log.Warn("capture: a bad row", "kind", d.Kind, "err", err)
				continue
			}
			good = append(good, d)
		}
		if _, err := w.store.Append(ctx, good); err != nil {
			w.log.Warn("capture: write the rows", "err", err, "rows", len(good))
		}
		batch = w.drain(nil)
	}
}

// ID makes the id of a row: <source>:<parts joined by colons>.
func ID(source string, parts ...string) string {
	return source + ":" + strings.Join(parts, ":")
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
