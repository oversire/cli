package orders

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	maxOrderBody = 1 << 20
	// answerWait is the expiry of a question answer, the same as on the hub.
	answerWait = 60 * time.Minute
)

// Direct takes work orders from the apps of the group without the hub.
// The results and the session states go to the WebSocket clients as runner.json frames.
type Direct struct {
	Log *slog.Logger

	runner *Runner
	notify func(any)
	clock  func() time.Time

	mu       sync.Mutex
	states   map[string]wire.State // the orders that run now: queued, then acked
	sessions map[string]wire.RunnerSession
	wg       sync.WaitGroup
}

// OrderState is the answer of GET /v1/orders/{id}.
type OrderState struct {
	State  wire.State            `json:"state"`
	Result *wire.WorkOrderResult `json:"result,omitempty"`
}

// Accepted is the answer of POST /v1/orders for a new order.
type Accepted struct {
	Order wire.WorkOrder `json:"order"`
}

// PermissionBody is the body of POST /v1/runner-sessions/{id}/permission.
type PermissionBody struct {
	Permission *string `json:"permission"`
	Allow      *bool   `json:"allow"`
}

func NewDirect(runner *Runner, notify func(any), clock func() time.Time) *Direct {
	return &Direct{Log: slog.Default(), runner: runner, notify: notify, clock: clock,
		states: map[string]wire.State{}, sessions: map[string]wire.RunnerSession{}}
}

func (d *Direct) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/orders", Access: server.Member, Handle: d.post},
		{Pattern: "GET /v1/orders/{id}", Access: server.Member, Handle: d.get},
		{Pattern: "POST /v1/runner-sessions/{id}/permission", Access: server.Member, Handle: d.permission},
		{Pattern: "POST /v1/runner-sessions/{id}/answer", Access: server.Member, Handle: d.answer},
	}
}

// Report keeps the session for the answer routes and sends it as a session frame. It does not block.
func (d *Direct) Report(s wire.RunnerSession) {
	d.mu.Lock()
	if s.State.Ended() {
		delete(d.sessions, s.ID)
	} else {
		d.sessions[s.ID] = s
	}
	d.mu.Unlock()
	d.send(wire.Session{Session: s})
}

// Wait returns after the started orders end.
func (d *Direct) Wait() { d.wg.Wait() }

func (d *Direct) post(w http.ResponseWriter, r *http.Request) {
	if !d.ordersFrom(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOrderBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var o wire.WorkOrder
	if err := json.Unmarshal(body, &o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if res, ok := d.runner.Result(o.ID); ok {
		d.send(res)
		writeJSON(w, http.StatusOK, OrderState{State: wire.State(res.State), Result: res.Result})
		return
	}
	d.start(o, r)
	writeJSON(w, http.StatusAccepted, Accepted{Order: o})
}

func (d *Direct) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if res, ok := d.runner.Result(id); ok {
		writeJSON(w, http.StatusOK, OrderState{State: wire.State(res.State), Result: res.Result})
		return
	}
	d.mu.Lock()
	st, ok := d.states[id]
	d.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, OrderState{State: st})
}

// permission answers the pending permission prompt of a session with a sessionSend order.
func (d *Direct) permission(w http.ResponseWriter, r *http.Request) {
	if !d.ordersFrom(w, r) {
		return
	}
	var b PermissionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxOrderBody)).Decode(&b); err != nil || b.Permission == nil || b.Allow == nil {
		writeError(w, http.StatusBadRequest, "body must be {permission, allow}")
		return
	}
	now := d.clock()
	d.mu.Lock()
	s, ok := d.sessions[r.PathValue("id")]
	switch {
	case !ok:
		d.mu.Unlock()
		writeError(w, http.StatusNotFound, "not found")
		return
	case s.Permission == nil || s.Permission.ID != *b.Permission || s.State != wire.SessionWaitingPermission:
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "no such prompt is pending")
		return
	case !s.Permission.ExpiresAt.After(now):
		d.mu.Unlock()
		writeError(w, http.StatusGone, "the prompt has expired")
		return
	}
	o := d.sendOrder(s, now, s.Permission.ExpiresAt, wire.SendPayload{Session: s.Session,
		Permission: &wire.PermissionAnswer{ID: s.Permission.ID, Allow: *b.Allow}})
	s.Permission = nil
	d.answered(s, now)
	d.mu.Unlock()
	d.send(wire.Session{Session: s})
	d.start(o, r)
	writeJSON(w, http.StatusOK, o)
}

