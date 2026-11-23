package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeSockets struct {
	mu      sync.Mutex
	callers []server.Caller
	own     []netip.Addr
	sent    []sentFrame
}

type sentFrame struct {
	to    server.Caller
	frame wire.OpenRouteFrame
}

func (f *fakeSockets) DeliverTo(match func(server.Caller) bool, msg any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.callers {
		if match(c) {
			f.sent = append(f.sent, sentFrame{c, msg.(wire.OpenRouteFrame)})
			n++
		}
	}
	return n
}

func (f *fakeSockets) HasCaller(match func(server.Caller) bool) bool {
	for _, c := range f.callers {
		if match(c) {
			return true
		}
	}
	return false
}

func (f *fakeSockets) IsOwnAddr(a netip.Addr) bool {
	return a.IsLoopback() || len(f.own) > 0 && f.own[0] == a
}

type fakePeers struct {
	peers []server.Peer
	calls int
}

func (f *fakePeers) Peers(context.Context) ([]server.Peer, error) {
	f.calls++
	return f.peers, nil
}

type fakeGroup struct{ g wire.Group }

func (f fakeGroup) Snapshot() (wire.Group, bool) { return f.g, true }
func (f fakeGroup) Self() wire.GroupMember       { return wire.GroupMember{Machine: "mbp"} }

type fakeEvents struct{ rows []wire.EventDraft }

func (f *fakeEvents) AddFor(_ wire.DaemonSession, d wire.EventDraft) { f.rows = append(f.rows, d) }

var (
	mbaV4  = netip.MustParseAddr("100.70.1.2")
	mbaV6  = netip.MustParseAddr("fd7a:115c:a1e0::2")
	miniV4 = netip.MustParseAddr("100.70.1.9")
	mbaApp = server.Caller{DeviceID: "D-mba", Name: "OverSire on mba", Kind: string(wire.DeviceApp), Machine: "mba", Addr: mbaV4}
	mbpApp = server.Caller{Loopback: true}
)

type rig struct {
	o      *Opener
	s      *fakeSockets
	book   *Book
	events *fakeEvents
	peers  *fakePeers
	now    time.Time
}

// newRig has the daemon mbp, its own app (loopback) and the app of mba on the tailnet. mini is a Mac with no app open.
func newRig(callers ...server.Caller) *rig {
	r := &rig{now: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return r.now }
	r.s = &fakeSockets{callers: callers, own: []netip.Addr{netip.MustParseAddr("100.97.81.23")}}
	r.book = NewBook(clock)
	r.events = &fakeEvents{}
	r.peers = &fakePeers{peers: []server.Peer{
		{Name: "mba", Addrs: []netip.Addr{mbaV4, mbaV6}},
		{Name: "mini", Addrs: []netip.Addr{miniV4}},
	}}
	g := wire.Group{Name: "My machines", Members: []wire.GroupMember{{Machine: "mbp"}, {Machine: "mini"}},
		Devices: []wire.GroupDevice{
			{DeviceID: "D-mba", Kind: wire.DeviceApp, Machine: "mba"},
			{DeviceID: "D-mini", Kind: wire.DeviceApp, Machine: "mini"},
		}}
	r.o = NewOpener(r.s, r.book, r.peers, fakeGroup{g}, r.events, clock)
	r.o.Log = discard
	return r
}

// both reports mbp (idle 5 s) and mba (idle 300 s): mbp has the latest input.
func (r *rig) both() {
	r.book.Report(mbpApp, "mbp", 5*time.Second)
	r.book.Report(mbaApp, "mbp", 300*time.Second)
}

func (r *rig) open(t *testing.T, click netip.Addr, on string) (wire.OpenResult, error) {
	t.Helper()
	p, _ := ParsePath("item/OVERSIRE-400", "")
	return r.o.Open(context.Background(), p, click, on, wire.OpenWeb, "")
}

func TestBookReport(t *testing.T) {
	r := newRig()
	r.both()
	all := r.book.All()
	if len(all) != 2 || all[0].Machine != "mbp" || !all[0].Loopback || all[1].Machine != "mba" || all[1].Addr != mbaV4 {
		t.Fatalf("got %+v", all)
	}
	r.book.Report(mbaApp, "mbp", 0)
	if all = r.book.All(); all[0].Machine != "mba" || len(all) != 2 {
		t.Fatalf("a new report did not move mba first: %+v", all)
	}
	if !all[0].Match(mbaApp) || all[0].Match(mbpApp) || all[1].Match(mbaApp) {
		t.Fatal("match")
	}
	daemon := mbaApp
	daemon.Kind = string(wire.DeviceDaemon)
	if all[0].Match(daemon) {
		t.Fatal("a daemon matches an app presence")
	}
}

