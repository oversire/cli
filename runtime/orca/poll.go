package orca

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// PollEvery is how often a text-only view reads the screen.
const PollEvery = time.Second

// pollFailures in a row end the view.
const pollFailures = 5

// The Orca error codes of a terminal that is gone.
var goneTerminal = []string{"terminal_not_found", "terminal_handle_stale"}

// PollReason tells the user why the view is text only.
const PollReason = "Orca does not let the daemon pair by itself. The view is text only."

// Poll is the text-only view of one Orca terminal: `orca terminal read --screen` and `orca terminal send`.
type Poll struct {
	o        *Orca
	terminal string
	every    time.Duration

	writeMu sync.Mutex // the keys go in order
}

// Terminal opens the text-only view of an Orca session. A session with no handle is found by its pane key.
func (o *Orca) Terminal(ctx context.Context, s wire.DaemonSession) (term.Source, error) {
	h := s.Terminal
	if h == "" {
		ts, err := o.Terminals(ctx)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(ts, func(t Terminal) bool { return t.PaneKey() == s.ID && !t.Orphaned })
		if i < 0 {
			return nil, term.ErrNoView
		}
		h = ts[i].Handle
	}
	return &Poll{o: o, terminal: h, every: PollEvery}, nil
}

// Chat: an Orca session has a terminal.
func (o *Orca) Chat(context.Context, wire.DaemonSession) (term.TurnSource, error) {
	return nil, term.ErrNoView
}

// StreamState: the view of each Orca session is text only.
func (o *Orca) StreamState() wire.RuntimeStream {
	return wire.RuntimeStream{State: wire.StreamTextOnly, Reason: PollReason}
}

func (p *Poll) Caps() term.Caps {
	return term.Caps{Source: wire.SourcePoll, Writable: true, Reason: PollReason}
}

func (p *Poll) Control(context.Context, *term.Viewport) error { return term.ErrNoView }

func (p *Poll) Attach(ctx context.Context, _ term.Viewport) (term.Snapshot, <-chan term.Chunk, error) {
	first, err := p.read(ctx)
	if err != nil {
		return term.Snapshot{}, nil, err
	}
	out := make(chan term.Chunk)
	go p.loop(ctx, first, out)
	return first, out, nil
}

func (p *Poll) loop(ctx context.Context, last term.Snapshot, out chan<- term.Chunk) {
	defer close(out)
	t := time.NewTicker(p.every)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		snap, err := p.read(ctx)
		var c term.Chunk
		switch {
		case ctx.Err() != nil:
			return
		case gone(err):
			c = term.Chunk{Ended: "exited"}
		case err != nil:
			if failures++; failures < pollFailures {
				continue
			}
			c = term.Chunk{Ended: "closed"}
		case bytes.Equal(snap.Data, last.Data):
			failures = 0
			continue
		default:
			failures, last = 0, snap
			c = term.Chunk{Repaint: &snap}
		}
		select {
		case out <- c:
		case <-ctx.Done():
			return
		}
		if c.Ended != "" {
			return
		}
	}
}

func gone(err error) bool {
	var e *Error
	return errors.As(err, &e) && slices.Contains(goneTerminal, e.Code)
}

// read gives the screen as a repaint: home, clear, then the rows. The size is the rows and the widest row.
func (p *Poll) read(ctx context.Context) (term.Snapshot, error) {
	var r struct {
		Terminal struct {
			Tail []string `json:"tail"`
		} `json:"terminal"`
	}
	if err := p.o.callInto(ctx, 10*time.Second, &r, "terminal", "read", "--terminal", p.terminal, "--screen"); err != nil {
		return term.Snapshot{}, err
	}
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	cols := 0
	for i, row := range r.Terminal.Tail {
		if i > 0 {
			b.WriteString("\r\n")
		}
		row = strings.ReplaceAll(row, " ", " ")
		b.WriteString(row)
		cols = max(cols, utf8.RuneCountInString(row))
	}
	return term.Snapshot{Data: []byte(b.String()), Size: term.Viewport{Cols: cols, Rows: len(r.Terminal.Tail)}}, nil
}

// Write types the keys. Return is --enter and Ctrl+C is --interrupt; other bytes go as text.
func (p *Poll) Write(ctx context.Context, in []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	for _, k := range splitKeys(in) {
		args := []string{"terminal", "send", "--terminal", p.terminal}
		if k.text != "" {
			args = append(args, "--text="+k.text) // a value that starts with -- needs the = form
		}
		if k.flag != "" {
			args = append(args, k.flag)
		}
		if _, err := p.o.call(ctx, 10*time.Second, args...); err != nil {
			return err
		}
	}
	return nil
}

// key is one send call: text, then an optional --enter or --interrupt.
type key struct{ text, flag string }

// splitKeys cuts the input at each Return and Ctrl+C. A NUL cannot go in an argument, so it is dropped.
func splitKeys(in []byte) []key {
	var out []key
	var text []byte
	for _, b := range in {
		switch b {
		case '\r':
			out = append(out, key{string(text), "--enter"})
			text = text[:0]
		case 0x03:
			if len(text) > 0 {
				out = append(out, key{text: string(text)})
				text = text[:0]
			}
			out = append(out, key{flag: "--interrupt"})
		case 0:
		default:
			text = append(text, b)
		}
	}
	if len(text) > 0 {
		out = append(out, key{text: string(text)})
	}
	return out
}
