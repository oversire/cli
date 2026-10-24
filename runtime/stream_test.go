package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// streamer has the chat view only.
type streamer struct {
	fake
	opened string
}

func (s *streamer) ID() string { return "str" }
func (s *streamer) Terminal(context.Context, wire.DaemonSession) (term.Source, error) {
	return nil, term.ErrNoView
}
func (s *streamer) Chat(_ context.Context, d wire.DaemonSession) (term.TurnSource, error) {
	s.opened = d.ID
	return nil, nil
}
func (s *streamer) StreamState() wire.RuntimeStream {
	return wire.RuntimeStream{State: wire.StreamLive}
}

func TestRegistryOpensTheViewOfTheRuntime(t *testing.T) {
	st := &streamer{}
	r := NewRegistry(st, &fake{})
	ctx := context.Background()
	if _, err := r.Chat(ctx, wire.DaemonSession{ID: "s-1", Runtime: "str"}); err != nil || st.opened != "s-1" {
		t.Fatal(err, st.opened)
	}
	if _, err := r.Terminal(ctx, wire.DaemonSession{Runtime: "str"}); !errors.Is(err, term.ErrNoView) {
		t.Fatal(err)
	}
	for _, rt := range []string{"fake", "cli"} {
		if _, err := r.Chat(ctx, wire.DaemonSession{Runtime: rt}); !errors.Is(err, term.ErrNoView) {
			t.Fatal(rt, err)
		}
	}
	status := r.Status(ctx)
	if !slices.Contains(status[1].Capabilities, CapSessionStream) || status[1].Stream == nil || status[1].Stream.State != wire.StreamLive {
		t.Fatalf("%+v", status[1])
	}
	if slices.Contains(status[0].Capabilities, CapSessionStream) || status[0].Stream != nil {
		t.Fatalf("%+v", status[0])
	}
}
