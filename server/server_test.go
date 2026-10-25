package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/wire"
)

type fakeSessions []wire.DaemonSession

func (f fakeSessions) All() []wire.DaemonSession { return f }
func (f fakeSessions) Get(id string) (wire.DaemonSession, bool) {
	for _, s := range f {
		if s.ID == id {
			return s, true
		}
	}
	return wire.DaemonSession{}, false
}

type fakeVerifier struct {
	callers map[string]Caller
	revoked chan string
}

func (f *fakeVerifier) Verify(token string) (Caller, bool) {
	c, ok := f.callers[token]
	return c, ok
}
func (f *fakeVerifier) Revoked() <-chan string { return f.revoked }

type fakeHooks struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeHooks) Ingest(_ context.Context, harness string, env wire.HookEnv, payload []byte) error {
	if harness == "bad" {
		return errors.New("unknown harness")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, harness+" "+env.PaneKey+" "+string(payload))
	return nil
}

type fixture struct {
	s     *Server
	log   *eventlog.Log
	v     *fakeVerifier
	hooks *fakeHooks
	loop  *httptest.Server // loopback listener with the guard
	net   *httptest.Server // Tailscale listener
}

func newFixture(t *testing.T, withVerifier bool) *fixture {
	t.Helper()
	f := &fixture{log: eventlog.New(10), hooks: &fakeHooks{}}
	f.v = &fakeVerifier{callers: map[string]Caller{"good": {DeviceID: "D1", Name: "mba app", Kind: "app", Machine: "mba"}}, revoked: make(chan string, 1)}
	var v Verifier
	if withVerifier {
		v = f.v
	}
	info := func() wire.DaemonInfo {
		return wire.DaemonInfo{Machine: "mbp", Version: "test", Contract: 1, Roles: []string{"serve"}, Group: "g1", Home: "mbp"}
	}
	ses := fakeSessions{{ID: "s1", Machine: "mbp", Runtime: "cli", Harness: "claude", State: wire.PhaseWorking}}
	f.s = NewServer(config.ServeConfig{}, info, v, f.log, ses, f.hooks)
	f.s.Log = discardLog()
	var guarded http.Handler
	f.loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded.ServeHTTP(w, r) }))
	port := f.loop.Listener.Addr().(*net.TCPAddr).Port
	f.s.port = port
	guarded = f.s.accessLog("loopback", f.s.guard(port, f.s.loopMux))
	f.net = httptest.NewServer(f.s.accessLog("tailnet", f.s.netMux))
	t.Cleanup(func() {
		f.s.closeClients(websocket.StatusGoingAway, "end")
		f.loop.Close()
		f.net.Close()
	})
	return f
}

func call(t *testing.T, method, url string, body string, header ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i] == "Host" {
			req.Host = header[i+1]
		} else {
			req.Header.Set(header[i], header[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

const jsonCT = "application/json"

func TestRouteTable(t *testing.T) {
	f := newFixture(t, true)
	want := map[string]Access{
		"GET /v1/whoami": Public, "GET /v1/health": Member, "GET /v1/connect": Member, "GET /v1/events": Member,
		"POST /v1/ack": Member, "GET /v1/sessions": Member, "GET /v1/sessions/{id}": Member,
		"POST /v1/hooks/{harness}": LoopbackOnly,
	}
	if len(f.s.routes) != len(want) {
		t.Fatalf("%d routes", len(f.s.routes))
	}
	for _, r := range f.s.routes {
		a, ok := want[r.Pattern]
		if !ok || a != r.Access || r.Handle == nil {
			t.Errorf("route %q access %d", r.Pattern, r.Access)
		}
		if r.Access != Member && r.Access != Public && r.Access != LoopbackOnly {
			t.Errorf("route %q has no access mode", r.Pattern)
		}
		if r.Access == LoopbackOnly {
			method, path, _ := strings.Cut(r.Pattern, " ")
			path = strings.ReplaceAll(path, "{harness}", "claude")
			if code, _ := call(t, method, f.net.URL+path, "{}", "Authorization", "Bearer good", "Content-Type", jsonCT); code != http.StatusNotFound {
				t.Errorf("%s on Tailscale: %d", r.Pattern, code)
			}
		}
	}
	// Routes of other packages follow the same table.
	f.s.Handle(Route{"GET /v1/questions", Member, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, struct{}{}) }},
		Route{"POST /v1/group/approve", LoopbackOnly, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, struct{}{}) }})
	if code, _ := call(t, "GET", f.net.URL+"/v1/questions", "", "Authorization", "Bearer good"); code != 200 {
		t.Fatal(code)
	}
	if code, _ := call(t, "POST", f.net.URL+"/v1/group/approve", "{}", "Authorization", "Bearer good", "Content-Type", jsonCT); code != 404 {
		t.Fatal(code)
	}
	if code, _ := call(t, "POST", f.loop.URL+"/v1/group/approve", "{}", "Content-Type", jsonCT); code != 200 {
		t.Fatal(code)
	}
	if code, _ := call(t, "GET", f.loop.URL+"/v1/store/ops", ""); code != 404 {
		t.Fatal("store ops before the store item:", code)
	}
}

