package hub

import (
	"slices"
	"sync"

	"github.com/oversire/cli/wire"
)

// sessionBox keeps the latest state of each session until the hub has it.
// A live session stays, so that it goes out again after each welcome.
type sessionBox struct {
	mu     sync.Mutex
	latest map[string]wire.RunnerSession
	dirty  map[string]bool
	// paths keeps the latest changed paths of each order until a send.
	paths map[string]wire.Paths
	kick  chan struct{}
}

func newSessionBox() *sessionBox {
	return &sessionBox{latest: map[string]wire.RunnerSession{}, dirty: map[string]bool{}, paths: map[string]wire.Paths{}, kick: make(chan struct{}, 1)}
}

func (b *sessionBox) putPaths(p wire.Paths) {
	b.mu.Lock()
	b.paths[p.Order] = p
	b.mu.Unlock()
	b.wake()
}

// takePaths gives the paths to send, oldest first.
func (b *sessionBox) takePaths() []wire.Paths {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]wire.Paths, 0, len(b.paths))
	for _, p := range b.paths {
		out = append(out, p)
	}
	clear(b.paths)
	slices.SortFunc(out, func(x, y wire.Paths) int { return x.Paths.At.Compare(y.Paths.At) })
	return out
}

// pathsFailed keeps p for the next send, unless a newer report replaced it.
func (b *sessionBox) pathsFailed(p wire.Paths) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.paths[p.Order]; !ok {
		b.paths[p.Order] = p
	}
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
