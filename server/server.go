package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/wire"
)

const (
	rebindEvery  = 30 * time.Second
	socketIdle   = 90 * time.Second // the app pings each 30 s
	maxHookBody  = 2 << 20
	maxSmallBody = 64 << 10
	pageLimit    = 500
)

// CloseRevoked is the WebSocket close code for a revoked credential. CloudSource reads it as unauthorized.
const CloseRevoked websocket.StatusCode = 4001

// HealthSources tells the state of each source of the daemon, for example "live", "off" or "failed: <reason>".
type HealthSources struct {
	Hooks string `json:"hooks"`
	Orca  string `json:"orca"`
	Hub   string `json:"hub"`
}

// Health is the answer of GET /v1/health. Uptime is in seconds.
type Health struct {
	OK      bool          `json:"ok"`
	Uptime  int64         `json:"uptime"`
	Sources HealthSources `json:"sources"`
}

// HookCall is the body of POST /v1/hooks/{harness}, and one line of the hook spool.
type HookCall struct {
	Env     wire.HookEnv    `json:"env"`
	Payload json.RawMessage `json:"payload"`
}

type addrSource interface {
	Addrs(ctx context.Context) ([]netip.Addr, error)
}

// Server serves the local-hub API on loopback (no credential) and on each tailnet address (group credential).
type Server struct {
	Log    *slog.Logger
	Health func() HealthSources // nil: each source is "off"

	cfg      config.ServeConfig
	info     func() wire.DaemonInfo
	verifier Verifier
	events   Events
	sessions Sessions
	hooks    HookSink
	tailnet  addrSource
	now      func() time.Time
	started  time.Time

	loopMux *http.ServeMux
	netMux  *http.ServeMux

	mu      sync.Mutex
	routes  []Route
	port    int
	nets    map[netip.Addr]*http.Server
	clients map[*client]struct{}
	acks    map[string]int64
}

// NewServer makes the server with the built-in routes. v is nil before the machine has a group:
// then the Tailscale listener serves only the Public routes.
func NewServer(cfg config.ServeConfig, info func() wire.DaemonInfo, v Verifier, events Events, sessions Sessions, hooks HookSink) *Server {
	s := &Server{
		Log: slog.Default(), cfg: cfg, info: info, verifier: v, events: events, sessions: sessions, hooks: hooks,
		tailnet: NewTailnet(proc.Local{}), now: time.Now,
		loopMux: http.NewServeMux(), netMux: http.NewServeMux(),
		nets: map[netip.Addr]*http.Server{}, clients: map[*client]struct{}{}, acks: map[string]int64{},
	}
	s.started = s.now()
	s.Handle(
		Route{"GET /v1/whoami", Public, s.whoami},
		Route{"GET /v1/health", Member, s.health},
		Route{"GET /v1/connect", Member, s.connect},
		Route{"GET /v1/events", Member, s.eventsPage},
		Route{"POST /v1/ack", Member, s.ack},
		Route{"GET /v1/sessions", Member, s.sessionList},
		Route{"GET /v1/sessions/{id}", Member, s.session},
		Route{"POST /v1/hooks/{harness}", LoopbackOnly, s.hook},
	)
	return s
}

// Handle adds routes. A LoopbackOnly route is not on the Tailscale listener, so it gets 404 there.
func (s *Server) Handle(routes ...Route) {
	s.mu.Lock()
	s.routes = append(s.routes, routes...)
	s.mu.Unlock()
	for _, r := range routes {
		s.loopMux.Handle(r.Pattern, s.onLoopback(r))
		if r.Access != LoopbackOnly {
			s.netMux.Handle(r.Pattern, s.onTailnet(r))
		}
	}
}

// Notify sends msg as a JSON text frame to each WebSocket client. loopbackOnly skips the Tailscale clients.
func (s *Server) Notify(loopbackOnly bool, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.Log.Error("notify", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		if !loopbackOnly || c.caller.Loopback {
			c.send(b)
		}
	}
}

