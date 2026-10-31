package oversire

import "context"

// Slots limits the sessions that run at the same time.
type Slots struct{ free chan struct{} }

func NewSlots(n int) *Slots { return &Slots{free: make(chan struct{}, max(n, 1))} }

// Acquire waits for a slot until ctx ends.
func (s *Slots) Acquire(ctx context.Context) error {
	select {
	case s.free <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Slots) Release() { <-s.free }

// Take takes a free slot with no wait, for a session that runs already. It gives false when no slot is free.
func (s *Slots) Take() bool {
	select {
	case s.free <- struct{}{}:
		return true
	default:
		return false
	}
}