// answer answers the open question of a session with a sessionSend order.
func (d *Direct) answer(w http.ResponseWriter, r *http.Request) {
	if !d.ordersFrom(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOrderBody))
	var a wire.QuestionAnswer
	if err == nil {
		err = wire.Decode(body, &a)
	}
	if err != nil || a.ID == "" || a.Items == nil {
		writeError(w, http.StatusBadRequest, "body must be a RunnerAnswer {id, items, note?}")
		return
	}
	now := d.clock()
	d.mu.Lock()
	s, ok := d.sessions[r.PathValue("id")]
	switch {
	case !ok:
		d.mu.Unlock()
		writeError(w, http.StatusNotFound, "not found")
		return
	case s.Question == nil || s.Question.ID != a.ID:
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "no such question is open")
		return
	}
	o := d.sendOrder(s, now, now.Add(answerWait), wire.SendPayload{Session: s.Session, Answer: &a})
	s.Question = nil
	d.answered(s, now)
	d.mu.Unlock()
	d.send(wire.Session{Session: s})
	d.start(o, r)
	writeJSON(w, http.StatusOK, o)
}

// answered sets the session to working until the next report. The caller holds mu.
func (d *Direct) answered(s wire.RunnerSession, now time.Time) {
	s.State, s.UpdatedAt = wire.SessionWorking, now
	d.sessions[s.ID] = s
}

func (d *Direct) sendOrder(s wire.RunnerSession, now, expires time.Time, p wire.SendPayload) wire.WorkOrder {
	return wire.WorkOrder{ID: "wo-" + randomHex(8), Runner: s.Runner, Runtime: s.Session.Workspace.Account.Provider,
		Suggestion: s.Order, CreatedAt: now, ExpiresAt: expires, State: wire.StateQueued, Payload: p}
}

// ordersFrom refuses a daemon credential: daemons do not order each other.
func (d *Direct) ordersFrom(w http.ResponseWriter, r *http.Request) bool {
	if c, ok := server.CallerFrom(r.Context()); ok && c.Kind == string(wire.DeviceDaemon) {
		writeError(w, http.StatusForbidden, "only an app gives orders")
		return false
	}
	return true
}

// start runs o in the background unless it runs already. The order outlives the request r.
// An order from loopback skips the allow list.
func (d *Direct) start(o wire.WorkOrder, r *http.Request) {
	c, ok := server.CallerFrom(r.Context())
	loopback, caller := ok && c.Loopback, c.Name
	if loopback {
		caller = "loopback"
	}
	log := d.Log.With("order", o.ID, "op", o.Payload.Op(), "runtime", o.Runtime, "via", "direct")
	log.Info("order received", "caller", caller, "id", r.Header.Get(server.RequestID))
	d.mu.Lock()
	if _, ok := d.states[o.ID]; ok {
		d.mu.Unlock()
		return
	}
	d.states[o.ID] = wire.StateQueued
	d.mu.Unlock()
	d.wg.Go(func() {
		defer func() {
			d.mu.Lock()
			delete(d.states, o.ID)
			d.mu.Unlock()
		}()
		ctx := context.Background()
		if loopback {
			ctx = server.WithCaller(ctx, server.Caller{Loopback: true})
		}
		start := d.clock()
		accepted := false
		res, out := d.runner.Run(ctx, o, Hooks{Accepted: func() {
			accepted = true
			d.mu.Lock()
			d.states[o.ID] = wire.StateAcked
			d.mu.Unlock()
			d.send(wire.Ack{Order: o.ID})
			log.Info("order started")
		}})
		switch out {
		case Busy:
			return
		case Ran:
			LogResult(log, res, accepted, d.clock().Sub(start))
		}
		d.send(res)
		d.runner.MarkSent(o.ID)
	})
}

func (d *Direct) send(m wire.Message) {
	b, err := wire.Encode(m)
	if err != nil {
		d.Log.Error("direct frame", "err", err)
		return
	}
	d.notify(json.RawMessage(b))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
