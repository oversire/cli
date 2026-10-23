package workers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func (b *Book) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/workers", Access: server.Member, Handle: b.serveStart},
		{Pattern: "GET /v1/workers/{id}", Access: server.Member, Handle: b.serveState},
		{Pattern: "POST /v1/workers/{id}/stop", Access: server.Member, Handle: b.serveStop},
		{Pattern: "DELETE /v1/workers/{id}", Access: server.Member, Handle: b.serveRelease},
	}
}

func (b *Book) serveStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var ws wire.WorkerStart
	if err == nil {
		err = wire.Decode(body, &ws)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	worker, err := b.Start(r.Context(), ws)
	if err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (b *Book) serveState(w http.ResponseWriter, r *http.Request) {
	st, err := b.State(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (b *Book) serveStop(w http.ResponseWriter, r *http.Request) {
	if err := b.Stop(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *Book) serveRelease(w http.ResponseWriter, r *http.Request) {
	if err := b.Release(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, status(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func status(err error) int {
	var ref *refused
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, runtime.ErrNoRuntime):
		return http.StatusNotFound
	case errors.Is(err, ErrBadStart):
		return http.StatusBadRequest
	case errors.As(err, &ref):
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
