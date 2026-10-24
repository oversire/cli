package runtime

import (
	"context"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// Streamer opens the live views of a session. A view that the runtime does not have gives term.ErrNoView.
type Streamer interface {
	Terminal(ctx context.Context, s wire.DaemonSession) (term.Source, error)
	Chat(ctx context.Context, s wire.DaemonSession) (term.TurnSource, error)
}

// StreamStater tells the state of the live view, for GET /v1/runtimes.
type StreamStater interface {
	StreamState() wire.RuntimeStream
}

// StreamRetrier connects the live view again, for POST /v1/runtimes/{id}/retry.
type StreamRetrier interface {
	RetryStream(ctx context.Context)
}

// StreamPairer keeps a pairing link for the live view, for POST and DELETE /v1/runtimes/{id}/pair.
type StreamPairer interface {
	PairStream(ctx context.Context, link string) error
	UnpairStream(ctx context.Context)
}

// Terminal opens the terminal view in the runtime of the session. The registry is the term.Opener of the daemon.
func (r *Registry) Terminal(ctx context.Context, s wire.DaemonSession) (term.Source, error) {
	st, ok := r.byID[s.Runtime].(Streamer)
	if !ok {
		return nil, term.ErrNoView
	}
	return st.Terminal(ctx, s)
}

// Chat opens the chat view in the runtime of the session.
func (r *Registry) Chat(ctx context.Context, s wire.DaemonSession) (term.TurnSource, error) {
	st, ok := r.byID[s.Runtime].(Streamer)
	if !ok {
		return nil, term.ErrNoView
	}
	return st.Chat(ctx, s)
}
