package group

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// node is one daemon in the test process. net acts as its Tailscale listener and applies the access mode of each
// route like server.Server does. loop acts as its loopback listener.
type node struct {
	name   string
	store  *Store
	joins  *Joins
	syncer *Syncer
	frames frames
	loop   *httptest.Server
	net    *httptest.Server
}

func newNode(t *testing.T, name string, clock *fakeClock) *node {
	t.Helper()
	n := &node{name: name}
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	n.loop = httptest.NewServer(loopMux)
	n.net = httptest.NewServer(netMux)
	t.Cleanup(func() { n.loop.Close(); n.net.Close() })
	port := n.net.Listener.Addr().(*net.TCPAddr).Port

	n.store = NewStore(filepath.Join(t.TempDir(), "group.json"), newSecrets(), clock.Now,
		func() { n.frames.add(map[string]any{"type": "group"}) })
	self := wire.GroupMember{Machine: name, Host: "127.0.0.1", Addrs: []string{"127.0.0.1"}, Port: port, Roles: []string{"serve"}}
	if err := n.store.EnsureCreated(self); err != nil {
		t.Fatal(err)
	}
	n.joins = NewJoins(n.store, n.frames.add, clock.Now)
	n.joins.pollEvery = 10 * time.Millisecond
	n.syncer = NewSyncer(n.store, nil, slog.New(slog.DiscardHandler))

	routes := append(n.joins.Routes(), n.syncer.Routes()...)
	routes = append(routes, server.Route{Pattern: "GET /v1/whoami", Access: server.Public, Handle: func(w http.ResponseWriter, r *http.Request) {
		id, home := n.store.Info()
		writeJSON(w, http.StatusOK, wire.DaemonInfo{Machine: name, Group: id, Home: home})
	}})
	for _, r := range routes {
		loopMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		if r.Access == server.LoopbackOnly {
			continue
		}
		netMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			token, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			c, ok := n.store.Verify(token)
			if !ok && r.Access == server.Member {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if ok {
				req = req.WithContext(server.WithCaller(req.Context(), c))
			}
			r.Handle(w, req)
		})
	}
	return n
}

// addr is the host:port of the Tailscale listener.
func (n *node) addr() string { return strings.TrimPrefix(n.net.URL, "http://") }

// local calls a loopback route, like the CLI does.
func (n *node) local(t *testing.T, method, path string, body, out any) {
	t.Helper()
	if _, err := (peerClient{http.DefaultClient}).do(t.Context(), method, n.loop.URL+path, "", body, out); err != nil {
		t.Fatalf("%s %s %s: %v", n.name, method, path, err)
	}
}

func (n *node) group(t *testing.T) wire.Group {
	t.Helper()
	g, ok := n.store.Snapshot()
	if !ok {
		t.Fatal(n.name, "has no group")
	}
	return g
}

func rounds(ctx context.Context, k int, nodes ...*node) {
	for range k {
		for _, n := range nodes {
			n.syncer.round(ctx)
		}
	}
}

