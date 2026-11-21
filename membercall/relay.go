// Package membercall relays the clicks of a client app (app-call.json MemberCall). A client sends the click to the home
// daemon. The daemon sends it on the WebSocket of the source app of the mirror and holds the request until the source
// app posts the result. The pattern is the same as in package appcall, but the call crosses machines.
package membercall

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/oversire/cli/appcall"
	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// MaxInFlight is the most calls that wait for the source app at one time.
const MaxInFlight = 8

// answeredKeep is how long the relay knows an answered id, to refuse a second result.
const answeredKeep = 10 * time.Minute

var (
	ErrNoSource  = errors.New("the source app is not open")
	ErrBusy      = errors.New("the source app is busy")
	ErrUnknown   = errors.New("no such member call")
	ErrNotSource = errors.New("only the source app answers")
	ErrAnswered  = errors.New("the member call has a result")
)

// Sources gives the sender of the last accepted put of the mirror (mirror.Store).
type Sources interface {
	Source() (mirror.Source, bool)
}

// Group gives the roster of this machine (group.Store). It is the same as mirror.Group.
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

type pendingCall struct {
	ch  chan wire.AppCallResult
	src mirror.Source
}

type Relay struct {
	Log *slog.Logger

	deliver func(match func(server.Caller) bool, msg any) int
	sources Sources
	group   Group
	self    func() wire.GroupMember
	wait    time.Duration
	now     func() time.Time

	mu       sync.Mutex
	pending  map[string]pendingCall
	answered map[string]time.Time
}

// New makes the relay. deliver sends a frame to the matching clients and gives the number that took it
// (server.Server.DeliverTo). self gives this machine, for a loopback caller. wait is how long a call waits for the source app.
func New(deliver func(match func(server.Caller) bool, msg any) int, src Sources, g Group, self func() wire.GroupMember, wait time.Duration) *Relay {
	return &Relay{Log: slog.Default(), deliver: deliver, sources: src, group: g, self: self, wait: wait, now: time.Now,
		pending: map[string]pendingCall{}, answered: map[string]time.Time{}}
}

// Do gives the call a new id, sends it to the source app and waits for its result.
// The end of the wait gives context.DeadlineExceeded.
func (r *Relay) Do(ctx context.Context, call wire.MemberCall) (wire.AppCallResult, error) {
	src, ok := r.sources.Source()
	if !ok {
		return wire.AppCallResult{}, ErrNoSource
	}
	call.ID = newID()
	if len(call.Args) == 0 {
		call.Args = json.RawMessage("{}")
	}
	ch := make(chan wire.AppCallResult, 1)
	r.mu.Lock()
	if len(r.pending) >= MaxInFlight {
		r.mu.Unlock()
		return wire.AppCallResult{}, ErrBusy
	}
	r.pending[call.ID] = pendingCall{ch: ch, src: src}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, call.ID)
		r.mu.Unlock()
	}()
	if r.deliver(src.Match, wire.MemberCallFrame{Type: wire.FrameMemberCall, Call: call}) == 0 {
		r.Log.Warn("member call: the source app is not connected", "id", call.ID, "action", call.Action, "from", call.From.Machine)
		return wire.AppCallResult{}, ErrNoSource
	}
	ctx, cancel := context.WithTimeout(ctx, r.wait)
	defer cancel()
	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		r.Log.Warn("member call: no result", "id", call.ID, "action", call.Action, "from", call.From.Machine,
			"change", appcall.Action{Name: call.Action, Args: call.Args}.Changes(), "err", ctx.Err())
		return wire.AppCallResult{}, ctx.Err()
	}
}

// Deliver gives the result of the source app to the waiting call. from is the caller of the result request.
func (r *Relay) Deliver(id string, res wire.AppCallResult, from server.Caller) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, at := range r.answered {
		if now.Sub(at) > answeredKeep {
			delete(r.answered, k)
		}
	}
	if _, ok := r.answered[id]; ok {
		return ErrAnswered
	}
	p, ok := r.pending[id]
	if !ok {
		return ErrUnknown
	}
	if !p.src.Match(from) {
		return ErrNotSource
	}
	res.ID = id
	r.answered[id] = now
	delete(r.pending, id)
	p.ch <- res
	return nil
}