func TestOpenGoesToTheMacOfTheClick(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	res, err := r.open(t, mbaV4, "")
	if err != nil || res.Machine != "mba" || res.How != wire.OpenByClick || *res.Idle != 300 || res.Group != "My machines" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(r.s.sent) != 1 || r.s.sent[0].to.DeviceID != "D-mba" {
		t.Fatalf("sent %+v", r.s.sent)
	}
	f := r.s.sent[0].frame
	if f.Type != "openRoute" || f.Open.Path != "item/OVERSIRE-400" || f.Open.Source != wire.OpenWeb || f.Open.Daemon != "mbp" || f.Open.ID != res.ID {
		t.Fatalf("frame %+v", f)
	}
	if len(r.events.rows) != 1 {
		t.Fatalf("rows %+v", r.events.rows)
	}
	row := r.events.rows[0]
	var pl map[string]string
	_ = json.Unmarshal(row.Payload, &pl)
	if row.ID != "link:"+res.ID || row.Kind != wire.KindLinkOpened || row.Item != "OVERSIRE-400" || row.Machine != "mba" ||
		row.ActorType != wire.ActorHuman || row.Via != "web" || row.Summary != "Opened item/OVERSIRE-400 on mba" ||
		pl["route"] != "item/OVERSIRE-400" || pl["opened"] != "mba" || pl["how"] != "click" || pl["source"] != "web" {
		t.Fatalf("row %+v %s", row, row.Payload)
	}
}

func TestOpenMatchesTheOtherAddressOfThePeer(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	res, err := r.open(t, mbaV6, "")
	if err != nil || res.Machine != "mba" || res.How != wire.OpenByClick {
		t.Fatalf("got %+v, %v", res, err)
	}
	// The app reached the daemon on another address: the match is by the name of the peer.
	app := mbaApp
	app.Addr = netip.MustParseAddr("100.70.200.1")
	r = newRig(mbpApp, app)
	r.book.Report(mbpApp, "mbp", 5*time.Second)
	r.book.Report(app, "mbp", 300*time.Second)
	if res, err := r.open(t, mbaV4, ""); err != nil || res.Machine != "mba" || res.How != wire.OpenByClick {
		t.Fatalf("by name: %+v, %v", res, err)
	}
	if _, _ = r.open(t, mbaV4, ""); r.peers.calls != 1 {
		t.Fatalf("the peers are not kept: %d calls", r.peers.calls)
	}
}

func TestOpenFromLoopbackOrAnUnknownAddressGoesToTheLatestInput(t *testing.T) {
	for _, click := range []netip.Addr{{}, netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("100.97.81.23"), netip.MustParseAddr("100.70.77.7")} {
		r := newRig(mbpApp, mbaApp)
		r.both()
		res, err := r.open(t, click, "")
		if err != nil || res.Machine != "mbp" || res.How != wire.OpenByInput || *res.Idle != 5 {
			t.Fatalf("%v: got %+v, %v", click, res, err)
		}
		if r.events.rows[0].Machine != "" {
			t.Fatalf("%v: an input open sets the machine of the row", click)
		}
	}
}

func TestOpenSkipsAPresenceWithNoSocket(t *testing.T) {
	r := newRig(mbaApp)
	r.both()
	if res, err := r.open(t, netip.Addr{}, ""); err != nil || res.Machine != "mba" {
		t.Fatalf("got %+v, %v", res, err)
	}
	r = newRig()
	r.both()
	if _, err := r.open(t, netip.Addr{}, ""); !errors.Is(err, ErrNoApp) {
		t.Fatalf("got %v", err)
	}
}

func TestOpenOnAMacWithNoAppGivesNotRunning(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	var nr *NotRunningError
	if _, err := r.open(t, miniV4, ""); !errors.As(err, &nr) || nr.Machine != "mini" {
		t.Fatalf("got %v", err)
	}
	if len(r.s.sent) != 0 {
		t.Fatal("a frame went to another Mac")
	}
	if _, err := r.open(t, netip.Addr{}, "mini"); !errors.As(err, &nr) || nr.Machine != "mini" {
		t.Fatalf("on: got %v", err)
	}
}

