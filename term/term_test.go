package term

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// fakeSource sends what the test puts into out.
type fakeSource struct {
	caps Caps

	mu       sync.Mutex
	attaches int
	out      chan Chunk
	writes   []string
	controls []*Viewport
}

func newFakeSource(caps Caps) *fakeSource {
	return &fakeSource{caps: caps, out: make(chan Chunk, 1024)}
}

func (f *fakeSource) Attach(ctx context.Context, v Viewport) (Snapshot, <-chan Chunk, error) {
	f.mu.Lock()
	f.attaches++
	n := f.attaches
	f.mu.Unlock()
	ch := make(chan Chunk)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-f.out:
				select {
				case ch <- c:
				case <-ctx.Done():
					return
				}
				if c.Ended != "" {
					return
				}
			}
		}
	}()
	return Snapshot{Data: []byte("snap" + strings.Repeat("!", n-1)), Size: Viewport{80, 24}}, ch, nil
}

func (f *fakeSource) Write(_ context.Context, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, string(p))
	return nil
}

func (f *fakeSource) Control(_ context.Context, v *Viewport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, v)
	return nil
}

func (f *fakeSource) Caps() Caps { return f.caps }

func (f *fakeSource) state() (int, []string, []*Viewport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attaches, append([]string(nil), f.writes...), append([]*Viewport(nil), f.controls...)
}

type fakeTurns struct {
	history []wire.SessionTurn
	live    chan wire.SessionTurn
	mu      sync.Mutex
	after   int
	sent    []string
}

func (f *fakeTurns) Turns(_ context.Context, after int) ([]wire.SessionTurn, <-chan wire.SessionTurn, error) {
	f.mu.Lock()
	f.after = after
	f.mu.Unlock()
	var out []wire.SessionTurn
	for _, t := range f.history {
		if t.Seq > after {
			out = append(out, t)
		}
	}
	return out, f.live, nil
}

func (f *fakeTurns) Send(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}

type fakeOpener struct {
	terminal func() Source
	chat     TurnSource
}

func (o fakeOpener) Terminal(context.Context, wire.DaemonSession) (Source, error) {
	if o.terminal == nil {
		return nil, ErrNoView
	}
	return o.terminal(), nil
}

func (o fakeOpener) Chat(context.Context, wire.DaemonSession) (TurnSource, error) {
	if o.chat == nil {
		return nil, ErrNoView
	}
	return o.chat, nil
}

type book map[string]wire.DaemonSession

func (b book) Get(id string) (wire.DaemonSession, bool) {
	s, ok := b[id]
	return s, ok
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newServer(t *testing.T, open Opener, now func() time.Time) *httptest.Server {
	t.Helper()
	api := NewAPI(book{"s-1": {ID: "s-1", Runtime: "orca"}}, open, now)
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type client struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan frame
	err    chan error
}

func dial(t *testing.T, srv *httptest.Server, attach wire.StreamAttach) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sessions/s-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(4 << 20)
	t.Cleanup(func() { conn.CloseNow() })
	c := &client{t: t, conn: conn, frames: make(chan frame, 1024), err: make(chan error, 1)}
	go c.pump()
	c.json(attach)
	return c
}

// pump reads in its own goroutine: a Read with a timeout closes the connection.
func (c *client) pump() {
	for {
		typ, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.err <- err
			return
		}
		var f frame
		if typ == websocket.MessageBinary {
			op, seq, p, err := wire.DecodeBinary(data)
			if err != nil {
				c.err <- err
				return
			}
			f = frame{op: op, seq: seq, data: string(p)}
		} else if f.msg, err = wire.DecodeDaemonFrame(data); err != nil {
			c.err <- err
			return
		}
		c.frames <- f
	}
}

func (c *client) json(m wire.Message) {
	c.t.Helper()
	b, _ := wire.Encode(m)
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) input(p string) {
	c.t.Helper()
	if err := c.conn.Write(context.Background(), websocket.MessageBinary, wire.EncodeBinary(wire.OpInput, 0, []byte(p))); err != nil {
		c.t.Fatal(err)
	}
}

