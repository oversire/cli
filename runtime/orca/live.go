package orca

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/oversire/cli/runtime/orca/orcaws"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// Live is the live view of one Orca terminal over the paired socket: terminal.subscribe, terminal.send and terminal.resizeForClient.
type Live struct {
	pair     *Pairing
	terminal string
	client   string // the client id of this viewer, for the size
	holding  atomic.Bool

	ctlMu sync.Mutex
	ctl   *orcaws.Conn // requests go on their own socket: a slow stream holds its socket
}

func newLive(p *Pairing, terminal string) *Live {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Live{pair: p, terminal: terminal, client: "oversire-" + hex.EncodeToString(b)}
}

func (l *Live) Caps() term.Caps {
	return term.Caps{Source: wire.SourceStream, Writable: true, Resizable: true}
}

// The events of a terminal.subscribe stream.
type liveEvent struct {
	Type       string   `json:"type"`
	Lines      []string `json:"lines"`
	Serialized string   `json:"serialized"`
	Chunk      string   `json:"chunk"`
	Mode       string   `json:"mode"`
	Cols       int      `json:"cols"`
	Rows       int      `json:"rows"`
}

func (e liveEvent) snapshot() term.Snapshot {
	data := e.Serialized
	if data == "" {
		data = "\x1b[H\x1b[2J" + strings.Join(e.Lines, "\r\n")
	}
	return term.Snapshot{Data: []byte(data), Size: term.Viewport{Cols: e.Cols, Rows: e.Rows}}
}

// Attach subscribes on a new socket. The view does not change the size until Control.
func (l *Live) Attach(ctx context.Context, _ term.Viewport) (term.Snapshot, <-chan term.Chunk, error) {
	c, err := l.pair.Dial(ctx)
	if err != nil {
		return term.Snapshot{}, nil, err
	}
	replies, err := c.Stream(ctx, "terminal.subscribe", map[string]string{"terminal": l.terminal})
	if err != nil {
		c.Close()
		return term.Snapshot{}, nil, err
	}
	first, err := next(replies, c)
	if err != nil {
		c.Close()
		return term.Snapshot{}, nil, err
	}
	out := make(chan term.Chunk)
	go func() {
		defer close(out)
		defer l.Close() // the keys and the size go only while a view is attached
		defer c.Close()
		if first.Type == "subscribed" { // no live pty: Orca gave the last lines and ends
			l.emit(ctx, out, term.Chunk{Ended: "exited"})
			return
		}
		l.loop(ctx, c, replies, out)
	}()
	return first.snapshot(), out, nil
}

// next gives the next event, or the error of the reply or the socket.
func next(replies <-chan orcaws.Reply, c *orcaws.Conn) (liveEvent, error) {
	var e liveEvent
	r, ok := <-replies
	switch {
	case !ok:
		if err := c.Err(); err != nil {
			return e, err
		}
		return e, errors.New("The Orca stream ended.")
	case !r.OK && r.Error != nil:
		return e, r.Error
	case !r.OK:
		return e, errors.New("Orca refused the stream.")
	}
	return e, json.Unmarshal(r.Result, &e)
}

func (l *Live) loop(ctx context.Context, c *orcaws.Conn, replies <-chan orcaws.Reply, out chan<- term.Chunk) {
	for {
		e, err := next(replies, c)
		if ctx.Err() != nil {
			return
		}
		var chunks []term.Chunk
		switch {
		case err != nil:
			var oe *orcaws.Error
			if errors.As(err, &oe) && slices.Contains(goneTerminal, oe.Code) {
				chunks = []term.Chunk{{Ended: "exited"}}
			} else {
				chunks = []term.Chunk{{Ended: "closed"}}
			}
		case e.Type == "data":
			chunks = []term.Chunk{{Output: []byte(e.Chunk)}}
		case e.Type == "scrollback":
			snap := e.snapshot()
			chunks = []term.Chunk{{Repaint: &snap}}
		case e.Type == "fit-override-changed":
			chunks = []term.Chunk{{Resized: &term.Viewport{Cols: e.Cols, Rows: e.Rows}}}
			if e.Mode != "mobile-fit" && l.holding.Swap(false) {
				chunks = append(chunks, term.Chunk{TakeBack: true})
			}
		case e.Type == "end":
			chunks = []term.Chunk{{Ended: "exited"}}
		}
		for _, ch := range chunks {
			if !l.emit(ctx, out, ch) || ch.Ended != "" {
				return
			}
		}
	}
}

func (l *Live) emit(ctx context.Context, out chan<- term.Chunk, c term.Chunk) bool {
	select {
	case out <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// call sends one request on the request socket, and dials it again once when it is closed.
func (l *Live) call(ctx context.Context, method string, params, out any) error {
	l.ctlMu.Lock()
	defer l.ctlMu.Unlock()
	for try := 0; ; try++ {
		if l.ctl == nil {
			c, err := l.pair.Dial(ctx)
			if err != nil {
				return err
			}
			l.ctl = c
		}
		err := l.ctl.Call(ctx, method, params, out)
		if err == nil || try > 0 || l.ctl.Err() == nil && !isClosed(l.ctl) {
			return err
		}
		l.ctl.Close()
		l.ctl = nil
	}
}

func isClosed(c *orcaws.Conn) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// Write types the keys. Return and Ctrl+C go as the enter and interrupt flags of terminal.send.
func (l *Live) Write(ctx context.Context, in []byte) error {
	for _, k := range splitKeys(in) {
		params := map[string]any{"terminal": l.terminal}
		if k.text != "" {
			params["text"] = k.text
		}
		switch k.flag {
		case "--enter":
			params["enter"] = true
		case "--interrupt":
			params["interrupt"] = true
		}
		var r struct {
			Send struct {
				Accepted *bool `json:"accepted"`
			} `json:"send"`
		}
		if err := l.call(ctx, "terminal.send", params, &r); err != nil {
			return err
		}
		if r.Send.Accepted != nil && !*r.Send.Accepted {
			return errors.New("Orca did not take the input. The terminal is locked to another client.")
		}
	}
	return nil
}

// Control fits the terminal to v. nil gives the size back to Orca.
func (l *Live) Control(ctx context.Context, v *term.Viewport) error {
	params := map[string]any{"terminal": l.terminal, "mode": "restore", "clientId": l.client}
	if v != nil {
		params["mode"], params["cols"], params["rows"] = "mobile-fit", v.Cols, v.Rows
	}
	l.holding.Store(v != nil)
	return l.call(ctx, "terminal.resizeForClient", params, nil)
}

// Close closes the request socket.
func (l *Live) Close() {
	l.ctlMu.Lock()
	defer l.ctlMu.Unlock()
	if l.ctl != nil {
		l.ctl.Close()
		l.ctl = nil
	}
}
