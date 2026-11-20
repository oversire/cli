package membercall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

var (
	mbaApp  = server.Caller{DeviceID: "D-mba", Name: "OverSire on mba", Kind: string(wire.DeviceApp), Machine: "mba"}
	miniApp = server.Caller{DeviceID: "D-mini", Name: "OverSire on mini", Kind: string(wire.DeviceApp), Machine: "mini"}
	daemon  = server.Caller{DeviceID: "D-d", Name: "mba daemon", Kind: string(wire.DeviceDaemon), Machine: "mba"}
	local   = server.Caller{Loopback: true}
)

type fakeSources struct {
	src mirror.Source
	ok  bool
}

func (f *fakeSources) Source() (mirror.Source, bool) { return f.src, f.ok }

type fakeGroup struct {
	g    wire.Group
	ok   bool
	self string
}

func (f *fakeGroup) Snapshot() (wire.Group, bool) { return f.g, f.ok }
func (f *fakeGroup) Self() wire.GroupMember       { return wire.GroupMember{Machine: f.self} }

// fakeSocket is the deliver func of the relay. It records the frames and gives the number of sockets that took them.
type fakeSocket struct {
	mu      sync.Mutex
	frames  []wire.MemberCallFrame
	sockets int
	matched []server.Caller
	answer  func(wire.MemberCall)
}

func (s *fakeSocket) deliver(match func(server.Caller) bool, msg any) int {
	f := msg.(wire.MemberCallFrame)
	s.mu.Lock()
	s.frames = append(s.frames, f)
	n := 0
	for _, c := range s.matched {
		if match(c) {
			n++
		}
	}
	s.mu.Unlock()
	if n > 0 && s.answer != nil {
		go s.answer(f.Call)
	}
	return n
}

func (s *fakeSocket) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

type rig struct {
	sock  *fakeSocket
	src   *fakeSources
	group *fakeGroup
	relay *Relay
	srv   *httptest.Server
	who   server.Caller
}

// newRig serves the routes of a home daemon "mbp" whose source app is the app of mba, connected on a socket.
func newRig(t *testing.T, wait time.Duration) *rig {
	t.Helper()
	g := &fakeGroup{g: wire.Group{ID: "g", Home: "mbp"}, ok: true, self: "mbp"}
	sock := &fakeSocket{matched: []server.Caller{mbaApp}}
	src := &fakeSources{src: mirror.Source{App: "app-1", Device: "D-mba", Name: "OverSire on mba", Machine: "mba"}, ok: true}
	r := New(sock.deliver, src, g, g.Self, wait)
	r.Log = slog.New(slog.DiscardHandler)
	x := &rig{sock: sock, src: src, group: g, relay: r, who: miniApp}
	mux := http.NewServeMux()
	for _, rt := range r.Routes() {
		if rt.Access != server.Member {
			t.Fatalf("%s: access %v", rt.Pattern, rt.Access)
		}
		mux.HandleFunc(rt.Pattern, rt.Handle)
	}
	x.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(w, req.WithContext(server.WithCaller(req.Context(), x.who)))
	}))
	t.Cleanup(x.srv.Close)
	return x
}

func (x *rig) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(x.srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func call(action string) wire.MemberCall {
	return wire.MemberCall{Action: action, Args: json.RawMessage(`{"item":"A-1"}`), From: wire.MemberCaller{Machine: "mini", Name: "OverSire on mini"}}
}

func TestDoDeliversResult(t *testing.T) {
	x := newRig(t, time.Second)
	x.sock.answer = func(c wire.MemberCall) {
		if err := x.relay.Deliver(c.ID, wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "A-1 is Done."}, mbaApp); err != nil {
			t.Error(err)
		}
	}
	res, err := x.relay.Do(context.Background(), call("item.setStatus"))
	if err != nil || res.Summary != "A-1 is Done." || res.Status != wire.AppCallOK {
		t.Fatal(res, err)
	}
	f := x.sock.frames[0]
	if f.Type != "memberCall" || !strings.HasPrefix(f.Call.ID, "m-") || f.Call.ID != res.ID || f.Call.From.Machine != "mini" {
		t.Fatal(f)
	}
	b, _ := json.Marshal(f)
	if err := wire.Decode(b, &wire.MemberCallFrame{}); err != nil {
		t.Fatal(string(b), err)
	}
}

func TestNoSourceGives503(t *testing.T) {
	x := newRig(t, time.Second)
	x.src.ok = false
	if _, err := x.relay.Do(context.Background(), call("item.setStatus")); !errors.Is(err, ErrNoSource) {
		t.Fatal(err)
	}
	if x.sock.count() != 0 {
		t.Fatal("a frame went out")
	}
	code, body := x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`)
	if code != 503 || !strings.Contains(body, "The OverSire app on mbp is not open.") {
		t.Fatal(code, body)
	}
	// A source with no WebSocket gives the same answer and names its machine.
	x.src.ok = true
	x.sock.matched = nil
	code, body = x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`)
	if code != 503 || !strings.Contains(body, "The OverSire app on mba is not open.") {
		t.Fatal(code, body)
	}
}