// frame is one frame from the daemon: a JSON Message, or a binary frame.
type frame struct {
	msg  wire.Message
	op   wire.StreamOp
	seq  uint64
	data string
}

func (c *client) next() frame {
	c.t.Helper()
	f, err := c.read(5 * time.Second)
	if err != nil {
		c.t.Fatal(err)
	}
	return f
}

func (c *client) read(d time.Duration) (frame, error) {
	select {
	case f := <-c.frames:
		return f, nil
	case err := <-c.err:
		return frame{}, err
	case <-time.After(d):
		return frame{}, errors.New("no frame")
	}
}

// until reads frames until one passes ok.
func (c *client) until(ok func(frame) bool) frame {
	c.t.Helper()
	for range 100 {
		if f := c.next(); ok(f) {
			return f
		}
	}
	c.t.Fatal("no such frame")
	return frame{}
}

func isLease(h wire.LeaseHolder) func(frame) bool {
	return func(f frame) bool { l, ok := f.msg.(wire.StreamLease); return ok && l.Holder == h }
}

func TestAttachSnapshotThenOutputInOrder(t *testing.T) {
	src := newFakeSource(Caps{Source: wire.SourceStream, Writable: true, Resizable: true})
	srv := newServer(t, fakeOpener{terminal: func() Source { return src }}, time.Now)
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal, Cols: 100, Rows: 30})

	at, ok := c.next().msg.(wire.StreamAttached)
	if !ok || at.Source != wire.SourceStream || at.Cols != 80 || !at.Writable || !at.Resizable {
		t.Fatalf("attached %+v", at)
	}
	snap := c.next()
	if snap.op != wire.OpSnapshot || snap.data != "snap" || snap.seq != 4 {
		t.Fatalf("snapshot %+v", snap)
	}
	c.until(isLease(wire.HolderNone))
	src.out <- Chunk{Output: []byte("ab")}
	src.out <- Chunk{Output: []byte("cde")}
	if f := c.next(); f.op != wire.OpOutput || f.data != "ab" || f.seq != 6 {
		t.Fatalf("output %+v", f)
	}
	if f := c.next(); f.data != "cde" || f.seq != 9 {
		t.Fatalf("output %+v", f)
	}
	src.out <- Chunk{Resized: &Viewport{90, 20}}
	if r, ok := c.next().msg.(wire.StreamResized); !ok || r.Cols != 90 {
		t.Fatalf("resized %+v", r)
	}
	src.out <- Chunk{Ended: "exited"}
	if e, ok := c.next().msg.(wire.StreamEnded); !ok || e.Reason != "exited" {
		t.Fatalf("ended %+v", e)
	}
}

func TestInputAndWritable(t *testing.T) {
	src := newFakeSource(Caps{Source: wire.SourceStream, Writable: true, Resizable: true})
	srv := newServer(t, fakeOpener{terminal: func() Source { return src }}, time.Now)
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal})
	c.until(isLease(wire.HolderNone))
	c.input("ls\r")
	c.json(wire.StreamSend{Text: "hi"})
	waitFor(t, func() bool { _, w, _ := src.state(); return len(w) == 2 })
	if _, w, _ := src.state(); w[0] != "ls\r" || w[1] != "hi" {
		t.Fatalf("writes %q", w)
	}

	ro := newFakeSource(Caps{Source: wire.SourcePoll})
	srv2 := newServer(t, fakeOpener{terminal: func() Source { return ro }}, time.Now)
	c2 := dial(t, srv2, wire.StreamAttach{View: wire.ViewTerminal})
	c2.input("x")
	if e, ok := c2.until(func(f frame) bool { _, ok := f.msg.(wire.StreamError); return ok }).msg.(wire.StreamError); !ok || e.Code != "notWritable" {
		t.Fatalf("error %+v", e)
	}
	c2.json(wire.StreamControl{Cols: 10, Rows: 10})
	if e := c2.until(func(f frame) bool { _, ok := f.msg.(wire.StreamError); return ok }).msg.(wire.StreamError); e.Code != "notResizable" {
		t.Fatalf("error %+v", e)
	}
}

