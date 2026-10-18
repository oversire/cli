package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/runner/wire"
)

type fakeRuntimes struct {
	mu    sync.Mutex
	calls []string
	gate  chan struct{}
}

func (f *fakeRuntimes) IDs() []string      { return []string{"orca"} }
func (f *fakeRuntimes) Has(id string) bool { return id == "orca" }
func (f *fakeRuntimes) Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, o.ID)
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	if o.ID == "boom" {
		return nil, errors.New("orca broke")
	}
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: "local"}, ID: "w1"}, ID: "t1"}
	return &wire.WorkOrderResult{Session: &s}, nil
}

func (f *fakeRuntimes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeAllow struct{ refuse string }

func (a fakeAllow) Check(wire.WorkOrder) error {
	if a.refuse != "" {
		return errors.New(a.refuse)
	}
	return nil
}
func (a fakeAllow) Summary() wire.AllowSummary {
	return wire.AllowSummary{Runtimes: []string{"orca"}, Repos: 2, Agents: []string{"claude"}}
}

type nopStatus struct{}

func (nopStatus) Connected(time.Time) {}
func (nopStatus) Disconnected(error)  {}
func (nopStatus) Order(string)        {}

// fakeHub runs script for each connection. n counts from 1.
type fakeHub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	conns  int
	header string
}

func newFakeHub(t *testing.T, status int, script func(ctx context.Context, c *websocket.Conn, n int)) *fakeHub {
	h := &fakeHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.conns++
		n := h.conns
		h.header = r.Header.Get("Authorization")
		h.mu.Unlock()
		if r.URL.Path != "/v1/connect" {
			http.NotFound(w, r)
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		script(r.Context(), c, n)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

// next reads the next JSON message and skips ping frames.
func next(t *testing.T, ctx context.Context, c *websocket.Conn) wire.Message {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Errorf("read: %v", err)
			return nil
		}
		if string(data) == "ping" {
			continue
		}
		m, err := wire.DecodeMessage(data)
		if err != nil {
			t.Errorf("decode %s: %v", data, err)
		}
		return m
	}
}

func send(ctx context.Context, c *websocket.Conn, m wire.Message) {
	b, _ := wire.Encode(m)
	c.Write(ctx, websocket.MessageText, b)
}

func order(id string, expires time.Time) wire.Order {
	return wire.Order{Order: wire.WorkOrder{
		ID: id, Runner: "r1", Runtime: "orca", Suggestion: "s1", State: wire.StateSent,
		CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: expires,
		Payload: wire.LaunchRequest{Repo: "/w", Name: "ENG-4", Agent: "claude", Prompt: "go"},
	}}
}

func client(t *testing.T, h *fakeHub, rt *fakeRuntimes, allow fakeAllow) *Client {
	c := NewClient(h.srv.URL, "tok", "0.1.0", rt, allow, OpenJournal(filepath.Join(t.TempDir(), "orders.json")), nopStatus{})
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	c.MinBackoff = 10 * time.Millisecond
	return c
}

func start(t *testing.T, c *Client) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop")
			return nil
		}
	}
}

func welcome(t *testing.T, ctx context.Context, c *websocket.Conn) wire.Hello {
	m := next(t, ctx, c)
	hello, ok := m.(wire.Hello)
	if !ok {
		t.Fatalf("first message %#v", m)
	}
	send(ctx, c, wire.Welcome{Device: "r1", Contract: 1})
	return hello
}

func TestHelloAndOrderFlow(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan []wire.Message, 1)
	var hello wire.Hello
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		hello = welcome(t, ctx, c)
		send(ctx, c, order("o1", time.Now().Add(time.Hour)))
		got <- []wire.Message{next(t, ctx, c), next(t, ctx, c)}
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	msgs := <-got
	stop()

	want := wire.Hello{Version: "0.1.0", Contract: 1, Runtimes: []string{"orca"}, OS: runtime.GOOS + "/" + runtime.GOARCH,
		Allow: &wire.AllowSummary{Runtimes: []string{"orca"}, Repos: 2, Agents: []string{"claude"}}}
	if hello.Version != want.Version || hello.Contract != 1 || !slices.Equal(hello.Runtimes, want.Runtimes) || hello.OS != want.OS || hello.Allow.Repos != 2 {
		t.Fatalf("hello %#v", hello)
	}
	if h.header != "Bearer tok" {
		t.Fatalf("header %q", h.header)
	}
	if a, ok := msgs[0].(wire.Ack); !ok || a.Order != "o1" {
		t.Fatalf("first %#v", msgs[0])
	}
	r, ok := msgs[1].(wire.Result)
	if !ok || r.State != wire.ResultDone || r.Result.Session.ID != "t1" {
		t.Fatalf("second %#v", msgs[1])
	}
	if rt.count() != 1 {
		t.Fatal("runtime calls", rt.calls)
	}
}