// Routes gives the member-call routes. A client calls them on the home daemon. Only an app credential or loopback is let in.
func (r *Relay) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/member-calls", Access: server.Member, Handle: r.home(r.apps(r.serveCall))},
		{Pattern: "POST /v1/member-calls/{id}/result", Access: server.Member, Handle: r.home(r.apps(r.serveResult))},
	}
}

type errorBody struct {
	Error string `json:"error"`
}

type notHome struct {
	Error string `json:"error"`
	Home  string `json:"home,omitempty"`
}

func (r *Relay) home(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		g, ok := r.group.Snapshot()
		switch {
		case !ok:
			reply(w, http.StatusConflict, notHome{Error: "this machine has no group"})
		case g.Home != r.group.Self().Machine:
			reply(w, http.StatusConflict, notHome{Error: g.Home + " is home", Home: g.Home})
		default:
			next(w, req)
		}
	}
}

// apps refuses a daemon credential: only an app clicks and answers.
func (r *Relay) apps(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if c, ok := server.CallerFrom(req.Context()); ok && c.Kind == string(wire.DeviceDaemon) {
			reply(w, http.StatusForbidden, errorBody{Error: "only an app sends member calls"})
			return
		}
		next(w, req)
	}
}

// from is the sender of a request. The daemon takes it from the credential. A loopback caller has the machine of this daemon.
func (r *Relay) from(req *http.Request) wire.MemberCaller {
	c, _ := server.CallerFrom(req.Context())
	if c.Loopback {
		return wire.MemberCaller{Machine: r.self().Machine}
	}
	return wire.MemberCaller{Machine: c.Machine, Name: c.Name, Device: c.DeviceID}
}

// sourceName is the machine of the source app, for the texts of the errors.
func (r *Relay) sourceName() string {
	if src, ok := r.sources.Source(); ok && src.Machine != "" {
		return src.Machine
	}
	return r.self().Machine
}

func (r *Relay) serveCall(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, appcall.MaxCallBody+1))
	if len(body) > appcall.MaxCallBody {
		reply(w, http.StatusRequestEntityTooLarge, errorBody{Error: "The call is too large."})
		return
	}
	// A field "from" in the body is ignored: the credential tells who clicked.
	var in wire.MemberCallRequest
	if err == nil {
		err = json.Unmarshal(body, &in)
	}
	switch {
	case err != nil:
		reply(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	case in.Action == "":
		reply(w, http.StatusBadRequest, errorBody{Error: "no action"})
		return
	case len(in.Args) > 0 && !isObject(in.Args):
		reply(w, http.StatusBadRequest, errorBody{Error: "args must be a JSON object"})
		return
	}
	from := r.from(req)
	res, err := r.Do(req.Context(), wire.MemberCall{Action: in.Action, Args: in.Args, From: from})
	switch {
	case errors.Is(err, ErrNoSource):
		reply(w, http.StatusServiceUnavailable, errorBody{Error: "The OverSire app on " + r.sourceName() + " is not open."})
	case errors.Is(err, ErrBusy):
		reply(w, http.StatusTooManyRequests, errorBody{Error: r.sourceName() + " is busy. Try again."})
	case errors.Is(err, context.DeadlineExceeded):
		reply(w, http.StatusGatewayTimeout, errorBody{Error: appcall.Action{Name: in.Action, Args: in.Args}.TimeoutMessage(r.sourceName())})
	case err != nil:
		reply(w, http.StatusServiceUnavailable, errorBody{Error: err.Error()})
	default:
		reply(w, http.StatusOK, res)
	}
}

func (r *Relay) serveResult(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	var res wire.AppCallResult
	if err == nil {
		err = wire.Decode(body, &res)
	}
	if err != nil {
		reply(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	if res.ID != req.PathValue("id") {
		reply(w, http.StatusBadRequest, errorBody{Error: "the id of the body is not the id of the path"})
		return
	}
	c, _ := server.CallerFrom(req.Context())
	switch err := r.Deliver(res.ID, res, c); {
	case errors.Is(err, ErrUnknown):
		reply(w, http.StatusNotFound, errorBody{Error: err.Error()})
	case errors.Is(err, ErrNotSource):
		reply(w, http.StatusForbidden, errorBody{Error: "Only the source app answers."})
	case errors.Is(err, ErrAnswered):
		reply(w, http.StatusConflict, errorBody{Error: err.Error()})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func isObject(b json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

func newID() string {
	return "m-" + wire.RandomHex(8)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
