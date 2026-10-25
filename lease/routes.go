package lease

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Group gives the roster of this machine (group.Store).
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

// Routes serves the leases on the home member of the group, also when the group has a cloud.
type Routes struct {
	book  *Book
	group Group
}

func NewRoutes(book *Book, group Group) *Routes { return &Routes{book: book, group: group} }

// notHome is the 409 body when this daemon is not home, as in the store routes. The app asks Home.
type notHome struct {
	Error string `json:"error"`
	Home  string `json:"home,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
}

func (r *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/leases/{name}", Access: server.Member, Handle: r.home(r.acquire)},
		{Pattern: "DELETE /v1/leases/{name}", Access: server.Member, Handle: r.home(r.release)},
		{Pattern: "GET /v1/leases/{name}", Access: server.Member, Handle: r.home(r.get)},
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

func (r *Routes) acquire(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<16))
	var lr wire.LeaseRequest
	if err == nil {
		err = wire.Decode(body, &lr)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "bad json: " + err.Error()})
		return
	}
	l, err := r.book.Acquire(req.PathValue("name"), lr)
	switch {
	case errors.Is(err, ErrHeld):
		writeJSON(w, http.StatusConflict, l)
	case errors.Is(err, ErrBadInput):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	default:
		writeJSON(w, http.StatusOK, l)
	}
}

func (r *Routes) release(w http.ResponseWriter, req *http.Request) {
	err := r.book.Release(req.PathValue("name"), req.URL.Query().Get("holder"))
	switch {
	case errors.Is(err, ErrNotHolder):
		writeJSON(w, http.StatusForbidden, errorBody{Error: err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (r *Routes) get(w http.ResponseWriter, req *http.Request) {
	l, ok := r.book.Get(req.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "nobody holds the lease"})
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
