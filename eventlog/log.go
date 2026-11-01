// Package eventlog keeps the last ProviderEvents of the daemon in memory, for GET /v1/events.
// With a Keeper, the log also writes each event to disk and reads the end again at the start (OVERSIRE-379).
// Without one, the log starts again at seq 1 after a restart. A client then sees head < its cursor and reads from the start.
package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"

	"github.com/oversire/cli/wire"
)

const DefaultCapacity = 2000

type entry struct {
	seq int64
	e   wire.ProviderEvent
}

// Hint is one kept event.
type Hint struct {
	Seq   int64
	Event wire.ProviderEvent
}

// Keeper writes the hints to disk (events.Store).
type Keeper interface {
	SaveHint(ctx context.Context, seq int64, e wire.ProviderEvent) error
	Hints(ctx context.Context, limit int) ([]Hint, error)
}

// Log is a ring of events with a seq that increases.
type Log struct {
	mu      sync.Mutex
	ring    []entry
	start   int // index of the oldest entry when the ring is full
	head    int64
	epoch   string
	subs    map[int]chan struct{}
	nextSub int
	cap     int
	keeper  Keeper
}

func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Log{cap: capacity, epoch: hex.EncodeToString(b), subs: map[int]chan struct{}{}}
}

// Keep reads the last hints of k into the log and writes each new event to k.
func (l *Log) Keep(ctx context.Context, k Keeper) error {
	hints, err := k.Hints(ctx, l.cap)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keeper = k
	for _, h := range hints {
		l.addLocked(entry{seq: h.Seq, e: h.Event})
	}
	return nil
}

// Epoch is a random id of this log. It changes at each start.
func (l *Log) Epoch() string { return l.epoch }

func (l *Log) Head() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.head
}

// Append adds the event and pokes each subscriber. A slow subscriber misses pokes, not events.
func (l *Log) Append(e wire.ProviderEvent) {
	l.mu.Lock()
	en := entry{seq: l.head + 1, e: e}
	l.addLocked(en)
	if l.keeper != nil {
		if err := l.keeper.SaveHint(context.Background(), en.seq, e); err != nil {
			slog.Warn("eventlog: keep the hint", "err", err)
		}
	}
	for _, c := range l.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
	l.mu.Unlock()
}

// Page gives at most limit events after the seq `after`, the same shape as the cloud.
func (l *Log) Page(after, limit int) wire.EventsPage {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		limit = 500
	}
	p := wire.EventsPage{Events: []wire.ProviderEvent{}, Head: l.head, Next: l.head}
	if len(l.ring) == 0 {
		return p
	}
	oldest := l.at(0).seq
	a := int64(after)
	p.Truncated = a < oldest-1
	for i := range l.ring {
		en := l.at(i)
		if en.seq <= a {
			continue
		}
		if len(p.Events) == limit {
			p.More = true
			break
		}
		p.Events = append(p.Events, en.e)
		p.Next = en.seq
	}
	if !p.More {
		p.Next = l.head
	}
	return p
}

func (l *Log) addLocked(en entry) {
	l.head = en.seq
	if len(l.ring) < l.cap {
		l.ring = append(l.ring, en)
	} else {
		l.ring[l.start] = en
		l.start = (l.start + 1) % l.cap
	}
}

func (l *Log) at(i int) entry { return l.ring[(l.start+i)%len(l.ring)] }

// Subscribe gives a channel that gets a poke after each append, and a cancel func.
func (l *Log) Subscribe() (<-chan struct{}, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextSub
	l.nextSub++
	c := make(chan struct{}, 1)
	l.subs[id] = c
	return c, func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}
