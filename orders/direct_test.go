package orders

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
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type fakeRuntimes struct {
	mu    sync.Mutex
	calls []wire.WorkOrder
}

func (f *fakeRuntimes) Has(id string) bool { return id == "oversire" }
func (f *fakeRuntimes) Do(_ context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, o)
	f.mu.Unlock()
	return &wire.WorkOrderResult{Session: &ref}, nil
}

func (f *fakeRuntimes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeAllow struct{ agent string }

func (a fakeAllow) Check(o wire.WorkOrder) error {
	if p, ok := o.Payload.(wire.LaunchRequest); ok && p.Agent != a.agent {
		return errors.New("The allow list does not have the agent " + p.Agent + ".")
	}
	return nil
}

type memJournal struct {
	mu   sync.Mutex
	res  map[string]wire.Result
	sent map[string]bool
}

func (j *memJournal) Get(id string) (wire.Result, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.res[id]
	return r, ok
}
func (j *memJournal) Put(r wire.Result) {
	j.mu.Lock()
	j.res[r.Order] = r
	j.mu.Unlock()
}
func (j *memJournal) MarkSent(id string) {
	j.mu.Lock()
	j.sent[id] = true
	j.mu.Unlock()
}

var ref = wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/oversire-1"}, ID: "s-1"}

func order(id, agent string, expires time.Time) wire.WorkOrder {
	return wire.WorkOrder{ID: id, Runner: "mbp", Runtime: "oversire", Suggestion: "sg-1", CreatedAt: now, ExpiresAt: expires,
		State: wire.StateQueued, Payload: wire.LaunchRequest{Agent: agent, Repo: "/src/app", Prompt: "fix"}}
}

type rig struct {
	d      *Direct
	rts    *fakeRuntimes
	jr     *memJournal
	srv    *httptest.Server
	mu     sync.Mutex
	caller server.Caller
	frames []wire.Message
}

func newRig(t *testing.T) *rig {
	g := &rig{rts: &fakeRuntimes{}, jr: &memJournal{res: map[string]wire.Result{}, sent: map[string]bool{}}, caller: server.Caller{Loopback: true}}
	g.d = NewDirect(NewRunner(g.rts, fakeAllow{agent: "claude"}, g.jr, func() time.Time { return now }), g.notify, func() time.Time { return now })
	g.d.Log = slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	for _, r := range g.d.Routes() {
		mux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			g.mu.Lock()
			c := g.caller
			g.mu.Unlock()
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), c)))
		})
	}
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *rig) setCaller(c server.Caller) {
	g.mu.Lock()
	g.caller = c
	g.mu.Unlock()
}

func (g *rig) notify(m any) {
	msg, err := wire.DecodeMessage(m.(json.RawMessage))
	if err != nil {
		panic(err)
	}
	g.mu.Lock()
	g.frames = append(g.frames, msg)
	g.mu.Unlock()
}

func (g *rig) results() []wire.Result {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []wire.Result
	for _, m := range g.frames {
		if r, ok := m.(wire.Result); ok {
			out = append(out, r)
		}
	}
	return out
}

