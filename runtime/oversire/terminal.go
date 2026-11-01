//go:build !windows

package oversire

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// handoverWait is how long Handover waits for an early refusal of the visor before it answers.
const handoverWait = 300 * time.Millisecond

// TerminalCommand is the command of an agent session in a terminal. A resume continues the
// vendor session. Only claude and codex have a resume command.
func TerminalCommand(l agentvisor.Launch) ([]string, error) {
	if !l.Resume && len(l.Spec.Command) > 0 {
		return l.Spec.Command, nil
	}
	h := agentwire.Harness(l.Spec.Harness)
	argv := []string{cmp.Or(l.Binary, l.Spec.Harness)}
	if l.Resume {
		switch h {
		case agentwire.Claude:
			argv = append(argv, "--resume", l.Session.VendorID)
		case agentwire.Codex:
			argv = append(argv, "resume", l.Session.VendorID)
		default:
			return nil, fmt.Errorf("%w: no terminal resume command for harness %s", agentvisor.ErrUnsupported, h)
		}
	}
	argv = append(argv, agentwire.ModelArgs(h, l.Spec.Model, l.Spec.Effort)...)
	return append(argv, l.Spec.ExtraArgs...), nil
}

// viewOf is the view of a visor session kind.
func viewOf(k agentvisor.Kind) wire.SessionView {
	if k == agentvisor.KindPTY {
		return wire.SessionViewTerminal
	}
	return wire.SessionViewChat
}