func TestOpenOn(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	res, err := r.open(t, netip.Addr{}, "MBA")
	if err != nil || res.Machine != "mba" || res.How != wire.OpenOn {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, err := r.open(t, netip.Addr{}, "nowhere"); !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("unknown: %v", err)
	}
	// An app that is connected but sends no presence is too old.
	old := server.Caller{DeviceID: "D-mini", Kind: string(wire.DeviceApp), Machine: "mini"}
	r = newRig(mbpApp, old)
	r.both()
	var nu *NeedsUpdateError
	if _, err := r.open(t, netip.Addr{}, "mini"); !errors.As(err, &nu) {
		t.Fatalf("old app: %v", err)
	}
}

func TestOpenWithAnAgentWritesAnAgentRow(t *testing.T) {
	r := newRig(mbpApp)
	r.both()
	p, _ := ParsePath("item/OVERSIRE-400/changes", "OVERSIRE-402")
	if _, err := r.o.Open(context.Background(), p, netip.Addr{}, "", wire.OpenCLI, "claude s-1"); err != nil {
		t.Fatal(err)
	}
	row := r.events.rows[0]
	if row.ActorType != wire.ActorAgent || row.Actor != "claude s-1" || row.Via != "cli" {
		t.Fatalf("row %+v", row)
	}
	if f := r.s.sent[0].frame.Open; f.Focus != "OVERSIRE-402" || f.Agent != "claude s-1" || f.Source != wire.OpenCLI {
		t.Fatalf("frame %+v", f)
	}
}

// serve puts the routes on a loopback mux and on a Tailscale mux, as the server does. On Tailscale,
// "Bearer good" is the app of mba and "Bearer peer" is a daemon. A Public route needs no credential.
func serve(t *testing.T, o *Opener, remote netip.Addr) (loop, tailnet string) {
	t.Helper()
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	callers := map[string]server.Caller{
		"Bearer good": mbaApp,
		"Bearer peer": {DeviceID: "D2", Name: "mini", Kind: string(wire.DeviceDaemon), Machine: "mini"},
	}
	for _, r := range o.Routes() {
		loopMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		if r.Access == server.LoopbackOnly {
			continue
		}
		netMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			req.RemoteAddr = netip.AddrPortFrom(remote, 50000).String()
			c, ok := callers[req.Header.Get("Authorization")]
			if !ok && r.Access != server.Public {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if ok {
				req = req.WithContext(server.WithCaller(req.Context(), c))
			}
			r.Handle(w, req)
		})
	}
	l, n := httptest.NewServer(loopMux), httptest.NewServer(netMux)
	t.Cleanup(func() { l.Close(); n.Close() })
	return l.URL, n.URL
}

