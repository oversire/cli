package ask

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

func str(s string) *string { return &s }

func phase(p wire.SessionPhase) *wire.SessionPhase { return &p }

// fakeScreen is an Orca terminal. onSend runs inside Send, like an agent that takes the prompt.
type fakeScreen struct {
	mu     sync.Mutex
	sent   []string
	lines  []string
	onSend func()
}

func (f *fakeScreen) Read(_ context.Context, _ string, _ int) ([]string, error) { return f.lines, nil }

func (f *fakeScreen) Send(_ context.Context, terminal, text string, enter bool) error {
	if !enter {
		return errors.New("no enter")
	}
	f.mu.Lock()
	f.sent = append(f.sent, terminal+": "+text)
	on := f.onSend
	f.mu.Unlock()
	if on != nil {
		on()
	}
	return nil
}

func (f *fakeScreen) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

type fakeRuntimes struct {
	mu     sync.Mutex
	orders []wire.WorkOrder
	onSend func()
}

func (f *fakeRuntimes) Do(_ context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	f.mu.Lock()
	f.orders = append(f.orders, o)
	on := f.onSend
	f.mu.Unlock()
	switch o.Payload.(type) {
	case wire.ReadPayload:
		return &wire.WorkOrderResult{Lines: []string{"> fix", "Done."}}, nil
	case wire.SendPayload:
		if on != nil {
			go on()
		}
	}
	return &wire.WorkOrderResult{}, nil
}

type rig struct {
	book   *sessions.Book
	screen *fakeScreen
	rts    *fakeRuntimes
	k      *Asker
	loop   *httptest.Server
	net    *httptest.Server
}

// newRig serves the route on a loopback mux and on a Tailscale mux that checks the credential "good",
// the same way as the server does.
func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{book: sessions.NewBook("mbp", "", nil, nil), screen: &fakeScreen{lines: []string{"⏺ It works.", ">", ""}}, rts: &fakeRuntimes{}}
	g.k = NewAsker(g.book, g.screen, g.rts, nil)
	g.k.Poll, g.k.Settle, g.k.unit = 10*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	for _, r := range g.k.Routes() {
		loopMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		if r.Access == server.LoopbackOnly {
			continue
		}
		netMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer good" && r.Access == server.Member {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{DeviceID: "D1", Kind: "app"})))
		})
	}
	g.loop, g.net = httptest.NewServer(loopMux), httptest.NewServer(netMux)
	t.Cleanup(func() { g.loop.Close(); g.net.Close() })
	return g
}

func (g *rig) orca(id string, state wire.SessionPhase, last string) {
	g.book.Apply(sessions.Patch{ID: id, Source: sessions.SourceHook, Runtime: str(wire.RuntimeOrca), Harness: str("claude"),
		Terminal: str("term-" + id), State: phase(state), LastMessage: str(last)})
}

func (g *rig) oversire(id string, state wire.SessionPhase, last string) {
	g.book.Apply(sessions.Patch{ID: id, Source: sessions.SourceWire, Runtime: str(wire.RuntimeOversire), Harness: str("claude"),
		Workspace: str("ws-1"), State: phase(state), LastMessage: str(last)})
}

func (g *rig) post(t *testing.T, base, id, body string, header ...string) (int, wire.SessionAskReply) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/sessions/"+id+"/ask", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out wire.SessionAskReply
	if resp.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func TestRoute(t *testing.T) {
	r := NewAsker(sessions.NewBook("mbp", "", nil, nil), nil, nil, nil).Routes()
	if len(r) != 1 || r[0].Pattern != "POST /v1/sessions/{id}/ask" || r[0].Access != server.Member {
		t.Fatalf("%+v", r)
	}
}

func TestAskOrcaGivesTheReply(t *testing.T) {
	g := newRig(t)
	g.orca("pane-1", wire.PhaseDone, "old answer")
	// A short turn: it starts and ends inside the send.
	g.screen.onSend = func() {
		g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseWorking)})
		g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseDone), LastMessage: str("TREBI-440 waits for a review.")})
	}
	code, rep := g.post(t, g.loop.URL, "pane-1", `{"text":"What blocks you?"}`)
	if code != http.StatusOK || rep.Reply != "TREBI-440 waits for a review." || rep.TurnEndedAt == nil || rep.Error != "" {
		t.Fatalf("%d %+v", code, rep)
	}
	if rep.ScreenTail != "⏺ It works.\n>" {
		t.Errorf("screen tail %q", rep.ScreenTail)
	}
	if got := g.screen.texts(); len(got) != 1 || got[0] != "term-pane-1: What blocks you?\n"+wire.AskReadOnlyLine {
		t.Errorf("sent %q", got)
	}
}

