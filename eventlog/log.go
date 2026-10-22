// Package eventlog keeps the last ProviderEvents of the daemon in memory, for GET /v1/events.
// The log starts again at seq 1 after a restart. A client then sees head < its cursor and reads from the start.
package eventlog

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/oversire/cli/wire"
)

const DefaultCapacity = 2000

type entry struct {
	seq int64
	e   wire.ProviderEvent
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
}

func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Log{cap: capacity, epoch: hex.EncodeToString(b), subs: map[int]chan struct{}{}}
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
	l.head++
	en := entry{seq: l.head, e: e}
	if len(l.ring) < l.cap {
		l.ring = append(l.ring, en)
	} else {
		l.ring[l.start] = en
		l.start = (l.start + 1) % l.cap
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
