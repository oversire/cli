package mirror

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// LeaseAutomations is the lease that the source of the mirror holds.
const LeaseAutomations = "automations"

// maxBody is the most bytes of a put request: MaxBatchBytes and the room for the JSON around the bodies.
const maxBody = MaxBatchBytes + 2<<20

// Holder gives the holder of a lease. It is false for a lease that is free or expired (lease.Book).
type Holder interface {
	Get(name string) (wire.Lease, bool)
}

// Group gives the roster of this machine (group.Store). It is the same as lease.Group.
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

// Routes serves the mirror on the home member of the group, also when the group has a cloud.
type Routes struct {
	store  *Store
	group  Group
	holder Holder
	self   func() wire.GroupMember
	online func(Source) bool
	notify func(msg any)
}

// NewRoutes makes the mirror routes. self gives this machine, for a loopback caller. online tells if a WebSocket client
// of the source is connected. notify sends a frame to the WebSocket clients (server.Server.Notify).
func NewRoutes(s *Store, g Group, h Holder, self func() wire.GroupMember, online func(Source) bool, notify func(msg any)) *Routes {
	if online == nil {
		online = func(Source) bool { return false }
	}
	if notify == nil {
		notify = func(any) {}
	}
	return &Routes{store: s, group: g, holder: h, self: self, online: online, notify: notify}
}

type notHome struct {
	Error string `json:"error"`
	Home  string `json:"home,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
}

func (r *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/mirror", Access: server.Member, Handle: r.home(r.head)},
		{Pattern: "GET /v1/mirror/docs", Access: server.Member, Handle: r.home(r.page)},
		{Pattern: "GET /v1/mirror/index", Access: server.Member, Handle: r.home(r.apps(r.index))},
		{Pattern: "PUT /v1/mirror/docs", Access: server.Member, Handle: r.home(r.apps(r.put))},
	}
}

func (r *Routes) home(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		g, ok := r.group.Snapshot()
		switch {
		case !ok:
			writeJSON(w, http.StatusConflict, notHome{Error: "this machine has no group"})
		case g.Home != r.group.Self().Machine:
			writeJSON(w, http.StatusConflict, notHome{Error: g.Home + " is home", Home: g.Home})
		default:
			next(w, req)
		}
	}
}

// apps refuses a daemon credential: only an app writes the mirror and reads its index.
func (r *Routes) apps(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if c, ok := server.CallerFrom(req.Context()); ok && c.Kind == string(wire.DeviceDaemon) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "only an app writes the mirror"})
			return
		}
		next(w, req)
	}
}

// caller is the sender of a request. A loopback caller has the machine of this daemon.
func (r *Routes) caller(req *http.Request) Source {
	c, _ := server.CallerFrom(req.Context())
	src := Source{Device: c.DeviceID, Name: c.Name, Machine: c.Machine, Loopback: c.Loopback}
	if c.Loopback {
		src.Machine = r.self().Machine
	}
	return src
}

// seen records the reader of a request. head < 0 keeps the head that the store has.
func (r *Routes) seen(req *http.Request, head int64) {
	c := r.caller(req)
	r.store.Seen(wire.MirrorReader{Machine: c.Machine, Name: c.Name, Device: c.Device, Head: head})
}

func (r *Routes) withOnline(h wire.MirrorHead) wire.MirrorHead {
	if src, ok := r.store.Source(); ok {
		h.SourceOnline = r.online(src)
	}
	return h
}

func (r *Routes) head(w http.ResponseWriter, req *http.Request) {
	// peek=1 reads the head without a reader record (the Machines page).
	if req.URL.Query().Get("peek") != "1" {
		r.seen(req, -1)
	}
	writeJSON(w, http.StatusOK, r.withOnline(r.store.Head()))
}

func (r *Routes) page(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	p, err := r.store.Page(after, limit, q.Get("epoch"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	if p.Reset {
		r.seen(req, 0)
	} else {
		r.seen(req, p.Next)
	}
	if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		writeJSON(w, http.StatusOK, p)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusOK)
	zw := gzip.NewWriter(w)
	_ = json.NewEncoder(zw).Encode(p)
	_ = zw.Close()
}

func (r *Routes) index(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, r.store.Index())
}

func (r *Routes) put(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	switch {
	case err != nil:
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "bad json"})
		return
	case len(body) > maxBody:
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "The batch is too large."})
		return
	}
	var p wire.MirrorPut
	if err := wire.Decode(body, &p); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "bad json: " + err.Error()})
		return
	}
	if l, ok := r.holder.Get(LeaseAutomations); !ok || l.Holder != p.Source {
		writeJSON(w, http.StatusForbidden, errorBody{Error: p.SourceName + " does not hold the automations lease."})
		return
	}
	was := r.store.Head().Ready
	res, err := r.store.Put(p, r.caller(req))
	switch {
	case errors.Is(err, ErrEpoch):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "The mirror changed. Send all again.", "epoch": r.store.Head().Epoch})
		return
	case errors.Is(err, ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "The batch is too large."})
		return
	case errors.Is(err, ErrBadDoc):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	if res.Applied > 0 || res.Head.Ready && !was {
		r.notify(wire.MirrorFrame{Type: wire.FrameMirror, Head: res.Head.Head, Epoch: res.Head.Epoch})
	}
	res.Head = r.withOnline(res.Head)
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
