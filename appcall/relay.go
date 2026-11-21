// Package appcall relays the app calls of the CLI (app-call.json): `oversire epic …` asks the app on this Mac
// to run an action. The daemon sends the call on the WebSocket of the app and holds the request until the app
// posts the result.
package appcall

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Wait is how long a call waits for the app. The CLI client waits longer.
const Wait = 60 * time.Second

// MaxCallBody is the largest body of POST /v1/app-calls. It holds an attachment of 10 MB as base64.
const MaxCallBody = 16 << 20

// MaxResultBody is the largest body of POST /v1/app-calls/{id}/result. A vendor output of 1 MB can pass 4 MiB
// when JSON escapes it.
const MaxResultBody = 16 << 20

// answeredKeep is how long the relay knows an answered id, to refuse a second result.
const answeredKeep = 10 * time.Minute

var (
	ErrNoApp    = errors.New("Open the OverSire app.")
	ErrBusy     = errors.New("The app did not take the call. Try again.")
	ErrUnknown  = errors.New("no such app call")
	ErrAnswered = errors.New("the app call has a result")
)

type Relay struct {
	Log *slog.Logger

	notify func(loopbackOnly bool, msg any) int
	hasApp func() bool
	wait   time.Duration
	now    func() time.Time

	mu       sync.Mutex
	pending  map[string]chan wire.AppCallResult
	answered map[string]time.Time
}

// New makes the relay. notify sends a frame and gives the number of clients that took it (server.Server.Deliver).
// hasApp tells if the app of this Mac is connected.
func New(notify func(loopbackOnly bool, msg any) int, hasApp func() bool, wait time.Duration) *Relay {
	return &Relay{Log: slog.Default(), notify: notify, hasApp: hasApp, wait: wait, now: time.Now,
		pending: map[string]chan wire.AppCallResult{}, answered: map[string]time.Time{}}
}

// Do gives the call a new id, sends it to the app and waits for its result.
// The end of the wait gives context.DeadlineExceeded.
func (r *Relay) Do(ctx context.Context, c wire.AppCall) (wire.AppCallResult, error) {
	if !r.hasApp() {
		return wire.AppCallResult{}, ErrNoApp
	}
	c.ID = newID()
	if len(c.Args) == 0 {
		c.Args = json.RawMessage("{}")
	}
	ch := make(chan wire.AppCallResult, 1)
	r.mu.Lock()
	r.pending[c.ID] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, c.ID)
		r.mu.Unlock()
	}()
	if r.notify(true, wire.AppCallFrame{Type: wire.FrameAppCall, Call: c}) == 0 {
		r.Log.Warn("app call: no app took the frame", "id", c.ID, "action", c.Action)
		return wire.AppCallResult{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, r.wait)
	defer cancel()
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		r.Log.Warn("app call: no result", "id", c.ID, "action", c.Action, "change", Action{Name: c.Action, Args: c.Args}.Changes(), "err", ctx.Err())
		return wire.AppCallResult{}, ctx.Err()
	}
}

// Deliver gives the result of the app to the waiting call.
func (r *Relay) Deliver(res wire.AppCallResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, at := range r.answered {
		if now.Sub(at) > answeredKeep {
			delete(r.answered, id)
		}
	}
	if _, ok := r.answered[res.ID]; ok {
		return ErrAnswered
	}
	ch, ok := r.pending[res.ID]
	if !ok {
		return ErrUnknown
	}
	r.answered[res.ID] = now
	delete(r.pending, res.ID)
	ch <- res
	return nil
}

// Routes gives the app-call routes. Only the CLI and the app of this Mac call them.
func (r *Relay) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/app-calls", Access: server.LoopbackOnly, Handle: r.serveCall},
		{Pattern: "POST /v1/app-calls/{id}/result", Access: server.LoopbackOnly, Handle: r.serveResult},
	}
}

func (r *Relay) serveCall(w http.ResponseWriter, req *http.Request) {
	var c wire.AppCall
	err := server.ReadJSON(req, MaxCallBody, &c)
	if errors.Is(err, server.ErrTooLarge) {
		server.WriteError(w, http.StatusRequestEntityTooLarge, "The call is too large.")
		return
	}
	switch {
	case err != nil:
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	case c.Action == "":
		server.WriteError(w, http.StatusBadRequest, "no action")
		return
	case len(c.Args) > 0 && !isObject(c.Args):
		server.WriteError(w, http.StatusBadRequest, "args must be a JSON object")
		return
	}
	res, err := r.Do(req.Context(), c)
	switch {
	case errors.Is(err, ErrNoApp), errors.Is(err, ErrBusy):
		server.WriteError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		server.WriteError(w, http.StatusGatewayTimeout, Action{Name: c.Action, Args: c.Args}.TimeoutMessage("The app"))
	case err != nil:
		server.WriteError(w, http.StatusServiceUnavailable, err.Error())
	default:
		server.WriteJSON(w, http.StatusOK, res)
	}
}

func (r *Relay) serveResult(w http.ResponseWriter, req *http.Request) {
	var res wire.AppCallResult
	if !server.Decode(w, req, MaxResultBody, "", &res) {
		return
	}
	if res.ID != req.PathValue("id") {
		server.WriteError(w, http.StatusBadRequest, "the id of the body is not the id of the path")
		return
	}
	switch err := r.Deliver(res); {
	case errors.Is(err, ErrUnknown):
		server.WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrAnswered):
		server.WriteError(w, http.StatusConflict, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func isObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

func newID() string {
	return "c-" + wire.RandomHex(8)
}