// Handover moves the session at the end of its turn. A move to chat waits until the user quits the
// agent CLI. The visor refuses a bad move at once, so Handover waits a short time for a refusal.
func (rt *Runtime) Handover(ctx context.Context, id string, view wire.SessionView) error {
	s, err := rt.session(wire.SessionRef{ID: id})
	if err != nil {
		return fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	}
	s.mu.Lock()
	vid, ended, moving := s.vid, s.info.State.Ended(), s.moving
	if !ended && vid != "" && !moving {
		s.moving = true
	}
	s.mu.Unlock()
	switch {
	case ended:
		return fmt.Errorf("%w: the session has ended", runtime.ErrConflict)
	case vid == "":
		return fmt.Errorf("%w: the session has not started yet", runtime.ErrConflict)
	case moving:
		return fmt.Errorf("%w: the session moves now", runtime.ErrConflict)
	}
	to := agentvisor.KindWire
	if view == wire.SessionViewTerminal {
		to = agentvisor.KindPTY
	}
	done := make(chan error, 1)
	go func() {
		err := rt.host.Handover(context.WithoutCancel(ctx), vid, to)
		s.mu.Lock()
		s.moving = false
		s.mu.Unlock()
		if err != nil {
			rt.log.Warn("move the session", "session", id, "view", view, "err", err)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return handoverError(err)
	case <-time.After(handoverWait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handoverError maps a refusal of the visor to the errors of the runtime.
func handoverError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, agentvisor.ErrUnsupported):
		return fmt.Errorf("%w: %w", runtime.ErrUnsupported, err)
	case errors.Is(err, agentvisor.ErrNotFound):
		return fmt.Errorf("%w: %w", runtime.ErrNotFound, err)
	case errors.Is(err, agentvisor.ErrState), errors.Is(err, agentvisor.ErrBusy), errors.Is(err, agentvisor.ErrInvalid):
		return fmt.Errorf("%w: %w", runtime.ErrConflict, err)
	}
	return err
}

// setKind keeps the view of the visor session and reports a change. Call it with s.mu held.
func (s *session) setKind(k agentvisor.Kind) {
	if v := viewOf(k); v != s.info.View {
		s.info.View = v
		s.publish()
	}
}

// Terminal opens the terminal of a session that runs in a PTY after a handover.
func (rt *Runtime) Terminal(ctx context.Context, d wire.DaemonSession) (term.Source, error) {
	s, err := rt.session(wire.SessionRef{ID: d.ID})
	if err != nil {
		return nil, term.ErrNoView
	}
	s.mu.Lock()
	vid := s.vid
	s.mu.Unlock()
	if vid == "" {
		return nil, term.ErrNoView
	}
	vs, err := rt.host.Session(ctx, vid)
	if err != nil || vs.Kind != agentvisor.KindPTY {
		return nil, term.ErrNoView
	}
	return &visorTerm{host: rt.host, vid: vid}, nil
}

// visorTerm is the terminal of a visor session for one viewer. The viewer can attach again, so
// each Attach opens a new visor attachment. The term API keeps the size lease of OverSire, so the
// lease frames of the visor do not go to the viewer.
type visorTerm struct {
	host Host
	vid  string

	mu     sync.Mutex
	stream *agentvisor.AttachStream
}

func (t *visorTerm) Caps() term.Caps {
	return term.Caps{Source: wire.SourceStream, Writable: true, Resizable: true}
}

func (t *visorTerm) Attach(ctx context.Context, _ term.Viewport) (term.Snapshot, <-chan term.Chunk, error) {
	a, err := t.host.Attach(ctx, t.vid, agentvisor.AttachOptions{View: agentvisor.ViewTerminal})
	if err != nil {
		return term.Snapshot{}, nil, err
	}
	snap, err := firstSnapshot(ctx, a)
	if err != nil {
		_ = a.Close()
		return term.Snapshot{}, nil, err
	}
	t.mu.Lock()
	if t.stream != nil {
		_ = t.stream.Close()
	}
	t.stream = a
	t.mu.Unlock()
	out := make(chan term.Chunk, 64)
	go t.pump(ctx, a, snap.Size, out)
	return snap, out, nil
}

// firstSnapshot reads the attached frame and the first snapshot of an attachment.
func firstSnapshot(ctx context.Context, a *agentvisor.AttachStream) (term.Snapshot, error) {
	var size term.Viewport
	for {
		f, err := a.Recv(ctx)
		if err != nil {
			return term.Snapshot{}, err
		}
		switch {
		case f.Op == agentvisor.OpSnapshot:
			return term.Snapshot{Data: f.Data, Size: size}, nil
		case f.Type == agentvisor.FrameAttached, f.Type == agentvisor.FrameResized:
			size = term.Viewport{Cols: f.Cols, Rows: f.Rows}
		case f.Type == agentvisor.FrameEnded:
			return term.Snapshot{}, fmt.Errorf("%w: the terminal %s", term.ErrNoView, f.Reason)
		case f.Type == agentvisor.FrameError:
			return term.Snapshot{}, errors.New(cmp.Or(f.Message, f.Code))
		}
	}
}

// pump gives the frames of the attachment to the viewer until ctx ends or the terminal ends.
func (t *visorTerm) pump(ctx context.Context, a *agentvisor.AttachStream, size term.Viewport, out chan<- term.Chunk) {
	defer close(out)
	defer a.Close() //nolint:errcheck // the attachment is done
	send := func(c term.Chunk) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		f, err := a.Recv(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				send(term.Chunk{Ended: "exited"})
			}
			return
		}
		var c term.Chunk
		switch {
		case f.Op == agentvisor.OpOutput:
			c.Output = f.Data
		case f.Op == agentvisor.OpSnapshot:
			c.Repaint = &term.Snapshot{Data: f.Data, Size: size}
		case f.Type == agentvisor.FrameResized:
			size = term.Viewport{Cols: f.Cols, Rows: f.Rows}
			c.Resized = &size
		case f.Type == agentvisor.FrameEnded:
			c.Ended = cmp.Or(string(f.Reason), "exited")
		default:
			continue
		}
		if !send(c) || c.Ended != "" {
			return
		}
	}
}

func (t *visorTerm) current() (*agentvisor.AttachStream, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream == nil {
		return nil, errors.New("the terminal is not attached")
	}
	return t.stream, nil
}

func (t *visorTerm) Write(ctx context.Context, p []byte) error {
	a, err := t.current()
	if err != nil {
		return err
	}
	return a.Input(ctx, p)
}

func (t *visorTerm) Control(ctx context.Context, v *term.Viewport) error {
	a, err := t.current()
	if err != nil {
		return err
	}
	if v == nil {
		return a.Handle(ctx, agentvisor.ClientFrame{Type: agentvisor.ClientRelease})
	}
	return a.Handle(ctx, agentvisor.ClientFrame{Type: agentvisor.ClientControl, Cols: v.Cols, Rows: v.Rows})
}
