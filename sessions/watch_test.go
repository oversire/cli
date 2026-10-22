package sessions

import (
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func TestWatchGetsStateChanges(t *testing.T) {
	b := NewBook("mbp", "", nil, func() time.Time { return t0 })
	c, cancel := b.Watch()
	b.Apply(Patch{ID: "s1", Source: SourceHook, At: t0, State: ptr(wire.PhaseWorking)})
	if got := <-c; got != "s1" {
		t.Fatalf("got %q", got)
	}
	b.Apply(Patch{ID: "s1", Source: SourceHook, At: at(1), State: ptr(wire.PhaseWorking)})
	select {
	case id := <-c:
		t.Fatalf("no change, got %q", id)
	default:
	}
	cancel()
	b.Apply(Patch{ID: "s1", Source: SourceHook, At: at(2), State: ptr(wire.PhaseWaiting)})
	select {
	case id := <-c:
		t.Fatalf("cancelled, got %q", id)
	default:
	}
}

func TestWatchNeverBlocksTheBook(t *testing.T) {
	b := NewBook("mbp", "", nil, func() time.Time { return t0 })
	_, cancel := b.Watch()
	defer cancel()
	for i := 0; i < 200; i++ {
		s := wire.PhaseWorking
		if i%2 == 1 {
			s = wire.PhaseWaiting
		}
		b.Apply(Patch{ID: "s1", Source: SourceHook, At: at(time.Duration(i)), State: &s})
	}
}
