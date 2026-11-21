package events

import (
	"context"
	"testing"
	"time"

	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/wire"
)

func TestHintsLiveThroughARestart(t *testing.T) {
	s, ctx := open(t), context.Background()
	a := eventlog.New(3)
	if err := a.Keep(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"e1", "e2", "e3", "e4"} {
		a.Append(wire.ProviderEvent{ID: id, Provider: "github", Kind: "ci", Action: "completed", At: t0})
	}
	a.Flush(ctx)
	b := eventlog.New(3)
	if err := b.Keep(ctx, s); err != nil {
		t.Fatal(err)
	}
	p := b.Page(0, 0)
	if p.Head != 4 || len(p.Events) != 3 || p.Events[0].ID != "e2" || !p.Truncated {
		t.Fatalf("%+v", p)
	}
	b.Append(wire.ProviderEvent{ID: "e5", At: t0})
	if p := b.Page(4, 0); len(p.Events) != 1 || p.Head != 5 {
		t.Fatalf("%+v", p)
	}
}

func TestAppendDoesNotWaitForTheStoreLock(t *testing.T) {
	s := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	a := eventlog.New(3)
	if err := a.Keep(ctx, s); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(stopped)
	}()
	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		a.Append(wire.ProviderEvent{ID: "e1", At: t0})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		s.mu.Unlock()
		cancel()
		t.Fatal("Append waits for the store lock")
	}
	s.mu.Unlock()
	cancel()
	<-stopped
	b := eventlog.New(3)
	if err := b.Keep(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if p := b.Page(0, 0); len(p.Events) != 1 || p.Events[0].ID != "e1" {
		t.Fatalf("%+v", p)
	}
}