func send(t *testing.T, method, url, body string, header ...string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func TestGoPage(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	_, tn := serve(t, r.o, mbaV4)
	code, h, body := send(t, "GET", tn+"/go/item/OVERSIRE-400/changes?focus=OVERSIRE-402", "")
	if code != 200 || !strings.Contains(body, "Opened OVERSIRE-400 in OverSire on mba.") || !strings.Contains(body, "You can close this tab.") ||
		strings.Contains(body, "refresh") || !strings.Contains(body, `href="oversire://item/OVERSIRE-400/changes#OVERSIRE-402"`) {
		t.Fatalf("%d %s", code, body)
	}
	if h.Get("Cache-Control") != "no-store" || !strings.HasPrefix(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers %v", h)
	}
	if f := r.s.sent[0].frame.Open; f.Focus != "OVERSIRE-402" || f.Source != wire.OpenWeb {
		t.Fatalf("frame %+v", f)
	}
	// A reload in 3 s gives the same page and no second push.
	if code, _, body2 := send(t, "GET", tn+"/go/item/OVERSIRE-400/changes?focus=OVERSIRE-402", ""); code != 200 || body2 != body || len(r.s.sent) != 1 {
		t.Fatalf("reload: %d, %d pushes", code, len(r.s.sent))
	}
	r.now = r.now.Add(4 * time.Second)
	send(t, "GET", tn+"/go/item/OVERSIRE-400/changes?focus=OVERSIRE-402", "")
	if len(r.s.sent) != 2 {
		t.Fatalf("after the wait: %d pushes", len(r.s.sent))
	}
}

func TestGoPageStates(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	loop, tn := serve(t, r.o, miniV4)
	code, _, body := send(t, "GET", loop+"/go/item/OVERSIRE-400", "")
	if code != 200 || !strings.Contains(body, "on mbp, the Mac with the latest input.") ||
		!strings.Contains(body, `<meta http-equiv="refresh" content="0;url=oversire://item/OVERSIRE-400">`) {
		t.Fatalf("input: %d %s", code, body)
	}
	code, _, body = send(t, "GET", tn+"/go/item/OVERSIRE-400", "")
	if code != 503 || !strings.Contains(body, "OverSire on mini is not running.") || !strings.Contains(body, "refresh") {
		t.Fatalf("not running: %d %s", code, body)
	}
	code, _, body = send(t, "GET", loop+"/go/javascript:alert(1)", "")
	if code != 400 || !strings.Contains(body, "This is not an OverSire link.") || strings.Contains(body, "oversire://") {
		t.Fatalf("bad route: %d %s", code, body)
	}
	r = newRig()
	loop, _ = serve(t, r.o, miniV4)
	if code, _, body = send(t, "GET", loop+"/go/home", ""); code != 503 || !strings.Contains(body, "No OverSire app is open.") {
		t.Fatalf("no app: %d %s", code, body)
	}
}

func TestGoNoPush(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	loop, _ := serve(t, r.o, miniV4)
	for _, h := range [][]string{{"Sec-Purpose", "prefetch"}, {"Purpose", "prefetch"}} {
		if code, _, body := send(t, "GET", loop+"/go/item/OVERSIRE-400", "", h...); code != 200 || !strings.Contains(body, "Open OVERSIRE-400 in OverSire") {
			t.Fatalf("%v: %d %s", h, code, body)
		}
	}
	if code, _, _ := send(t, "HEAD", loop+"/go/item/OVERSIRE-400", ""); code != 200 {
		t.Fatalf("head: %d", code)
	}
	if len(r.s.sent) != 0 {
		t.Fatalf("pushes: %d", len(r.s.sent))
	}
	for i := range pushLimit + 1 {
		code, _, _ := send(t, "GET", loop+"/go/item/A-"+string(rune('0'+i%10))+string(rune('a'+i/10)), "")
		if i < pushLimit && code != 200 || i == pushLimit && code != 429 {
			t.Fatalf("push %d: %d", i, code)
		}
	}
}

func TestPresenceRoute(t *testing.T) {
	r := newRig()
	loop, tn := serve(t, r.o, mbaV4)
	if code, _, _ := send(t, "POST", tn+"/v1/presence", `{"idle":14}`, "Authorization", "Bearer good"); code != 204 {
		t.Fatalf("app: %d", code)
	}
	if code, _, _ := send(t, "POST", loop+"/v1/presence", `{"idle":3}`); code != 204 {
		t.Fatalf("loopback: %d", code)
	}
	all := r.book.All()
	if len(all) != 2 || all[0].Machine != "mbp" || all[1].Machine != "mba" || all[1].InputAt != r.now.Add(-14*time.Second) || all[1].Addr != mbaV4 {
		t.Fatalf("book %+v", all)
	}
	if code, _, _ := send(t, "POST", tn+"/v1/presence", `{"idle":1}`, "Authorization", "Bearer peer"); code != 403 {
		t.Fatalf("daemon: %d", code)
	}
	if code, _, _ := send(t, "POST", tn+"/v1/presence", `{"idle":-1}`, "Authorization", "Bearer good"); code != 400 {
		t.Fatalf("negative: %d", code)
	}
	if code, _, _ := send(t, "POST", tn+"/v1/presence", `{"idle":1}`); code != 401 {
		t.Fatalf("no credential: %d", code)
	}
}

func TestOpenRouteIsLoopbackOnly(t *testing.T) {
	r := newRig(mbpApp, mbaApp)
	r.both()
	loop, tn := serve(t, r.o, mbaV4)
	if code, _, _ := send(t, "POST", tn+"/v1/open", `{"path":"item/A-1"}`, "Authorization", "Bearer good"); code != 404 && code != 405 {
		t.Fatalf("tailnet: %d", code)
	}
	code, _, body := send(t, "POST", loop+"/v1/open", `{"path":"item/A-1","on":"mba","agent":"claude s-1"}`)
	var res wire.OpenResult
	_ = json.Unmarshal([]byte(body), &res)
	if code != 200 || res.Machine != "mba" || res.How != wire.OpenOn {
		t.Fatalf("%d %s", code, body)
	}
	for body, want := range map[string]int{
		`{"path":"nope"}`:                 400,
		`{"path":"item/A-1","on":"zzz"}`:  404,
		`{"path":"item/A-1","on":"mini"}`: 503,
	} {
		if code, _, b := send(t, "POST", loop+"/v1/open", body); code != want {
			t.Errorf("%s: %d %s", body, code, b)
		}
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))