func TestCreditsStopAndStartTheFlow(t *testing.T) {
	src := newFakeSource(Caps{Source: wire.SourceStream})
	srv := newServer(t, fakeOpener{terminal: func() Source { return src }}, time.Now)
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal})
	c.until(isLease(wire.HolderNone))
	block := strings.Repeat("x", 64<<10)
	for range 10 {
		src.out <- Chunk{Output: []byte(block)}
	}
	var last uint64
	for range 8 { // 8 × 64 KiB plus the snapshot is over 512 KiB
		last = c.next().seq
	}
	if _, err := c.read(300 * time.Millisecond); err == nil {
		t.Fatal("the daemon sent over the credit")
	}
	c.json(wire.StreamAck{Seq: last})
	if f := c.next(); f.op != wire.OpOutput {
		t.Fatalf("no output after the ack: %+v", f)
	}
}

func TestLagGivesANewSnapshot(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	src := newFakeSource(Caps{Source: wire.SourceStream})
	srv := newServer(t, fakeOpener{terminal: func() Source { return src }}, clk.now)
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal})
	c.until(isLease(wire.HolderNone))
	block := strings.Repeat("x", 64<<10)
	for range 12 {
		src.out <- Chunk{Output: []byte(block)}
	}
	var last uint64
	for range 8 {
		last = c.next().seq
	}
	time.Sleep(100 * time.Millisecond)
	clk.add(LagReset + time.Second)
	time.Sleep(1200 * time.Millisecond) // one tick sees the lag
	c.json(wire.StreamAck{Seq: last})
	f := c.until(func(f frame) bool { return f.op == wire.OpSnapshot })
	if f.data != "snap!" {
		t.Fatalf("snapshot %+v", f)
	}
	if n, _, _ := src.state(); n != 2 {
		t.Fatalf("attaches %d", n)
	}
}

func TestLeaseRules(t *testing.T) {
	a := newFakeSource(Caps{Source: wire.SourceStream, Writable: true, Resizable: true})
	b := newFakeSource(Caps{Source: wire.SourceStream, Writable: true, Resizable: true})
	srcs := make(chan Source, 2)
	srcs <- a
	srcs <- b
	clk := &clock{t: time.Unix(1000, 0)}
	srv := newServer(t, fakeOpener{terminal: func() Source { return <-srcs }}, clk.now)
	ca := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal})
	ca.until(isLease(wire.HolderNone))
	cb := dial(t, srv, wire.StreamAttach{View: wire.ViewTerminal})
	cb.until(isLease(wire.HolderNone))

	ca.json(wire.StreamControl{Cols: 120, Rows: 40})
	ca.until(isLease(wire.HolderYou))
	cb.until(isLease(wire.HolderOther))
	if _, _, ctl := a.state(); len(ctl) != 1 || *ctl[0] != (Viewport{120, 40}) {
		t.Fatalf("controls %v", ctl)
	}
	a.out <- Chunk{TakeBack: true}
	ca.until(isLease(wire.HolderDesktop))
	cb.until(isLease(wire.HolderDesktop))

	cb.json(wire.StreamControl{Cols: 50, Rows: 20})
	cb.until(isLease(wire.HolderYou))
	ca.until(isLease(wire.HolderOther))
	cb.json(wire.StreamRelease{})
	ca.until(isLease(wire.HolderNone))
	waitFor(t, func() bool { _, _, ctl := b.state(); return len(ctl) == 2 && ctl[1] == nil })

	// A closed holder gives the size back.
	cb.json(wire.StreamControl{Cols: 50, Rows: 20})
	ca.until(isLease(wire.HolderOther))
	cb.conn.Close(websocket.StatusNormalClosure, "")
	ca.until(isLease(wire.HolderNone))
	waitFor(t, func() bool { _, _, ctl := b.state(); return len(ctl) == 4 && ctl[3] == nil })

	// No input for LeaseIdle gives the size back.
	ca.json(wire.StreamControl{Cols: 70, Rows: 20})
	ca.until(isLease(wire.HolderYou))
	clk.add(LeaseIdle + time.Second)
	ca.until(isLease(wire.HolderNone))
}

