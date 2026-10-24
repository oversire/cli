package term

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/wire"
)

// The flow rules of one viewer.
const (
	MaxInFlight   = 512 << 10 // binary bytes that the client did not ack
	LagReset      = 5 * time.Second
	MaxTurnsFrame = 64 << 10
	pingEvery     = 30 * time.Second
	writeTimeout  = 10 * time.Second
	readLimit     = 1 << 20 // a large paste
)

// viewer is one WebSocket on the stream of one session.
type viewer struct {
	conn    *websocket.Conn
	session string
	leases  *Leases
	now     func() time.Time

	sent    uint64 // binary bytes sent; only the main loop writes it
	acked   atomic.Uint64
	ackSeen chan struct{}         // a new ack came
	holder  chan wire.LeaseHolder // the newest holder only
	frames  chan wire.Message     // client frames from the read loop
	inputs  chan []byte           // binary input from the read loop
	failed  chan error            // the read loop stopped
}

func newViewer(conn *websocket.Conn, session string, leases *Leases, now func() time.Time) *viewer {
	return &viewer{
		conn: conn, session: session, leases: leases, now: now,
		ackSeen: make(chan struct{}, 1), holder: make(chan wire.LeaseHolder, 1),
		frames: make(chan wire.Message, 16), inputs: make(chan []byte, 64), failed: make(chan error, 1),
	}
}

func (v *viewer) leaseChanged(h wire.LeaseHolder) {
	for {
		select {
		case v.holder <- h:
			return
		default:
			select {
			case <-v.holder:
			default:
			}
		}
	}
}

func (v *viewer) send(ctx context.Context, m wire.Message) error {
	b, err := wire.Encode(m)
	if err != nil {
		return err
	}
	wctx, done := context.WithTimeout(ctx, writeTimeout)
	defer done()
	return v.conn.Write(wctx, websocket.MessageText, b)
}

func (v *viewer) sendBinary(ctx context.Context, op wire.StreamOp, data []byte) error {
	v.sent += uint64(len(data))
	wctx, done := context.WithTimeout(ctx, writeTimeout)
	defer done()
	return v.conn.Write(wctx, websocket.MessageBinary, wire.EncodeBinary(op, v.sent, data))
}

func (v *viewer) fail(ctx context.Context, code, msg string) {
	_ = v.send(ctx, wire.StreamError{Code: code, Message: msg})
}

// readLoop sends the frames of the client to the main loop. Acks go to acked at once.
func (v *viewer) readLoop(ctx context.Context) {
	for {
		typ, data, err := v.conn.Read(ctx)
		if err != nil {
			v.failed <- err
			return
		}
		if typ == websocket.MessageBinary {
			op, _, p, err := wire.DecodeBinary(data)
			if err != nil || op != wire.OpInput {
				v.fail(ctx, "badFrame", "The daemon takes only input in a binary frame.")
				continue
			}
			select {
			case v.inputs <- p:
			case <-ctx.Done():
				return
			}
			continue
		}
		m, err := wire.DecodeClientFrame(data)
		if err != nil {
			v.fail(ctx, "badFrame", err.Error())
			continue
		}
		if a, ok := m.(wire.StreamAck); ok {
			if a.Seq > v.acked.Load() {
				v.acked.Store(a.Seq)
			}
			select {
			case v.ackSeen <- struct{}{}:
			default:
			}
			continue
		}
		select {
		case v.frames <- m:
		case <-ctx.Done():
			return
		}
	}
}

