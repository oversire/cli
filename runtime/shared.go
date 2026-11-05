package runtime

import (
	"context"
	"sync"
	"time"
)

// errKeep is the longest time that Shared gives an error, or the last good value in place of an error.
const errKeep = 2 * time.Second

// Shared gives one fetch result of a key to all callers within ttl. Only one fetch of a key is in flight.
// The fetch has its own context, so a caller that stops does not stop it. With keepLast, a failed fetch gives the
// last good value. Clear makes the next call fetch again.
type Shared[T any] struct {
	ttl, limit time.Duration
	keepLast   bool
	now        func() time.Time

	mu    sync.Mutex
	slots map[string]*sharedSlot[T]
}

type sharedSlot[T any] struct {
	f    *fetch[T] // the last fetch, in flight or done
	last T         // the last good value
	good bool
}

type fetch[T any] struct {
	done  chan struct{}
	until time.Time // the result is good until then
	val   T
	err   error
}

// NewShared gives a Shared. limit is the timeout of one fetch.
func NewShared[T any](ttl, limit time.Duration, keepLast bool, now func() time.Time) *Shared[T] {
	return &Shared[T]{ttl: ttl, limit: limit, keepLast: keepLast, now: now, slots: map[string]*sharedSlot[T]{}}
}

// Get gives the value of key. It starts a fetch when no fetch is in flight and the last result is old.
func (s *Shared[T]) Get(ctx context.Context, key string, get func(context.Context) (T, error)) (T, error) {
	s.mu.Lock()
	sl := s.slots[key]
	if sl == nil {
		s.prune()
		sl = &sharedSlot[T]{}
		s.slots[key] = sl
	}
	f := sl.f
	if f == nil || s.old(f) {
		f = &fetch[T]{done: make(chan struct{})}
		sl.f = f
		go s.run(ctx, sl, f, get)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Clear makes the next Get of key fetch again. A fetch in flight ends, but it is not given to new callers.
func (s *Shared[T]) Clear(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sl := s.slots[key]; sl != nil {
		sl.f = nil
	}
}

func (s *Shared[T]) run(ctx context.Context, sl *sharedSlot[T], f *fetch[T], get func(context.Context) (T, error)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.limit)
	defer cancel()
	v, err := get(ctx)
	s.mu.Lock()
	keep := s.ttl
	if err == nil {
		sl.last, sl.good = v, true
	} else {
		keep = min(errKeep, s.ttl)
		if s.keepLast && sl.good {
			v, err = sl.last, nil
		}
	}
	f.val, f.err, f.until = v, err, s.now().Add(keep)
	s.mu.Unlock()
	close(f.done)
}

// old tells if a fetch is done and its result is too old. Call it with s.mu.
func (s *Shared[T]) old(f *fetch[T]) bool {
	select {
	case <-f.done:
		return !s.now().Before(f.until)
	default:
		return false
	}
}

// prune forgets the slots with an old result, when there are many. Call it with s.mu.
func (s *Shared[T]) prune() {
	if len(s.slots) < 64 {
		return
	}
	for k, sl := range s.slots {
		if sl.f == nil || s.old(sl.f) {
			delete(s.slots, k)
		}
	}
}