// Listen gives the bound addresses. Tailscale is the first IPv4 tailnet address, else the first IPv6 one.
func (s *Server) Listen() wire.DaemonListen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var l wire.DaemonListen
	if s.port == 0 {
		return l
	}
	l.Loopback = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	addrs := make([]netip.Addr, 0, len(s.nets))
	for a := range s.nets {
		addrs = append(addrs, a)
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int {
		if a.Is4() != b.Is4() {
			if a.Is4() {
				return -1
			}
			return 1
		}
		return a.Compare(b)
	})
	if len(addrs) > 0 {
		l.Tailscale = netip.AddrPortFrom(addrs[0], uint16(s.port)).String()
	}
	return l
}

// Cursor gives the last acked seq of a device. The loopback device is "loopback".
func (s *Server) Cursor(device string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.acks[device]
	return seq, ok
}

// Run binds 127.0.0.1 and [::1], and the tailnet addresses when Tailscale is on. It blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	ln4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.ListenPort())))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	port := ln4.Addr().(*net.TCPAddr).Port
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
	lns := []net.Listener{ln4}
	if ln6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		lns = append(lns, ln6)
	} else {
		s.Log.Warn("no IPv6 loopback listener", "err", err)
	}
	s.Log.Info("server listen", "port", port, "tailscale", s.cfg.UseTailscale())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	handler := s.guard(port, s.loopMux)
	errc := make(chan error, len(lns))
	var servers []*http.Server
	var wg sync.WaitGroup
	for _, ln := range lns {
		srv := newHTTPServer(handler)
		servers = append(servers, srv)
		wg.Go(func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		})
	}
	wg.Go(func() { s.pokeLoop(ctx) })
	if s.verifier != nil {
		wg.Go(func() { s.revokeLoop(ctx) })
	}
	if s.cfg.UseTailscale() {
		wg.Go(func() { s.tailnetLoop(ctx) })
	}

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	cancel()
	s.closeClients(websocket.StatusGoingAway, "daemon stop")
	stop, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	for _, srv := range servers {
		_ = srv.Shutdown(stop)
	}
	wg.Wait()
	s.rebind(nil)
	return err
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
}

