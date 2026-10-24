// Package term serves the live view of a session: GET /v1/sessions/{id}/stream. Contract: session-stream.json.
package term

import (
	"context"
	"errors"

	"github.com/oversire/cli/wire"
)

// Viewport is a terminal size in cells. A zero value is not known.
type Viewport struct{ Cols, Rows int }

// Snapshot is a full repaint of the screen in ANSI.
type Snapshot struct {
	Data []byte
	Size Viewport
}

// Chunk is one event of an attached source. Only one field is set.
type Chunk struct {
	Output   []byte    // terminal bytes
	Repaint  *Snapshot // a full repaint in place of output, for example a new poll of the screen
	Resized  *Viewport // a snapshot comes next
	TakeBack bool      // the runtime UI took the size back
	Ended    string    // the terminal ended: exited, removed or closed. The channel closes next.
}

// Caps tells what a source can do.
type Caps struct {
	Source    wire.StreamSource
	Writable  bool
	Resizable bool
	Reason    string // why the source is poll
}

// Source is the terminal of one session for one viewer.
type Source interface {
	// Attach gives the screen now, then the chunks after it. The channel closes when ctx is done or the terminal ends.
	Attach(ctx context.Context, v Viewport) (Snapshot, <-chan Chunk, error)
	Write(ctx context.Context, p []byte) error
	// Control sets the size of the terminal. nil gives the size back to the runtime UI.
	Control(ctx context.Context, v *Viewport) error
	Caps() Caps
}

// TurnSource is the chat of one session that has no terminal.
type TurnSource interface {
	// Turns gives the turns after seq after, then the new turns. The channel closes when ctx is done or the session ends.
	Turns(ctx context.Context, after int) ([]wire.SessionTurn, <-chan wire.SessionTurn, error)
	Send(ctx context.Context, text string) error
	// Interrupt stops the current turn. The session stays open.
	Interrupt(ctx context.Context) error
}

// Opener gives a new source for each viewer of a session.
type Opener interface {
	Terminal(ctx context.Context, s wire.DaemonSession) (Source, error)
	Chat(ctx context.Context, s wire.DaemonSession) (TurnSource, error)
}

// ErrNoView is the error of an Opener that has no such view for the session.
var ErrNoView = errors.New("the session has no such view")