func (g *rig) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(g.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (g *rig) state(t *testing.T, id string) (int, OrderState) {
	t.Helper()
	resp, err := http.Get(g.srv.URL + "/v1/orders/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st OrderState
	_ = json.NewDecoder(resp.Body).Decode(&st)
	return resp.StatusCode, st
}

func TestDirectOrder(t *testing.T) {
	g := newRig(t)
	code, body := g.post(t, "/v1/orders", order("wo-1", "claude", now.Add(time.Hour)))
	if code != http.StatusAccepted {
		t.Fatal(code, string(body))
	}
	var acc struct {
		Order wire.WorkOrder `json:"order"`
	}
	if err := json.Unmarshal(body, &acc); err != nil || acc.Order.ID != "wo-1" {
		t.Fatal("answer:", err, string(body))
	}
	g.d.Wait()
	code, st := g.state(t, "wo-1")
	if code != 200 || st.State != wire.StateDone || st.Result == nil || st.Result.Session.ID != "s-1" {
		t.Fatalf("state: %d %+v", code, st)
	}
	res := g.results()
	if len(res) != 1 || res[0].Order != "wo-1" || res[0].State != wire.ResultDone {
		t.Fatalf("frames: %+v", g.frames)
	}
	if _, ok := g.frames[0].(wire.Ack); !ok {
		t.Fatalf("first frame: %#v", g.frames[0])
	}
	if !g.jr.sent["wo-1"] {
		t.Fatal("result not marked sent")
	}
	if code, _ := g.state(t, "wo-nope"); code != 404 {
		t.Fatal("unknown order:", code)
	}
}

func TestDirectRepeatID(t *testing.T) {
	g := newRig(t)
	o := order("wo-1", "claude", now.Add(time.Hour))
	g.post(t, "/v1/orders", o)
	g.d.Wait()
	code, body := g.post(t, "/v1/orders", o)
	g.d.Wait()
	var st OrderState
	_ = json.Unmarshal(body, &st)
	if code != 200 || st.State != wire.StateDone || st.Result.Session.ID != "s-1" {
		t.Fatal(code, string(body))
	}
	if g.rts.count() != 1 {
		t.Fatal("runs:", g.rts.count())
	}
	if len(g.results()) != 2 {
		t.Fatal("the stored result goes out again:", g.results())
	}
}

func TestDirectExpired(t *testing.T) {
	g := newRig(t)
	g.post(t, "/v1/orders", order("wo-1", "claude", now.Add(-time.Second)))
	g.d.Wait()
	if _, st := g.state(t, "wo-1"); st.State != wire.StateExpired {
		t.Fatal(st)
	}
	if g.rts.count() != 0 || g.results()[0].State != wire.ResultExpired {
		t.Fatal("expired order ran")
	}
}

func TestDirectAllowList(t *testing.T) {
	g := newRig(t)
	g.setCaller(server.Caller{DeviceID: "a-1", Kind: string(wire.DeviceApp), Machine: "mba"})
	g.post(t, "/v1/orders", order("wo-1", "codex", now.Add(time.Hour)))
	g.d.Wait()
	_, st := g.state(t, "wo-1")
	if st.State != wire.StateFailed || st.Result == nil || st.Result.Error == "" || g.rts.count() != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestDirectLoopbackSkipsAllowList(t *testing.T) {
	g := newRig(t)
	g.post(t, "/v1/orders", order("wo-1", "codex", now.Add(time.Hour)))
	g.d.Wait()
	if _, st := g.state(t, "wo-1"); st.State != wire.StateDone || g.rts.count() != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestDirectRefusesDaemon(t *testing.T) {
	g := newRig(t)
	g.setCaller(server.Caller{DeviceID: "d-2", Kind: string(wire.DeviceDaemon), Machine: "mba"})
	for _, path := range []string{"/v1/orders", "/v1/runner-sessions/s-1/permission", "/v1/runner-sessions/s-1/answer"} {
		if code, _ := g.post(t, path, order("wo-1", "claude", now.Add(time.Hour))); code != http.StatusForbidden {
			t.Fatal(path, code)
		}
	}
	g.setCaller(server.Caller{DeviceID: "a-1", Kind: string(wire.DeviceApp), Machine: "mba"})
	if code, _ := g.post(t, "/v1/orders", order("wo-1", "claude", now.Add(time.Hour))); code != http.StatusAccepted {
		t.Fatal("app:", code)
	}
	g.d.Wait()
}

func TestDirectBadBody(t *testing.T) {
	g := newRig(t)
	if code, _ := g.post(t, "/v1/orders", map[string]any{"id": "wo-1", "op": "shell"}); code != http.StatusBadRequest {
		t.Fatal(code)
	}
}

func TestDirectPermission(t *testing.T) {
	g := newRig(t)
	s := wire.RunnerSession{ID: "s-1", Runner: "mbp", Session: ref, Order: "wo-1", Agent: "claude", Repo: "/src/app",
		State: wire.SessionWaitingPermission, StartedAt: now, UpdatedAt: now,
		Permission: &wire.PermissionPrompt{ID: "p-1", Tool: "Bash", Text: "ls", ExpiresAt: now.Add(time.Minute)}}
	g.d.Report(s)
	if code, _ := g.post(t, "/v1/runner-sessions/s-1/permission", map[string]any{"permission": "p-2", "allow": true}); code != http.StatusConflict {
		t.Fatal("wrong prompt:", code)
	}
	if code, _ := g.post(t, "/v1/runner-sessions/s-9/permission", map[string]any{"permission": "p-1", "allow": true}); code != http.StatusNotFound {
		t.Fatal("unknown session:", code)
	}
	if code, _ := g.post(t, "/v1/runner-sessions/s-1/permission", map[string]any{"permission": "p-1"}); code != http.StatusBadRequest {
		t.Fatal("no allow:", code)
	}
	code, body := g.post(t, "/v1/runner-sessions/s-1/permission", map[string]any{"permission": "p-1", "allow": true})
	if code != 200 {
		t.Fatal(code, string(body))
	}
	g.d.Wait()
	if g.rts.count() != 1 {
		t.Fatal("no send order")
	}
	p := g.rts.calls[0].Payload.(wire.SendPayload)
	if p.Permission == nil || p.Permission.ID != "p-1" || !p.Permission.Allow || p.Session != ref {
		t.Fatalf("%+v", p)
	}
	if code, _ := g.post(t, "/v1/runner-sessions/s-1/permission", map[string]any{"permission": "p-1", "allow": true}); code != http.StatusConflict {
		t.Fatal("second answer:", code)
	}

	s.ID, s.Permission = "s-2", &wire.PermissionPrompt{ID: "p-3", ExpiresAt: now}
	g.d.Report(s)
	if code, _ := g.post(t, "/v1/runner-sessions/s-2/permission", map[string]any{"permission": "p-3", "allow": false}); code != http.StatusGone {
		t.Fatal("expired prompt:", code)
	}
}

func TestDirectAnswer(t *testing.T) {
	g := newRig(t)
	g.d.Report(wire.RunnerSession{ID: "s-1", Runner: "mbp", Session: ref, Order: "wo-1", Agent: "claude", State: wire.SessionWaitingInput,
		Question: &wire.RunnerQuestion{ID: "q-1", Round: 1, AskedAt: now}, StartedAt: now, UpdatedAt: now})
	a := wire.QuestionAnswer{ID: "q-1", Items: []wire.QuestionAnswerItem{{ID: "i-1", Choices: []string{"yes"}}}}
	if code, _ := g.post(t, "/v1/runner-sessions/s-1/answer", wire.QuestionAnswer{ID: "q-2", Items: a.Items}); code != http.StatusConflict {
		t.Fatal("wrong question:", code)
	}
	if code, _ := g.post(t, "/v1/runner-sessions/s-1/answer", map[string]any{"id": "q-1", "items": []any{}, "x": 1}); code != http.StatusBadRequest {
		t.Fatal("unknown field:", code)
	}
	code, body := g.post(t, "/v1/runner-sessions/s-1/answer", a)
	if code != 200 {
		t.Fatal(code, string(body))
	}
	var o wire.WorkOrder
	if err := json.Unmarshal(body, &o); err != nil || !o.ExpiresAt.Equal(now.Add(answerWait)) {
		t.Fatal(err, string(body))
	}
	g.d.Wait()
	if p := g.rts.calls[0].Payload.(wire.SendPayload); p.Answer == nil || p.Answer.ID != "q-1" {
		t.Fatalf("%+v", p)
	}
}

// The result frame goes to a WebSocket client of the real server.
func TestDirectResultFrame(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	off := false
	srv := server.NewServer(config.ServeConfig{Port: port, Tailscale: &off}, func() wire.DaemonInfo { return wire.DaemonInfo{Machine: "mbp"} },
		nil, eventlog.New(10), noSessions{}, nil)
	srv.Log = slog.New(slog.DiscardHandler)
	jr := &memJournal{res: map[string]wire.Result{}, sent: map[string]bool{}}
	d := NewDirect(NewRunner(&fakeRuntimes{}, fakeAllow{agent: "claude"}, jr, func() time.Time { return now }),
		func(m any) { srv.Notify(false, m) }, func() time.Time { return now })
	d.Log = srv.Log
	srv.Handle(d.Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	base := "127.0.0.1:" + strconv.Itoa(port)
	var conn *websocket.Conn
	for i := 0; i < 200; i++ {
		if conn, _, err = websocket.Dial(ctx, "ws://"+base+"/v1/connect", nil); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// The pong tells that the server has the client.
	read := func() string {
		rctx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		_, data, err := conn.Read(rctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil || read() != "pong" {
		t.Fatal("no pong", err)
	}

	b, _ := json.Marshal(order("wo-1", "claude", now.Add(time.Hour)))
	resp, err := http.Post("http://"+base+"/v1/orders", "application/json", bytes.NewReader(b))
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatal(err, resp)
	}
	resp.Body.Close()
	for {
		m, err := wire.DecodeMessage([]byte(read()))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := m.(wire.Result); ok {
			if r.Order != "wo-1" || r.State != wire.ResultDone || r.Result.Session.ID != "s-1" {
				t.Fatalf("%+v", r)
			}
			break
		}
	}
	d.Wait()
}

type noSessions struct{}

func (noSessions) All() []wire.DaemonSession             { return nil }
func (noSessions) Get(string) (wire.DaemonSession, bool) { return wire.DaemonSession{}, false }