func (s *Server) tailnetLoop(ctx context.Context) {
	t := time.NewTicker(rebindEvery)
	defer t.Stop()
	for {
		if addrs, err := s.tailnet.Addrs(ctx); err != nil {
			s.Log.Debug("tailnet addresses", "err", err)
		} else {
			s.rebind(addrs)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// rebind opens a listener for each new address and closes the listeners of the addresses that went away.
func (s *Server) rebind(addrs []netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[netip.Addr]bool{}
	for _, a := range addrs {
		if !a.IsValid() || a.IsUnspecified() {
			continue
		}
		want[a] = true
		if s.nets[a] != nil {
			continue
		}
		ln, err := net.Listen("tcp", netip.AddrPortFrom(a, uint16(s.port)).String())
		if err != nil {
			s.Log.Warn("tailnet listen", "addr", a, "err", err)
			continue
		}
		srv := newHTTPServer(s.netMux)
		s.nets[a] = srv
		s.Log.Info("tailnet listen", "addr", ln.Addr().String())
		go srv.Serve(ln)
	}
	for a, srv := range s.nets {
		if !want[a] {
			_ = srv.Close()
			delete(s.nets, a)
			s.Log.Info("tailnet close", "addr", a)
		}
	}
}

func (s *Server) pokeLoop(ctx context.Context) {
	pokes, cancel := s.events.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-pokes:
			if !ok {
				return
			}
			s.Notify(false, map[string]any{"type": "poke", "head": s.events.Page(0, 1).Head})
		}
	}
}

func (s *Server) revokeLoop(ctx context.Context) {
	revoked := s.verifier.Revoked()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-revoked:
			if !ok {
				return
			}
			s.mu.Lock()
			for c := range s.clients {
				if !c.caller.Loopback && c.caller.DeviceID == id {
					go c.conn.Close(CloseRevoked, "device revoked")
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) closeClients(code websocket.StatusCode, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		go c.conn.Close(code, reason)
	}
}

// guard stops calls from web pages on loopback: an Origin, a Host of another name (DNS rebinding), or a write that is not JSON.
func (s *Server) guard(port int, next http.Handler) http.Handler {
	p := strconv.Itoa(port)
	hosts := []string{"127.0.0.1:" + p, "[::1]:" + p, "localhost:" + p}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Origin") != "":
			writeError(w, http.StatusForbidden, "origin not allowed")
		case !slices.Contains(hosts, strings.ToLower(r.Host)):
			writeError(w, http.StatusForbidden, "host not allowed")
		case writes(r.Method) && !isJSON(r.Header.Get("Content-Type")):
			writeError(w, http.StatusForbidden, "content type must be application/json")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func writes(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func isJSON(contentType string) bool {
	t, _, err := mime.ParseMediaType(contentType)
	return err == nil && t == "application/json"
}

func (s *Server) onLoopback(r Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.Handle(w, req.WithContext(WithCaller(req.Context(), Caller{Loopback: true})))
	})
}

func (s *Server) onTailnet(r Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, ok := s.verify(req)
		if !ok && r.Access == Member {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if ok {
			req = req.WithContext(WithCaller(req.Context(), c))
		}
		r.Handle(w, req)
	})
}

func (s *Server) verify(r *http.Request) (Caller, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || s.verifier == nil {
		return Caller{}, false
	}
	c, ok := s.verifier.Verify(token)
	c.Loopback = false
	return c, ok
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	d := s.info()
	d.Listen = s.Listen()
	d.Device = ""
	if c, ok := CallerFrom(r.Context()); ok {
		d.Device = c.DeviceID
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	src := HealthSources{Hooks: "off", Orca: "off", Hub: "off"}
	if s.Health != nil {
		src = s.Health()
	}
	writeJSON(w, http.StatusOK, Health{OK: true, Uptime: int64(s.now().Sub(s.started).Seconds()), Sources: src})
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if sc := q.Get("schema"); sc != "" && sc != "1" {
		writeError(w, http.StatusBadRequest, "unknown schema "+sc)
		return
	}
	after, _ := strconv.Atoi(q.Get("after"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = pageLimit
	}
	writeJSON(w, http.StatusOK, s.events.Page(max(after, 0), min(max(limit, 1), pageLimit)))
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq *int64 `json:"seq"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmallBody)).Decode(&body); err != nil || body.Seq == nil || *body.Seq < 0 {
		writeError(w, http.StatusBadRequest, "bad seq")
		return
	}
	device := "loopback"
	if c, _ := CallerFrom(r.Context()); !c.Loopback {
		device = c.DeviceID
	}
	s.mu.Lock()
	s.acks[device] = max(s.acks[device], *body.Seq)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sessionList(w http.ResponseWriter, _ *http.Request) {
	all := s.sessions.All()
	if all == nil {
		all = []wire.DaemonSession{}
	}
	writeJSON(w, http.StatusOK, wire.DaemonSessionsPage{Sessions: all})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	ses, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no session")
		return
	}
	writeJSON(w, http.StatusOK, ses)
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	if s.hooks == nil {
		writeError(w, http.StatusServiceUnavailable, "hooks are off")
		return
	}
	var call HookCall
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHookBody)).Decode(&call); err != nil {
		writeError(w, http.StatusBadRequest, "bad hook call")
		return
	}
	if err := s.hooks.Ingest(r.Context(), r.PathValue("harness"), call.Env, call.Payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// connect is the WebSocket of the app. It speaks the frames of the cloud hub: "ping" gets "pong", and JSON frames
// {"type":"poke"} and {"type":"store"} tell the client to read again.
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	caller, _ := CallerFrom(r.Context())
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &client{conn: conn, caller: caller, out: make(chan []byte, 16)}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		conn.CloseNow()
	}()
	go c.writeLoop(ctx, cancel)
	for {
		rctx, done := context.WithTimeout(ctx, socketIdle)
		typ, data, err := conn.Read(rctx)
		done()
		if err != nil {
			return
		}
		if typ == websocket.MessageText && string(data) == "ping" {
			c.send([]byte("pong"))
		}
	}
}

// client is one WebSocket connection.
type client struct {
	conn   *websocket.Conn
	caller Caller
	out    chan []byte
}

// send drops the frame when the buffer is full: a poke in the buffer already tells the client to read.
func (c *client) send(b []byte) {
	select {
	case c.out <- b:
	default:
	}
}

func (c *client) writeLoop(ctx context.Context, cancel func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c.out:
			wctx, done := context.WithTimeout(ctx, 5*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, b)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
