package hub

import (
	"slices"
	"sync"

	"github.com/oversire/runner/wire"
)

// sessionBox keeps the latest state of each session until the hub has it.
// A live session stays, so that it goes out again after each welcome.
type sessionBox struct {
	mu     sync.Mutex
	latest map[string]wire.RunnerSession
	dirty  map[string]bool
	kick   chan struct{}
}

func newSessionBox() *sessionBox {
	return &sessionBox{latest: map[string]wire.RunnerSession{}, dirty: map[string]bool{}, kick: make(chan struct{}, 1)}
}

func (b *sessionBox) put(s wire.RunnerSession) {
	b.mu.Lock()
	b.latest[s.ID] = s
	b.dirty[s.ID] = true
	b.mu.Unlock()
	b.wake()
}

// markAll makes all kept sessions go out again.
func (b *sessionBox) markAll() {
	b.mu.Lock()
	for id := range b.latest {
		b.dirty[id] = true
	}
	b.mu.Unlock()
	b.wake()
}

func (b *sessionBox) wake() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// take gives the sessions to send, oldest update first.
func (b *sessionBox) take() []wire.RunnerSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]wire.RunnerSession, 0, len(b.dirty))
	for id := range b.dirty {
		out = append(out, b.latest[id])
	}
	clear(b.dirty)
	slices.SortFunc(out, func(x, y wire.RunnerSession) int { return x.UpdatedAt.Compare(y.UpdatedAt) })
	return out
}

// sent records the result of one send. The hub keeps an ended session, so the box drops it.
func (b *sessionBox) sent(s wire.RunnerSession, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dirty[s.ID] {
		return
	}
	if !ok {
		b.dirty[s.ID] = true
	} else if s.State.Ended() {
		delete(b.latest, s.ID)
	}
}