func TestAskWaitsForTheTurnEnd(t *testing.T) {
	g := newRig(t)
	g.orca("pane-1", wire.PhaseDone, "old")
	g.screen.onSend = func() {
		go func() {
			g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseWorking)})
			time.Sleep(30 * time.Millisecond)
			// The state comes first, the message later.
			g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseWaiting)})
			time.Sleep(10 * time.Millisecond)
			g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, LastMessage: str("new")})
		}()
	}
	code, rep := g.post(t, g.loop.URL, "pane-1", `{"text":"Which test fails?","readOnly":false,"timeoutSec":100}`)
	if code != http.StatusOK || rep.Reply != "new" {
		t.Fatalf("%d %+v", code, rep)
	}
	if got := g.screen.texts(); got[0] != "term-pane-1: Which test fails?" {
		t.Errorf("sent %q", got)
	}
}

func TestAskOversireSteers(t *testing.T) {
	g := newRig(t)
	g.oversire("s-1", wire.PhaseWorking, "")
	g.rts.onSend = func() {
		time.Sleep(20 * time.Millisecond)
		g.oversire("s-1", wire.PhaseWaiting, "All tests pass.")
	}
	code, rep := g.post(t, g.loop.URL, "s-1", `{"text":"Status?"}`)
	if code != http.StatusOK || rep.Reply != "All tests pass." || rep.ScreenTail != "> fix\nDone." {
		t.Fatalf("%d %+v", code, rep)
	}
	o := g.rts.orders[0]
	p, ok := o.Payload.(wire.SendPayload)
	if !ok || o.Runtime != wire.RuntimeOversire || p.Session.ID != "s-1" || p.Session.Workspace.ID != "ws-1" || !strings.HasPrefix(p.Text, "Status?\n") {
		t.Fatalf("%+v", o)
	}
}

func TestAskTimeout(t *testing.T) {
	g := newRig(t)
	g.orca("pane-1", wire.PhaseDone, "old")
	code, rep := g.post(t, g.loop.URL, "pane-1", `{"text":"Hello?","timeoutSec":2}`)
	if code != http.StatusGatewayTimeout || rep.Error != wire.AskTimeout || rep.Message == "" || rep.ScreenTail == "" || rep.Reply != "" {
		t.Fatalf("%d %+v", code, rep)
	}
}

func TestAskBusy(t *testing.T) {
	g := newRig(t)
	g.orca("pane-1", wire.PhaseWorking, "")
	code, rep := g.post(t, g.loop.URL, "pane-1", `{"text":"Hello?"}`)
	if code != http.StatusConflict || rep.Error != wire.AskBusy || rep.ScreenTail == "" {
		t.Fatalf("%d %+v", code, rep)
	}
	if len(g.screen.texts()) != 0 {
		t.Fatal("a busy session got the text")
	}
}

func TestAskRefusals(t *testing.T) {
	g := newRig(t)
	g.book.Apply(sessions.Patch{ID: "cli:abc", Source: sessions.SourceHook, Runtime: str(wire.RuntimeCLI), State: phase(wire.PhaseDone)})
	g.book.Apply(sessions.Patch{ID: "pane-2", Source: sessions.SourceHook, Runtime: str(wire.RuntimeOrca), State: phase(wire.PhaseDone)})
	cases := []struct {
		id, body string
		code     int
		err      string
	}{
		{"nope", `{"text":"x"}`, http.StatusNotFound, wire.AskNoSession},
		{"cli:abc", `{"text":"x"}`, http.StatusConflict, wire.AskUnsupported},
		{"pane-2", `{"text":"x"}`, http.StatusConflict, wire.AskUnsupported}, // no terminal
		{"nope", `{"text":""}`, http.StatusBadRequest, wire.AskBadRequest},
		{"nope", `{"text":"x","other":1}`, http.StatusBadRequest, wire.AskBadRequest},
		{"nope", `{"text":"` + strings.Repeat("a", wire.MaxAskText+1) + `"}`, http.StatusBadRequest, wire.AskBadRequest},
	}
	for _, c := range cases {
		if code, rep := g.post(t, g.loop.URL, c.id, c.body); code != c.code || rep.Error != c.err {
			t.Errorf("%s %.20s: %d %+v", c.id, c.body, code, rep)
		}
	}
}

func TestAskAccess(t *testing.T) {
	g := newRig(t)
	g.orca("pane-1", wire.PhaseDone, "old")
	g.screen.onSend = func() {
		g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseWorking)})
		g.book.Apply(sessions.Patch{ID: "pane-1", Source: sessions.SourceHook, State: phase(wire.PhaseDone), LastMessage: str("hi " + time.Now().String())})
	}
	for _, h := range [][]string{nil, {"Authorization", "Bearer wrong"}} {
		if code, _ := g.post(t, g.net.URL, "pane-1", `{"text":"x"}`, h...); code != http.StatusUnauthorized {
			t.Errorf("remote %v: %d", h, code)
		}
	}
	if len(g.screen.texts()) != 0 {
		t.Fatal("a refused call sent the text")
	}
	if code, rep := g.post(t, g.net.URL, "pane-1", `{"text":"x"}`, "Authorization", "Bearer good"); code != http.StatusOK || rep.Reply == "" {
		t.Errorf("member: %d %+v", code, rep)
	}
	if code, rep := g.post(t, g.loop.URL, "pane-1", `{"text":"x"}`); code != http.StatusOK || rep.Reply == "" {
		t.Errorf("loopback: %d %+v", code, rep)
	}
}
