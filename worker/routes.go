package worker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const maxBody = 1 << 16

// Routes serves the worker API. Only the local CLI calls it, so each route is for loopback.
type Routes struct{ e *Engine }

func NewRoutes(e *Engine) *Routes { return &Routes{e: e} }

func (rt *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/workers/{id}/read", Access: server.LoopbackOnly, Handle: rt.read},
		{Pattern: "PUT /v1/workers/{id}/cursors/{reader}", Access: server.LoopbackOnly, Handle: rt.commit},
	}
}

func (rt *Routes) read(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "The limit is not a number."})
			return
		}
		limit = n
	}
	out, err := rt.e.Read(r.Context(), r.PathValue("id"), r.URL.Query().Get("reader"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (rt *Routes) commit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	var c Cursor
	if err == nil {
		err = wire.Decode(body, &c)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The body is not valid JSON: " + err.Error()})
		return
	}
	if err := rt.e.Commit(r.PathValue("id"), r.PathValue("reader"), c.Seq); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type errorBody struct {
	Error string `json:"error"`
}

func fail(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	status := map[Kind]int{NotFound: http.StatusNotFound, Invalid: http.StatusBadRequest, Conflict: http.StatusConflict}[e.Kind]
	writeJSON(w, status, errorBody{Error: e.Text})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