func (v *viewer) pinger(ctx context.Context, cancel func()) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, done := context.WithTimeout(ctx, writeTimeout)
			err := v.conn.Ping(pctx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func (v *viewer) inFlight() uint64 { return v.sent - min(v.acked.Load(), v.sent) }

// terminal serves a terminal view until the terminal ends or the client goes.
func (v *viewer) terminal(ctx context.Context, src Source, size Viewport) error {
	caps := src.Caps()
	attach := func() (Snapshot, <-chan Chunk, context.CancelFunc, error) {
		actx, cancel := context.WithCancel(ctx)
		snap, chunks, err := src.Attach(actx, size)
		if err != nil {
			cancel()
			return Snapshot{}, nil, nil, err
		}
		return snap, chunks, cancel, nil
	}
	snap, chunks, stop, err := attach()
	if err != nil {
		v.fail(ctx, "sourceFailed", err.Error())
		return err
	}
	defer func() { stop() }()
	if err := v.send(ctx, wire.StreamAttached{Source: caps.Source, Cols: snap.Size.Cols, Rows: snap.Size.Rows,
		Writable: caps.Writable, Resizable: caps.Resizable, Reason: caps.Reason}); err != nil {
		return err
	}
	if err := v.sendBinary(ctx, wire.OpSnapshot, snap.Data); err != nil {
		return err
	}
	v.leases.join(v.session, v)
	defer func() {
		if v.leases.leave(v.session, v) {
			_ = src.Control(context.WithoutCancel(ctx), nil)
		}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var pausedAt time.Time
	stale := false // the queue was dropped: attach again when the client catches up
	for {
		var in <-chan Chunk
		if v.inFlight() < MaxInFlight && !stale {
			in = chunks
			pausedAt = time.Time{}
		} else if pausedAt.IsZero() {
			pausedAt = v.now()
		}
		if stale && v.inFlight() < MaxInFlight {
			stop()
			if snap, chunks, stop, err = attach(); err != nil {
				v.fail(ctx, "sourceFailed", err.Error())
				return err
			}
			stale = false
			if err := v.sendBinary(ctx, wire.OpSnapshot, snap.Data); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-v.failed:
			return err
		case <-v.ackSeen:
		case h := <-v.holder:
			if err := v.send(ctx, wire.StreamLease{Holder: h}); err != nil {
				return err
			}
		case <-tick.C:
			if !pausedAt.IsZero() && !stale && v.now().Sub(pausedAt) >= LagReset {
				stale = true
				stop()
			}
			if v.leases.expired(v.session, v) {
				_ = src.Control(ctx, nil)
			}
		case p := <-v.inputs:
			if !caps.Writable {
				v.fail(ctx, "notWritable", "This view cannot type into the terminal.")
				continue
			}
			v.leases.touch(v.session, v)
			if err := src.Write(ctx, p); err != nil {
				v.fail(ctx, "sourceFailed", err.Error())
			}
		case m := <-v.frames:
			v.terminalFrame(ctx, src, caps, m)
		case c, ok := <-in:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return v.end(ctx, "closed")
			}
			if err := v.chunk(ctx, c); err != nil {
				return err
			}
			if c.Ended != "" {
				return v.end(ctx, c.Ended)
			}
		}
	}
}

func (v *viewer) chunk(ctx context.Context, c Chunk) error {
	switch {
	case c.Output != nil:
		return v.sendBinary(ctx, wire.OpOutput, c.Output)
	case c.Repaint != nil:
		return v.sendBinary(ctx, wire.OpSnapshot, c.Repaint.Data)
	case c.Resized != nil:
		return v.send(ctx, wire.StreamResized{Cols: c.Resized.Cols, Rows: c.Resized.Rows})
	case c.TakeBack:
		v.leases.takeBack(v.session)
	}
	return nil
}

func (v *viewer) terminalFrame(ctx context.Context, src Source, caps Caps, m wire.Message) {
	switch f := m.(type) {
	case wire.StreamControl:
		if !caps.Resizable {
			v.fail(ctx, "notResizable", "This source cannot change the size of the terminal.")
			return
		}
		if err := src.Control(ctx, &Viewport{Cols: f.Cols, Rows: f.Rows}); err != nil {
			v.fail(ctx, "sourceFailed", err.Error())
			return
		}
		v.leases.take(v.session, v)
	case wire.StreamRelease:
		if v.leases.release(v.session, v) {
			if err := src.Control(ctx, nil); err != nil {
				v.fail(ctx, "sourceFailed", err.Error())
			}
		}
	case wire.StreamSend:
		if !caps.Writable {
			v.fail(ctx, "notWritable", "This view cannot type into the terminal.")
			return
		}
		if err := src.Write(ctx, []byte(f.Text)); err != nil {
			v.fail(ctx, "sourceFailed", err.Error())
		}
	case wire.StreamInterrupt:
		if !caps.Writable {
			v.fail(ctx, "notWritable", "This view cannot type into the terminal.")
			return
		}
		if err := src.Write(ctx, []byte{0x03}); err != nil {
			v.fail(ctx, "sourceFailed", err.Error())
		}
	default:
		v.fail(ctx, "badFrame", "The daemon does not take a "+m.Type()+" frame now.")
	}
}

// chat serves a chat view until the session ends or the client goes.
func (v *viewer) chat(ctx context.Context, src TurnSource, after int) error {
	history, live, err := src.Turns(ctx, after)
	if err != nil {
		v.fail(ctx, "sourceFailed", err.Error())
		return err
	}
	if err := v.send(ctx, wire.StreamAttached{Source: wire.SourceWire, Writable: true}); err != nil {
		return err
	}
	for _, f := range TurnFrames(history) {
		if err := v.send(ctx, f); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-v.failed:
			return err
		case <-v.inputs:
			v.fail(ctx, "notWritable", "The chat view takes text in a send frame.")
		case m := <-v.frames:
			var err error
			switch f := m.(type) {
			case wire.StreamSend:
				err = src.Send(ctx, f.Text)
			case wire.StreamInterrupt:
				err = src.Interrupt(ctx)
			default:
				v.fail(ctx, "badFrame", "The chat view takes only send and interrupt frames.")
				continue
			}
			if err != nil {
				v.fail(ctx, "sourceFailed", err.Error())
			}
		case t, ok := <-live:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return v.end(ctx, "exited")
			}
			if err := v.send(ctx, wire.StreamTurns{From: t.Seq, To: t.Seq, Turns: []wire.SessionTurn{t}}); err != nil {
				return err
			}
		}
	}
}

func (v *viewer) end(ctx context.Context, reason string) error {
	_ = v.send(ctx, wire.StreamEnded{Reason: reason})
	return v.conn.Close(websocket.StatusNormalClosure, reason)
}

// TurnFrames splits turns into frames of at most MaxTurnsFrame bytes of JSON. A larger turn goes alone.
func TurnFrames(turns []wire.SessionTurn) []wire.StreamTurns {
	var out []wire.StreamTurns
	var cur []wire.SessionTurn
	size := 0
	const overhead = 64 // {"type":"turns","from":…,"to":…,"turns":[]}
	flush := func() {
		if len(cur) > 0 {
			out = append(out, wire.StreamTurns{From: cur[0].Seq, To: cur[len(cur)-1].Seq, Turns: cur})
			cur, size = nil, 0
		}
	}
	for _, t := range turns {
		b, _ := json.Marshal(t)
		if len(cur) > 0 && overhead+size+len(b)+1 > MaxTurnsFrame {
			flush()
		}
		cur = append(cur, t)
		size += len(b) + 1
	}
	flush()
	return out
}