func TestRuntimeErrorFails(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan wire.Message, 2)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		send(ctx, c, order("boom", time.Now().Add(time.Hour)))
		got <- next(t, ctx, c)
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	<-got
	r := (<-got).(wire.Result)
	stop()
	if r.State != wire.ResultFailed || r.Result.Error != "orca broke" {
		t.Fatalf("%#v", r)
	}
}

func TestExpiredOrderNeverRuns(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan wire.Message, 1)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		send(ctx, c, order("o3", time.Now().Add(-time.Second)))
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	m := <-got
	stop()
	if r, ok := m.(wire.Result); !ok || r.State != wire.ResultExpired || r.Result != nil {
		t.Fatalf("%#v", m)
	}
	if rt.count() != 0 {
		t.Fatal("an expired order ran")
	}
}

func TestHeldOrderNeverRuns(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan wire.Message, 1)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		held := order("o5", time.Now().Add(time.Hour))
		held.Order.State = wire.StateHeld
		send(ctx, c, held)
		send(ctx, c, order("o6", time.Now().Add(-time.Second)))
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	m := <-got
	stop()
	if r, ok := m.(wire.Result); !ok || r.Order != "o6" {
		t.Fatalf("the held order sent a message: %#v", m)
	}
	if rt.count() != 0 {
		t.Fatal("a held order ran")
	}
}

func TestAllowListRefusalStartsNothing(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan wire.Message, 2)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		send(ctx, c, order("o2", time.Now().Add(time.Hour)))
		got <- next(t, ctx, c)
		o := order("o4", time.Now().Add(time.Hour))
		o.Order.Runtime = "oversire"
		send(ctx, c, o)
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{refuse: "The allow list has no repo /w."}))
	first, second := (<-got).(wire.Result), (<-got).(wire.Result)
	stop()
	if first.State != wire.ResultFailed || first.Result.Error != "The allow list has no repo /w." {
		t.Fatalf("%#v", first)
	}
	if second.State != wire.ResultFailed || second.Result.Error != "This runner has no runtime oversire." {
		t.Fatalf("%#v", second)
	}
	if rt.count() != 0 {
		t.Fatal("a refused order ran")
	}
}

func TestResentFinishedOrderDoesNotRunTwice(t *testing.T) {
	rt := &fakeRuntimes{}
	got := make(chan wire.Message, 3)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		o := order("o1", time.Now().Add(time.Hour))
		send(ctx, c, o)
		got <- next(t, ctx, c)
		got <- next(t, ctx, c)
		send(ctx, c, o)
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	<-got
	<-got
	again := <-got
	stop()
	if r, ok := again.(wire.Result); !ok || r.State != wire.ResultDone || r.Result.Session == nil {
		t.Fatalf("re-sent order gave %#v", again)
	}
	if rt.count() != 1 {
		t.Fatalf("runtime calls %v", rt.calls)
	}
}

func TestReconnectAfterDropAndFlushResult(t *testing.T) {
	rt := &fakeRuntimes{gate: make(chan struct{})}
	got := make(chan wire.Message, 3)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		if n == 1 {
			send(ctx, c, order("o1", time.Now().Add(time.Hour)))
			got <- next(t, ctx, c)
			c.Close(websocket.StatusGoingAway, "hibernate")
			close(rt.gate)
			return
		}
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	stop := start(t, client(t, h, rt, fakeAllow{}))
	ack := <-got
	r := <-got
	stop()
	if _, ok := ack.(wire.Ack); !ok {
		t.Fatalf("%#v", ack)
	}
	if res, ok := r.(wire.Result); !ok || res.Order != "o1" || res.State != wire.ResultDone {
		t.Fatalf("the result after the reconnect: %#v", r)
	}
	if h.count() < 2 {
		t.Fatal("no reconnect")
	}
}