func TestTimeoutGives504(t *testing.T) {
	x := newRig(t, 10*time.Millisecond)
	if _, err := x.relay.Do(context.Background(), call("item.setStatus")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	x.relay.mu.Lock()
	n := len(x.relay.pending)
	x.relay.mu.Unlock()
	if n != 0 {
		t.Fatal("the pending entry stays")
	}
	if err := x.relay.Deliver(x.sock.frames[0].Call.ID, wire.AppCallResult{Status: wire.AppCallOK}, mbaApp); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	code, body := x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`)
	if code != 504 || !strings.Contains(body, "mba did not answer. Try again.") {
		t.Fatal(code, body)
	}
}

func TestBusyGives429(t *testing.T) {
	x := newRig(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for range MaxInFlight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = x.relay.Do(ctx, call("item.setStatus"))
		}()
	}
	for i := 0; x.sock.count() < MaxInFlight; i++ {
		if i > 400 {
			t.Fatal("the calls did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := x.relay.Do(context.Background(), call("item.setStatus")); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	code, body := x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`)
	if code != 429 || !strings.Contains(body, "mba is busy. Try again.") {
		t.Fatal(code, body)
	}
	if x.sock.count() != MaxInFlight {
		t.Fatalf("%d frames", x.sock.count())
	}
	cancel()
	wg.Wait()
}

func TestResultFromOtherCallerIs403(t *testing.T) {
	x := newRig(t, time.Second)
	done := make(chan struct{})
	x.sock.answer = func(c wire.MemberCall) {
		defer close(done)
		path := "/v1/member-calls/" + c.ID + "/result"
		body := `{"id":"` + c.ID + `","status":"ok","summary":"x"}`
		x.who = miniApp
		if code, out := x.post(t, path, body); code != 403 || !strings.Contains(out, "Only the source app answers.") {
			t.Error(code, out)
		}
		x.who = local
		if code, _ := x.post(t, path, body); code != 403 {
			t.Error("loopback is not the source:", code)
		}
		x.who = mbaApp
		if code, out := x.post(t, path, body); code != http.StatusNoContent {
			t.Error(code, out)
		}
		if code, _ := x.post(t, path, body); code != http.StatusConflict {
			t.Error("a second result:", code)
		}
		if code, _ := x.post(t, "/v1/member-calls/m-none/result", `{"id":"m-none","status":"ok","summary":""}`); code != 404 {
			t.Error(code)
		}
		if code, _ := x.post(t, "/v1/member-calls/m-x/result", `{"id":"m-y","status":"ok","summary":""}`); code != 400 {
			t.Error(code)
		}
	}
	code, body := x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`)
	<-done
	if code != 200 || !strings.Contains(body, `"summary":"x"`) {
		t.Fatal(code, body)
	}
}

func TestRoutesWhenNotHome(t *testing.T) {
	x := newRig(t, time.Second)
	x.group.g.Home = "box"
	for _, p := range []string{"/v1/member-calls", "/v1/member-calls/m-1/result"} {
		code, body := x.post(t, p, `{"action":"a","args":{}}`)
		if code != 409 || !strings.Contains(body, `"home":"box"`) {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	x.group.ok = false
	if code, _ := x.post(t, "/v1/member-calls", `{}`); code != 409 {
		t.Error(code)
	}
	if x.sock.count() != 0 {
		t.Fatal("a frame went out")
	}
}

func TestRejectsDaemonCredential(t *testing.T) {
	x := newRig(t, time.Second)
	x.who = daemon
	for _, p := range []string{"/v1/member-calls", "/v1/member-calls/m-1/result"} {
		if code, _ := x.post(t, p, `{"action":"a","args":{}}`); code != 403 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if x.sock.count() != 0 {
		t.Fatal("a frame went out")
	}
}

func TestFromIsTheCaller(t *testing.T) {
	x := newRig(t, time.Second)
	x.sock.answer = func(c wire.MemberCall) {
		_ = x.relay.Deliver(c.ID, wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "ok"}, mbaApp)
	}
	x.who = miniApp
	body := `{"action":"item.setStatus","args":{"item":"A-1"},"from":{"machine":"mbp","name":"fake","device":"D-fake"}}`
	if code, out := x.post(t, "/v1/member-calls", body); code != 200 {
		t.Fatal(code, out)
	}
	got := x.sock.frames[0].Call
	if got.From != (wire.MemberCaller{Machine: "mini", Name: "OverSire on mini", Device: "D-mini"}) || got.Action != "item.setStatus" || string(got.Args) != `{"item":"A-1"}` {
		t.Fatalf("%+v", got)
	}
}

func TestFromOnLoopbackIsSelf(t *testing.T) {
	x := newRig(t, time.Second)
	x.sock.answer = func(c wire.MemberCall) {
		_ = x.relay.Deliver(c.ID, wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "ok"}, mbaApp)
	}
	x.who = local
	if code, out := x.post(t, "/v1/member-calls", `{"action":"item.setStatus","args":{}}`); code != 200 {
		t.Fatal(code, out)
	}
	if got := x.sock.frames[0].Call.From; got != (wire.MemberCaller{Machine: "mbp"}) {
		t.Fatalf("%+v", got)
	}
}

func TestBadRequests(t *testing.T) {
	x := newRig(t, time.Second)
	for _, b := range []string{`{"args":{}}`, `{"action":"a","args":[]}`, `nope`} {
		if code, _ := x.post(t, "/v1/member-calls", b); code != 400 {
			t.Errorf("%s: %d", b, code)
		}
	}
	if x.sock.count() != 0 {
		t.Fatal("a frame went out")
	}
}

func TestSourceMatchSendsOnlyToTheSource(t *testing.T) {
	src := mirror.Source{Device: "D-mba", Machine: "mba"}
	if !src.Match(mbaApp) || src.Match(miniApp) || src.Match(local) || src.Match(server.Caller{}) {
		t.Fatal("device source")
	}
	loop := mirror.Source{Loopback: true, Machine: "mbp"}
	if !loop.Match(local) || loop.Match(mbaApp) {
		t.Fatal("loopback source")
	}
}