func converged(t *testing.T, nodes ...*node) {
	t.Helper()
	first := nodes[0].group(t)
	for _, n := range nodes[1:] {
		g := n.group(t)
		if g.ID != first.ID || g.Version != first.Version || !sameContent(g, first) {
			t.Fatalf("%s v%d %s differs from %s v%d %s", n.name, g.Version, machines(g), nodes[0].name, first.Version, machines(first))
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timeout:", what)
}

func TestThreeDaemons(t *testing.T) {
	clock := newClock()
	a, b, c := newNode(t, "a", clock), newNode(t, "b", clock), newNode(t, "c", clock)
	ctx := t.Context()
	groupID := a.group(t).ID

	// B joins by code.
	var inv wire.GroupInvite
	a.local(t, http.MethodPost, "/v1/group/invites", map[string]any{}, &inv)
	if inv.Via != a.addr() {
		t.Fatal(inv.Via)
	}
	clock.Add(time.Second)
	var g wire.Group
	b.local(t, http.MethodPost, "/v1/group/enter", map[string]string{"host": inv.Via, "code": inv.Code}, &g)
	if g.ID != groupID || len(g.Members) != 2 {
		t.Fatalf("%+v", g)
	}

	// C joins by request through B.
	clock.Add(time.Second)
	var p wire.JoinPending
	c.local(t, http.MethodPost, "/v1/group/ask", map[string]string{"host": b.addr()}, &p)
	fr := b.frames.last("joinRequest")
	if fr == nil || fr["confirm"] != p.Confirm || fr["machine"] != "c" {
		t.Fatalf("%+v", fr)
	}
	var pending []Pending
	b.local(t, http.MethodGet, "/v1/group/requests", nil, &pending)
	if len(pending) != 1 || pending[0].Request != p.Request {
		t.Fatalf("%+v", pending)
	}
	b.local(t, http.MethodPost, "/v1/group/requests/"+p.Request+"/approve", map[string]any{}, nil)
	waitFor(t, "c adopts the group", func() bool { id, _ := c.store.Info(); return id == groupID })
	waitFor(t, "joinResult frame", func() bool { f := c.frames.last("joinResult"); return f != nil && f["status"] == StatusApproved })

	// All three converge within two sync rounds.
	rounds(ctx, 2, a, b, c)
	converged(t, a, b, c)
	if g := a.group(t); machines(g) != "[a b c]" || len(g.Devices) != 3 {
		t.Fatalf("%+v", g)
	}

	// An app on A gets a credential through loopback, and reads the roster on B with it.
	var cred wire.DeviceCredential
	a.local(t, http.MethodPost, "/v1/group/devices", map[string]string{"name": "OverSire on a", "kind": "app"}, &cred)
	rounds(ctx, 1, b)
	if _, err := (peerClient{http.DefaultClient}).do(ctx, http.MethodGet, b.net.URL+"/v1/group", cred.Token, nil, &g); err != nil {
		t.Fatal("app credential on b:", err)
	}
	// A loopback-only route is not on the Tailscale listener.
	if code, _ := (peerClient{http.DefaultClient}).do(ctx, http.MethodPost, b.net.URL+"/v1/group/invites", cred.Token, map[string]any{}, nil); code != http.StatusNotFound {
		t.Fatal(code)
	}
	// An app cannot poke.
	if code, _ := (peerClient{http.DefaultClient}).do(ctx, http.MethodPost, b.net.URL+"/v1/group/poke", cred.Token, map[string]any{}, nil); code != http.StatusForbidden {
		t.Fatal(code)
	}

	// Move home to B.
	b.local(t, http.MethodPost, "/v1/group/home", map[string]string{"machine": "b"}, nil)
	rounds(ctx, 2, a, b, c)
	converged(t, a, b, c)
	if a.group(t).Home != "b" {
		t.Fatal(a.group(t).Home)
	}

	// Rotate on A. The members get the new secret, and the sync works after the grace time.
	a.local(t, http.MethodPost, "/v1/group/rotate", map[string]any{}, nil)
	rounds(ctx, 1, a, b, c)
	clock.Add(RotateGrace + time.Minute)
	rounds(ctx, 2, a, b, c)
	converged(t, a, b, c)
	if id, _ := c.store.Info(); id != groupID {
		t.Fatal("c reset after the rotate")
	}
	if _, ok := b.store.Verify(cred.Token); ok {
		t.Fatal("the old app credential is valid after the rotate")
	}

	// A removes C. C resets itself.
	clock.Add(time.Minute)
	cDevices := []string{}
	for _, d := range a.group(t).Devices {
		if d.Machine == "c" {
			cDevices = append(cDevices, d.DeviceID)
		}
	}
	a.local(t, http.MethodDelete, "/v1/group/members/c", nil, nil)
	select {
	case id := <-a.store.Revoked():
		if id != cDevices[0] {
			t.Fatal(id)
		}
	default:
		t.Fatal("no revoked id")
	}
	rounds(ctx, 2, a, b, c)
	converged(t, a, b)
	if g := a.group(t); machines(g) != "[a b]" || len(g.Removed) != 1 {
		t.Fatalf("%+v", g)
	}
	cg := c.group(t)
	if cg.ID == groupID || machines(cg) != "[c]" || cg.Home != "c" {
		t.Fatalf("c did not reset: %+v", cg)
	}
}

// Join the other way: the machine without a group shows a code, and a member admits it.
func TestRequestCodeAndAdmit(t *testing.T) {
	clock := newClock()
	m, n := newNode(t, "m", clock), newNode(t, "n", clock)
	var code wire.GroupInvite
	n.local(t, http.MethodPost, "/v1/group/offers", map[string]any{}, &code)
	clock.Add(time.Second)
	m.local(t, http.MethodPost, "/v1/group/admit", map[string]string{"host": n.addr(), "code": code.Code}, nil)
	if n.group(t).ID != m.group(t).ID {
		t.Fatal("n did not join")
	}
	rounds(t.Context(), 1, m, n)
	converged(t, m, n)
	// The code is used.
	if _, err := (peerClient{http.DefaultClient}).do(t.Context(), http.MethodPost, n.net.URL+"/v1/group/offer", "",
		wire.GroupInvite{Code: code.Code, Via: m.addr()}, nil); err == nil {
		t.Fatal("code used twice")
	}
}

func TestDenyOverHTTP(t *testing.T) {
	clock := newClock()
	a, b := newNode(t, "a", clock), newNode(t, "b", clock)
	var p wire.JoinPending
	b.local(t, http.MethodPost, "/v1/group/ask", map[string]string{"host": a.addr()}, &p)
	a.local(t, http.MethodPost, "/v1/group/requests/"+p.Request+"/deny", map[string]any{}, nil)
	waitFor(t, "denied frame", func() bool { f := b.frames.last("joinResult"); return f != nil && f["status"] == StatusDenied })
	if b.group(t).ID == a.group(t).ID {
		t.Fatal("b joined")
	}
}

func TestDiscover(t *testing.T) {
	clock := newClock()
	a, b := newNode(t, "a", clock), newNode(t, "b", clock)
	host, port, _ := net.SplitHostPort(b.addr())
	pn, _ := strconv.Atoi(port)
	// Discover uses the port of this machine, so give a the port of b.
	self := a.store.Self()
	self.Port = pn
	_ = a.store.EnsureCreated(self)
	peers := []server.Peer{
		{Name: "b", Addrs: []netip.Addr{netip.MustParseAddr(host)}, Online: true},
		{Name: "off", Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.99")}, Online: false},
	}
	got := a.syncer.Discover(t.Context(), peers)
	if len(got) != 1 || got[0].Machine != "b" || got[0].Group == "" {
		t.Fatalf("%+v", got)
	}
}

func TestRouteTable(t *testing.T) {
	clock := newClock()
	n := newNode(t, "a", clock)
	want := map[string]server.Access{
		"GET /v1/group":                        server.Member,
		"POST /v1/group/poke":                  server.Member,
		"POST /v1/group/rekey":                 server.Member,
		"POST /v1/group/renew":                 server.Member,
		"POST /v1/group/devices":               server.LoopbackOnly,
		"POST /v1/group/invites":               server.LoopbackOnly,
		"POST /v1/group/offers":                server.LoopbackOnly,
		"POST /v1/group/requests":              server.Public,
		"GET /v1/group/requests/{id}":          server.Public,
		"GET /v1/group/requests":               server.LoopbackOnly,
		"POST /v1/group/requests/{id}/approve": server.LoopbackOnly,
		"POST /v1/group/requests/{id}/deny":    server.LoopbackOnly,
		"POST /v1/group/join":                  server.Public,
		"POST /v1/group/offer":                 server.Public,
		"POST /v1/group/enter":                 server.LoopbackOnly,
		"POST /v1/group/ask":                   server.LoopbackOnly,
		"POST /v1/group/admit":                 server.LoopbackOnly,
		"DELETE /v1/group/members/{machine}":   server.LoopbackOnly,
		"DELETE /v1/group/devices/{id}":        server.LoopbackOnly,
		"POST /v1/group/home":                  server.LoopbackOnly,
		"POST /v1/group/rotate":                server.LoopbackOnly,
		"GET /v1/group/peers":                  server.LoopbackOnly,
	}
	routes := append(n.joins.Routes(), n.syncer.Routes()...)
	if len(routes) != len(want) {
		t.Fatalf("%d routes, want %d", len(routes), len(want))
	}
	for _, r := range routes {
		a, ok := want[r.Pattern]
		if !ok || a != r.Access || r.Handle == nil {
			t.Fatalf("%s: access %d", r.Pattern, r.Access)
		}
	}
}

// Run pokes the members after a local change, and a poked member pulls at once.
func TestRunPokes(t *testing.T) {
	clock := newClock()
	a, b := newNode(t, "a", clock), newNode(t, "b", clock)
	var inv wire.GroupInvite
	a.local(t, http.MethodPost, "/v1/group/invites", map[string]any{}, &inv)
	b.local(t, http.MethodPost, "/v1/group/enter", map[string]string{"host": inv.Via, "code": inv.Code}, nil)
	a.syncer.every, b.syncer.every = time.Hour, time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go a.syncer.Run(ctx)
	go b.syncer.Run(ctx)
	waitFor(t, "first round", func() bool { return len(a.group(t).Members) == 2 })
	a.local(t, http.MethodPost, "/v1/group/home", map[string]string{"machine": "b"}, nil)
	waitFor(t, "b pulls after the poke", func() bool { return b.group(t).Home == "b" })
}