func TestPingAndIgnoredFrames(t *testing.T) {
	pinged := make(chan bool, 1)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		c.Write(ctx, websocket.MessageText, []byte("pong"))
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"store","head":3}`))
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if string(data) == "ping" {
				pinged <- true
				<-ctx.Done()
				return
			}
		}
	})
	c := client(t, h, &fakeRuntimes{}, fakeAllow{})
	c.PingEvery = 20 * time.Millisecond
	stop := start(t, c)
	select {
	case <-pinged:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping")
	}
	stop()
	if h.count() != 1 {
		t.Fatal("pong or an unknown type broke the connection")
	}
}

func TestRefusedStops(t *testing.T) {
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		next(t, ctx, c)
		send(ctx, c, wire.Refused{Reason: "This runner knows work-order schema 0.", Contracts: []int{2}})
		c.Close(websocket.StatusPolicyViolation, "refused")
	})
	err := client(t, h, &fakeRuntimes{}, fakeAllow{}).Run(context.Background())
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "Update the runner") {
		t.Fatalf("got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if h.count() != 1 {
		t.Fatalf("%d connects after refused", h.count())
	}
}

func TestUnauthorizedStops(t *testing.T) {
	h := newFakeHub(t, http.StatusUnauthorized, nil)
	err := client(t, h, &fakeRuntimes{}, fakeAllow{}).Run(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if h.count() != 1 {
		t.Fatalf("%d connects after 401", h.count())
	}
}

func TestBadOrderFails(t *testing.T) {
	got := make(chan wire.Message, 1)
	h := newFakeHub(t, 0, func(ctx context.Context, c *websocket.Conn, n int) {
		welcome(t, ctx, c)
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"order","order":{"id":"o9","op":"shell","payload":{"cmd":"x"}}}`))
		got <- next(t, ctx, c)
		<-ctx.Done()
	})
	rt := &fakeRuntimes{}
	stop := start(t, client(t, h, rt, fakeAllow{}))
	m := <-got
	stop()
	if r, ok := m.(wire.Result); !ok || r.Order != "o9" || r.State != wire.ResultFailed {
		t.Fatalf("%#v", m)
	}
	if rt.count() != 0 {
		t.Fatal("a bad order ran")
	}
}

func TestConnectURL(t *testing.T) {
	if got := ConnectURL("https://hub.example/"); got != "wss://hub.example/v1/connect" {
		t.Fatal(got)
	}
	if got := ConnectURL("http://127.0.0.1:8787"); got != "ws://127.0.0.1:8787/v1/connect" {
		t.Fatal(got)
	}
}

func TestJournalKeepsTheLast500(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.json")
	j := OpenJournal(path)
	for i := range JournalSize + 5 {
		j.Put(wire.Result{Order: "o" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), State: wire.ResultDone})
	}
	j = OpenJournal(path)
	if len(j.entries) != JournalSize {
		t.Fatal(len(j.entries))
	}
	if _, ok := j.Get("oa"); ok {
		t.Fatal("the oldest entry stays")
	}
}

func runnerSession(id string, state wire.RunnerSessionState, at time.Time) wire.RunnerSession {
	return wire.RunnerSession{
		ID: id, Runner: "r1", Order: "o1", Agent: "claude", Repo: "/w", State: state, StartedAt: at, UpdatedAt: at,
		Session: wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "w/ENG-4"}, ID: id},
	}
}

func TestSessionReportsAndResendAfterWelcome(t *testing.T) {
	got := make(chan wire.Message, 8)
	var c *Client
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	h := newFakeHub(t, 0, func(ctx context.Context, conn *websocket.Conn, n int) {
		welcome(t, ctx, conn)
		switch n {
		case 1:
			got <- next(t, ctx, conn)
			c.Report(runnerSession("s-1", wire.SessionWorking, at.Add(time.Second)))
			got <- next(t, ctx, conn)
			conn.Close(websocket.StatusGoingAway, "hibernate")
		case 2:
			got <- next(t, ctx, conn)
			c.Report(runnerSession("s-1", wire.SessionDone, at.Add(2*time.Second)))
			got <- next(t, ctx, conn)
			conn.Close(websocket.StatusGoingAway, "hibernate")
		case 3:
			ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			if _, data, err := conn.Read(ctx); err == nil && string(data) != "ping" {
				got <- wire.Unknown{Kind: string(data)}
			}
			<-ctx.Done()
		}
	})
	c = client(t, h, &fakeRuntimes{}, fakeAllow{})
	c.Report(runnerSession("s-1", wire.SessionQueued, at))
	stop := start(t, c)
	var states []wire.RunnerSessionState
	for range 4 {
		select {
		case m := <-got:
			s, ok := m.(wire.Session)
			if !ok || s.Session.ID != "s-1" {
				t.Fatalf("got %#v", m)
			}
			states = append(states, s.Session.State)
		case <-time.After(5 * time.Second):
			t.Fatal("no session frame")
		}
	}
	for h.count() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	stop()
	want := []wire.RunnerSessionState{wire.SessionQueued, wire.SessionWorking, wire.SessionWorking, wire.SessionDone}
	if !slices.Equal(states, want) {
		t.Fatalf("states %v, want %v", states, want)
	}
	select {
	case m := <-got:
		t.Fatalf("a sent done session went out again: %#v", m)
	default:
	}
}