func TestLoopbackGuard(t *testing.T) {
	f := newFixture(t, true)
	port := strconv.Itoa(f.s.port)
	cases := []struct {
		name, method, path, body string
		header                   []string
		code                     int
	}{
		{"plain", "GET", "/v1/whoami", "", nil, 200},
		{"localhost", "GET", "/v1/whoami", "", []string{"Host", "localhost:" + port}, 200},
		{"ipv6 host", "GET", "/v1/whoami", "", []string{"Host", "[::1]:" + port}, 200},
		{"origin", "GET", "/v1/whoami", "", []string{"Origin", "https://x"}, 403},
		{"rebinding host", "GET", "/v1/whoami", "", []string{"Host", "evil.example:" + port}, 403},
		{"other port", "GET", "/v1/whoami", "", []string{"Host", "127.0.0.1:1"}, 403},
		{"post text", "POST", "/v1/ack", `{"seq":1}`, []string{"Content-Type", "text/plain"}, 403},
		{"post form", "POST", "/v1/ack", `{"seq":1}`, []string{"Content-Type", "application/x-www-form-urlencoded"}, 403},
		{"post no type", "POST", "/v1/ack", `{"seq":1}`, nil, 403},
		{"post json", "POST", "/v1/ack", `{"seq":1}`, []string{"Content-Type", "application/json; charset=utf-8"}, 200},
		{"origin on 404", "GET", "/nothing", "", []string{"Origin", "https://x"}, 403},
	}
	for _, c := range cases {
		if code, b := call(t, c.method, f.loop.URL+c.path, c.body, c.header...); code != c.code {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
}

func TestTailnetAuth(t *testing.T) {
	f := newFixture(t, true)
	for _, h := range [][]string{nil, {"Authorization", "Bearer wrong"}, {"Authorization", "good"}, {"Authorization", "Bearer "}} {
		if code, _ := call(t, "GET", f.net.URL+"/v1/sessions", "", h...); code != 401 {
			t.Errorf("%v: %d", h, code)
		}
	}
	if code, _ := call(t, "GET", f.net.URL+"/v1/sessions", "", "Authorization", "Bearer good"); code != 200 {
		t.Fatal(code)
	}
	// Revoked: the verifier no longer knows the token.
	delete(f.v.callers, "good")
	if code, _ := call(t, "GET", f.net.URL+"/v1/sessions", "", "Authorization", "Bearer good"); code != 401 {
		t.Fatal("revoked:", code)
	}
	f.v.callers["good"] = Caller{DeviceID: "D1", Loopback: true}
	var d wire.DaemonInfo
	_, b := call(t, "GET", f.net.URL+"/v1/whoami", "", "Authorization", "Bearer good")
	json.Unmarshal(b, &d)
	if d.Device != "D1" || d.Machine != "mbp" {
		t.Fatalf("%s", b)
	}
	_, b = call(t, "GET", f.net.URL+"/v1/whoami", "")
	d = wire.DaemonInfo{}
	json.Unmarshal(b, &d)
	if d.Device != "" {
		t.Fatalf("whoami without a credential: %s", b)
	}
}

func TestNoGroup(t *testing.T) {
	f := newFixture(t, false)
	if code, _ := call(t, "GET", f.net.URL+"/v1/whoami", ""); code != 200 {
		t.Fatal(code)
	}
	for _, p := range []string{"/v1/sessions", "/v1/events", "/v1/health"} {
		if code, _ := call(t, "GET", f.net.URL+p, "", "Authorization", "Bearer good"); code != 401 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := call(t, "GET", f.loop.URL+"/v1/sessions", ""); code != 200 {
		t.Fatal("loopback:", code)
	}
}

func TestRoutes(t *testing.T) {
	f := newFixture(t, true)
	f.log.Append(wire.ProviderEvent{ID: "e1", Schema: 1})
	f.log.Append(wire.ProviderEvent{ID: "e2", Schema: 1})

	var info wire.DaemonInfo
	_, b := call(t, "GET", f.loop.URL+"/v1/whoami", "")
	json.Unmarshal(b, &info)
	if info.Machine != "mbp" || info.Group != "g1" || info.Listen.Loopback != "127.0.0.1:"+strconv.Itoa(f.s.port) || info.Device != "" {
		t.Fatalf("whoami %s", b)
	}

	var page wire.EventsPage
	_, b = call(t, "GET", f.loop.URL+"/v1/events?after=1&limit=10&schema=1", "")
	json.Unmarshal(b, &page)
	if len(page.Events) != 1 || page.Events[0].ID != "e2" || page.Head != 2 || page.Next != 2 {
		t.Fatalf("events %s", b)
	}
	if code, _ := call(t, "GET", f.loop.URL+"/v1/events?schema=9", ""); code != 400 {
		t.Fatal("schema 9:", code)
	}
	_, b = call(t, "GET", f.loop.URL+"/v1/events?limit=1", "")
	json.Unmarshal(b, &page)
	if len(page.Events) != 1 || !page.More {
		t.Fatalf("limit %s", b)
	}

	if code, _ := call(t, "POST", f.net.URL+"/v1/ack", `{"seq":2}`, "Authorization", "Bearer good"); code != 200 {
		t.Fatal(code)
	}
	call(t, "POST", f.net.URL+"/v1/ack", `{"seq":1}`, "Authorization", "Bearer good")
	if seq, ok := f.s.Cursor("D1"); !ok || seq != 2 {
		t.Fatal("cursor", seq, ok)
	}
	if code, _ := call(t, "POST", f.loop.URL+"/v1/ack", `{"seq":-1}`, "Content-Type", jsonCT); code != 400 {
		t.Fatal(code)
	}
	if code, _ := call(t, "POST", f.loop.URL+"/v1/ack", `{}`, "Content-Type", jsonCT); code != 400 {
		t.Fatal(code)
	}

	_, b = call(t, "GET", f.loop.URL+"/v1/sessions", "")
	var sp wire.DaemonSessionsPage
	json.Unmarshal(b, &sp)
	if len(sp.Sessions) != 1 || sp.Sessions[0].ID != "s1" {
		t.Fatalf("sessions %s", b)
	}
	if code, _ := call(t, "GET", f.loop.URL+"/v1/sessions/s1", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := call(t, "GET", f.loop.URL+"/v1/sessions/nope", ""); code != 404 {
		t.Fatal(code)
	}

	f.s.Health = func() HealthSources { return HealthSources{Hooks: "live", Orca: "off", Hub: "failed: refused"} }
	f.s.started = time.Now().Add(-90 * time.Second)
	var h Health
	_, b = call(t, "GET", f.loop.URL+"/v1/health", "")
	json.Unmarshal(b, &h)
	if !h.OK || h.Uptime < 89 || h.Sources.Hub != "failed: refused" || h.Sources.Hooks != "live" {
		t.Fatalf("health %s", b)
	}

	code, b := call(t, "POST", f.loop.URL+"/v1/hooks/claude", `{"env":{"paneKey":"p1"},"payload":{"hook_event_name":"Stop"}}`, "Content-Type", jsonCT)
	if code != 200 || strings.TrimSpace(string(b)) != "{}" || len(f.hooks.calls) != 1 || f.hooks.calls[0] != `claude p1 {"hook_event_name":"Stop"}` {
		t.Fatalf("hook %d %s %v", code, b, f.hooks.calls)
	}
	if code, _ := call(t, "POST", f.loop.URL+"/v1/hooks/bad", `{"payload":{}}`, "Content-Type", jsonCT); code != 400 {
		t.Fatal(code)
	}
	if code, _ := call(t, "POST", f.loop.URL+"/v1/hooks/claude", `{`, "Content-Type", jsonCT); code != 400 {
		t.Fatal(code)
	}
}

func dial(t *testing.T, base string, header http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v1/connect", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func read(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitClients waits until the server knows n sockets, so that a frame does not go out before the accept.
func waitClients(t *testing.T, s *Server, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		got := len(s.clients)
		s.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("not %d clients", n)
}

func TestWebSocket(t *testing.T) {
	f := newFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.s.pokeLoop(ctx)
	go f.s.revokeLoop(ctx)

	local := dial(t, f.loop.URL, nil)
	remote := dial(t, f.net.URL, http.Header{"Authorization": {"Bearer good"}})
	waitClients(t, f.s, 2)

	if err := local.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if m := read(t, local); m != "pong" {
		t.Fatal(m)
	}

	f.log.Append(wire.ProviderEvent{ID: "e1", Schema: 1})
	for _, c := range []*websocket.Conn{local, remote} {
		if m := read(t, c); m != `{"head":1,"type":"poke"}` {
			t.Fatal(m)
		}
	}

	f.s.Notify(true, map[string]string{"type": "joinRequest"})
	f.s.Notify(false, map[string]string{"type": "store"})
	if m := read(t, local); m != `{"type":"joinRequest"}` {
		t.Fatal(m)
	}
	if m := read(t, local); m != `{"type":"store"}` {
		t.Fatal(m)
	}
	if m := read(t, remote); m != `{"type":"store"}` {
		t.Fatal("remote got a loopback-only frame:", m)
	}

	f.v.revoked <- "D1"
	rctx, rdone := context.WithTimeout(ctx, 2*time.Second)
	defer rdone()
	_, _, err := remote.Read(rctx)
	if websocket.CloseStatus(err) != CloseRevoked {
		t.Fatal("revoke:", err)
	}
	waitClients(t, f.s, 1)
}

func TestWebSocketAuth(t *testing.T) {
	f := newFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.net.URL, "http")+"/v1/connect", nil)
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatal(err, resp)
	}
	_, resp, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.loop.URL, "http")+"/v1/connect",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://x"}}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatal(err, resp)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestRebind(t *testing.T) {
	f := newFixture(t, true)
	f.s.port = freePort(t)
	a := netip.MustParseAddr("127.0.0.1")
	f.s.rebind([]netip.Addr{a, netip.IPv4Unspecified()})
	defer f.s.rebind(nil)
	if len(f.s.nets) != 1 {
		t.Fatalf("listeners %v", f.s.nets)
	}
	addr := f.s.Listen().Tailscale
	if addr != "127.0.0.1:"+strconv.Itoa(f.s.port) {
		t.Fatal(addr)
	}
	if code, _ := call(t, "GET", "http://"+addr+"/v1/sessions", ""); code != 401 {
		t.Fatal(code)
	}
	f.s.rebind([]netip.Addr{a})
	if len(f.s.nets) != 1 {
		t.Fatal("rebind made a second listener")
	}
	f.s.rebind(nil)
	if _, err := http.Get("http://" + addr + "/v1/whoami"); err == nil {
		t.Fatal("listener still open")
	}
	if f.s.Listen().Tailscale != "" {
		t.Fatal("tailscale address after close")
	}
}

type staticAddrs []netip.Addr

func (s staticAddrs) Addrs(context.Context) ([]netip.Addr, error) { return s, nil }

func TestRun(t *testing.T) {
	port := freePort(t)
	off := false
	s := NewServer(config.ServeConfig{Port: port, Tailscale: &off}, func() wire.DaemonInfo { return wire.DaemonInfo{Machine: "mbp"} },
		nil, eventlog.New(10), fakeSessions{}, nil)
	s.Log = discardLog()
	s.tailnet = staticAddrs{netip.MustParseAddr("127.0.0.1")} // unused: Tailscale is off
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	var code int
	for i := 0; i < 200 && code != 200; i++ {
		if resp, err := http.Get(base + "/v1/whoami"); err == nil {
			code = resp.StatusCode
			resp.Body.Close()
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if code != 200 {
		t.Fatal("no answer")
	}
	if s.Listen().Tailscale != "" {
		t.Fatal("tailnet listener with Tailscale off")
	}
	body, _ := json.Marshal(HookCall{Payload: json.RawMessage(`{}`)})
	resp, err := http.Post(base+"/v1/hooks/claude", jsonCT, bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("hooks off:", err, resp)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run does not stop")
	}
	if _, err := http.Get(base + "/v1/whoami"); err == nil {
		t.Fatal("loopback still open")
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestCallerTrusted(t *testing.T) {
	cases := []struct {
		name string
		c    Caller
		want bool
	}{
		{"loopback", Caller{Loopback: true}, true},
		{"app", Caller{DeviceID: "a-1", Kind: string(wire.DeviceApp), Machine: "mba"}, true},
		{"daemon", Caller{DeviceID: "d-1", Kind: string(wire.DeviceDaemon), Machine: "mba"}, false},
		{"no caller", Caller{}, false},
	}
	for _, c := range cases {
		if got := c.c.Trusted(); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if c, _ := CallerFrom(context.Background()); c.Trusted() {
		t.Error("a context with no caller is trusted")
	}
}