func TestChat(t *testing.T) {
	at := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	ft := &fakeTurns{live: make(chan wire.SessionTurn, 4)}
	for i := 1; i <= 5; i++ {
		ft.history = append(ft.history, wire.SessionTurn{Seq: i, Role: wire.RoleAssistant, At: at, Text: strings.Repeat("y", 30<<10)})
	}
	srv := newServer(t, fakeOpener{chat: ft}, time.Now)
	after := 1
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewChat, After: &after})
	if a, ok := c.next().msg.(wire.StreamAttached); !ok || a.Source != wire.SourceWire || !a.Writable {
		t.Fatalf("attached %+v", a)
	}
	var seqs []int
	for len(seqs) < 4 {
		tf, ok := c.next().msg.(wire.StreamTurns)
		if !ok || len(tf.Turns) > 2 || tf.From != tf.Turns[0].Seq {
			t.Fatalf("turns %+v", tf)
		}
		for _, x := range tf.Turns {
			seqs = append(seqs, x.Seq)
		}
	}
	if seqs[0] != 2 || seqs[3] != 5 {
		t.Fatalf("seqs %v", seqs)
	}
	ft.live <- wire.SessionTurn{Seq: 6, Role: wire.RoleUser, At: at, Text: "go on"}
	if tf := c.next().msg.(wire.StreamTurns); tf.From != 6 || tf.Turns[0].Text != "go on" {
		t.Fatalf("live %+v", tf)
	}
	c.json(wire.StreamSend{Text: "stop"})
	waitFor(t, func() bool { ft.mu.Lock(); defer ft.mu.Unlock(); return len(ft.sent) == 1 })
	close(ft.live)
	if e, ok := c.next().msg.(wire.StreamEnded); !ok || e.Reason != "exited" {
		t.Fatalf("ended %+v", e)
	}
}

func TestTurnFrames(t *testing.T) {
	small := wire.SessionTurn{Seq: 1, Role: wire.RoleUser, Text: "a"}
	big := wire.SessionTurn{Seq: 2, Role: wire.RoleAssistant, Text: strings.Repeat("z", 70<<10)}
	fs := TurnFrames([]wire.SessionTurn{small, big, {Seq: 3, Text: "b"}})
	if len(fs) != 3 || fs[1].From != 2 || fs[1].To != 2 {
		t.Fatalf("frames %+v", len(fs))
	}
	if TurnFrames(nil) != nil {
		t.Fatal("frames for no turns")
	}
}

func TestRouteIsMember(t *testing.T) {
	// The server checks the access: loopback with no token, the tailnet with the group credential.
	r := NewAPI(book{}, fakeOpener{}, time.Now).Routes()
	if len(r) != 1 || r[0].Pattern != "GET /v1/sessions/{id}/stream" || r[0].Access != server.Member {
		t.Fatalf("%+v", r)
	}
}

func TestRefusals(t *testing.T) {
	srv := newServer(t, fakeOpener{}, time.Now)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/sessions/s-1/stream", nil)
	req.Header.Set("Origin", "https://evil.example")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("origin: %v %v", res.StatusCode, err)
	}
	if res, err := http.Get(srv.URL + "/v1/sessions/nope/stream"); err != nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown: %v %v", res.StatusCode, err)
	}
	c := dial(t, srv, wire.StreamAttach{View: wire.ViewChat})
	if e, ok := c.next().msg.(wire.StreamError); !ok || e.Code != "unsupported" {
		t.Fatalf("error %+v", e)
	}
	// The first frame must be attach.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sessions/s-1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := wire.Encode(wire.StreamAck{Seq: 1})
	_ = conn.Write(ctx, websocket.MessageText, b)
	_, _, err = conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Fatalf("close %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout")
}
