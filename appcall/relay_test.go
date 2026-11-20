package appcall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// fakeApp is the notify of the relay. answer runs for each frame.
type fakeApp struct {
	mu     sync.Mutex
	frames []wire.AppCallFrame
	answer func(wire.AppCall)
}

func (a *fakeApp) notify(loopbackOnly bool, msg any) int {
	if !loopbackOnly {
		panic("an app call goes only to loopback clients")
	}
	f := msg.(wire.AppCallFrame)
	a.mu.Lock()
	a.frames = append(a.frames, f)
	a.mu.Unlock()
	if a.answer != nil {
		go a.answer(f.Call)
	}
	return 1
}

func (a *fakeApp) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.frames)
}

func newRelay(app *fakeApp, has bool, wait time.Duration) *Relay {
	r := New(app.notify, func() bool { return has }, wait)
	r.Log = slog.New(slog.DiscardHandler)
	return r
}

func TestDoDeliversResult(t *testing.T) {
	app := &fakeApp{}
	r := newRelay(app, true, time.Second)
	app.answer = func(c wire.AppCall) {
		if err := r.Deliver(wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "1 epic.", Data: json.RawMessage(`[1]`)}); err != nil {
			t.Error(err)
		}
	}
	res, err := r.Do(context.Background(), wire.AppCall{Action: "epic.list", Agent: "claude s1"})
	if err != nil || res.Summary != "1 epic." || string(res.Data) != "[1]" {
		t.Fatal(res, err)
	}
	f := app.frames[0]
	if f.Type != "appCall" || f.Call.ID == "" || f.Call.ID != res.ID || string(f.Call.Args) != "{}" || f.Call.Agent != "claude s1" {
		t.Fatal(f)
	}
	b, _ := json.Marshal(f)
	if err := wire.Decode(b, &wire.AppCallFrame{}); err != nil || !strings.HasPrefix(string(b), `{"type":"appCall","call":{"id":"c-`) {
		t.Fatal(string(b), err)
	}
}

func TestNoApp(t *testing.T) {
	app := &fakeApp{}
	r := newRelay(app, false, time.Second)
	if _, err := r.Do(context.Background(), wire.AppCall{Action: "epic.list"}); !errors.Is(err, ErrNoApp) {
		t.Fatal(err)
	}
	if app.count() != 0 {
		t.Fatal("a frame went out")
	}
}

func TestBusyApp(t *testing.T) {
	r := New(func(bool, any) int { return 0 }, func() bool { return true }, time.Second)
	r.Log = slog.New(slog.DiscardHandler)
	if _, err := r.Do(context.Background(), wire.AppCall{Action: "epic.list"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestTimeout(t *testing.T) {
	app := &fakeApp{}
	r := newRelay(app, true, 50*time.Millisecond)
	if _, err := r.Do(context.Background(), wire.AppCall{Action: "epic.list"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	r.mu.Lock()
	n := len(r.pending)
	r.mu.Unlock()
	if n != 0 {
		t.Fatal("the pending entry stays")
	}
	if err := r.Deliver(wire.AppCallResult{ID: app.frames[0].Call.ID, Status: wire.AppCallOK}); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}

// rig serves the routes as the server does: all on loopback, only the routes that are not LoopbackOnly on Tailscale.
func rig(t *testing.T, r *Relay) (loop, net *httptest.Server) {
	t.Helper()
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	for _, rt := range r.Routes() {
		loopMux.HandleFunc(rt.Pattern, func(w http.ResponseWriter, req *http.Request) {
			rt.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		if rt.Access != server.LoopbackOnly {
			netMux.HandleFunc(rt.Pattern, rt.Handle)
		}
	}
	loop, net = httptest.NewServer(loopMux), httptest.NewServer(netMux)
	t.Cleanup(func() { loop.Close(); net.Close() })
	return loop, net
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSecondResultConflict(t *testing.T) {
	app := &fakeApp{}
	r := newRelay(app, true, time.Second)
	loop, _ := rig(t, r)
	codes := make(chan int, 2)
	app.answer = func(c wire.AppCall) {
		for range 2 {
			code, _ := post(t, loop.URL+"/v1/app-calls/"+c.ID+"/result", `{"id":"`+c.ID+`","status":"ok","summary":"done"}`)
			codes <- code
		}
	}
	code, body := post(t, loop.URL+"/v1/app-calls", `{"action":"epic.close","args":{"project":"OVERSIRE","epic":"Plan"}}`)
	if code != 200 || !strings.Contains(body, `"summary":"done"`) {
		t.Fatal(code, body)
	}
	if a, b := <-codes, <-codes; a != http.StatusNoContent || b != http.StatusConflict {
		t.Fatal(a, b)
	}
	if code, _ := post(t, loop.URL+"/v1/app-calls/c-none/result", `{"id":"c-none","status":"ok","summary":""}`); code != 404 {
		t.Fatal(code)
	}
	if code, _ := post(t, loop.URL+"/v1/app-calls/c-x/result", `{"id":"c-y","status":"ok","summary":""}`); code != 400 {
		t.Fatal(code)
	}
	if code, _ := post(t, loop.URL+"/v1/app-calls", `{"action":"epic.list","args":[]}`); code != 400 {
		t.Fatal(code)
	}
}

func TestRouteErrors(t *testing.T) {
	loop, _ := rig(t, newRelay(&fakeApp{}, false, time.Second))
	if code, body := post(t, loop.URL+"/v1/app-calls", `{"action":"epic.list"}`); code != 503 || !strings.Contains(body, "Open the OverSire app.") {
		t.Fatal(code, body)
	}
	loop, _ = rig(t, newRelay(&fakeApp{}, true, 20*time.Millisecond))
	if code, body := post(t, loop.URL+"/v1/app-calls", `{"action":"epic.list"}`); code != 504 || !strings.Contains(body, "Update the app.") {
		t.Fatal(code, body)
	}
}

func TestRoutesLoopbackOnly(t *testing.T) {
	r := newRelay(&fakeApp{}, true, time.Second)
	for _, rt := range r.Routes() {
		if rt.Access != server.LoopbackOnly {
			t.Errorf("%s is not loopback only", rt.Pattern)
		}
	}
	_, net := rig(t, r)
	for _, p := range []string{"/v1/app-calls", "/v1/app-calls/c-1/result"} {
		if code, _ := post(t, net.URL+p, `{}`); code != http.StatusNotFound {
			t.Errorf("%s on Tailscale: %d", p, code)
		}
	}
}

func TestCallBodyLimit(t *testing.T) {
	app := &fakeApp{}
	r := newRelay(app, true, time.Second)
	app.answer = func(c wire.AppCall) {
		r.Deliver(wire.AppCallResult{ID: c.ID, Status: wire.AppCallOK, Summary: "got " + strconv.Itoa(len(c.Args))})
	}
	loop, _ := rig(t, r)
	body := func(n int) string { return `{"action":"item.attach","args":{"data":"` + strings.Repeat("a", n) + `"}}` }
	if code, out := post(t, loop.URL+"/v1/app-calls", body(2<<20)); code != 200 || !strings.Contains(out, `"summary":"got `) {
		t.Fatal(code, out[:min(len(out), 200)])
	}
	code, out := post(t, loop.URL+"/v1/app-calls", body(17<<20))
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(out, "The call is too large.") {
		t.Fatal(code, out)
	}
	if app.count() != 1 {
		t.Fatalf("%d frames", app.count())
	}
}
