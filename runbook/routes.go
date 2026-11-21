package runbook

import (
	"errors"
	"net/http"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const maxBody = 1 << 20

// Routes serves the runbook runs of the engine. All routes are for members. A daemon of the group only reads.
type Routes struct{ engine *Engine }

func NewRoutes(e *Engine) *Routes { return &Routes{engine: e} }

// errorBody is the body of each refused call. Problems are only in a 400 for a runbook that is not valid.
type errorBody struct {
	Error    string    `json:"error"`
	Problems []Problem `json:"problems,omitempty"`
}

func (rt *Routes) Routes() []server.Route {
	member := func(p string, h http.HandlerFunc) server.Route {
		return server.Route{Pattern: p, Access: server.Member, Handle: h}
	}
	// A daemon of the group only reads: only an app or loopback changes a run.
	app := func(h http.HandlerFunc) http.HandlerFunc {
		return server.RefuseDaemon("Only an app changes a runbook run.", h)
	}
	return []server.Route{
		member("POST /v1/runbook-runs", app(rt.start)),
		member("GET /v1/runbook-runs", rt.list),
		member("GET /v1/runbook-runs/{id}", rt.get),
		member("POST /v1/runbook-runs/{id}/approve", app(rt.run(rt.engine.Approve))),
		member("POST /v1/runbook-runs/{id}/pause", app(rt.run(rt.engine.Pause))),
		member("POST /v1/runbook-runs/{id}/resume", app(rt.run(rt.engine.Resume))),
		member("POST /v1/runbook-runs/{id}/stop", app(rt.stop)),
		member("POST /v1/runbook-runs/{id}/steps/{step}/done", app(rt.done)),
		member("POST /v1/runbook-runs/{id}/steps/{step}/retry", app(rt.step(rt.engine.Retry))),
		member("POST /v1/runbook-runs/{id}/steps/{step}/skip", app(rt.step(rt.engine.Skip))),
		member("PATCH /v1/runbook-runs/{id}/steps/{step}", app(rt.patch)),
	}
}

func (rt *Routes) start(w http.ResponseWriter, r *http.Request) {
	var s wire.RunbookStart
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &s) {
		return
	}
	run, err := rt.engine.Start(r.Context(), s)
	reply(w, http.StatusCreated, run, err)
}

func (rt *Routes) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	open := q.Get("open") == "1" || q.Get("open") == "true"
	out := []wire.RunbookRun{}
	for _, run := range rt.engine.List(q.Get("item")) {
		if !open || run.State.Open() {
			out = append(out, run)
		}
	}
	server.WriteJSON(w, http.StatusOK, out)
}

func (rt *Routes) get(w http.ResponseWriter, r *http.Request) {
	run, ok := rt.engine.Get(r.PathValue("id"))
	if !ok {
		server.WriteError(w, http.StatusNotFound, "There is no run "+r.PathValue("id")+".")
		return
	}
	server.WriteJSON(w, http.StatusOK, run)
}

func (rt *Routes) run(f func(string) (wire.RunbookRun, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, err := f(r.PathValue("id"))
		reply(w, http.StatusOK, run, err)
	}
}

func (rt *Routes) stop(w http.ResponseWriter, r *http.Request) {
	run, err := rt.engine.Stop(r.Context(), r.PathValue("id"))
	reply(w, http.StatusOK, run, err)
}

func (rt *Routes) step(f func(run, step string) (wire.RunbookRun, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, err := f(r.PathValue("id"), r.PathValue("step"))
		reply(w, http.StatusOK, run, err)
	}
}

func (rt *Routes) done(w http.ResponseWriter, r *http.Request) {
	var d wire.StepDone
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &d) {
		return
	}
	run, err := rt.engine.Done(r.PathValue("id"), r.PathValue("step"), d)
	reply(w, http.StatusOK, run, err)
}

func (rt *Routes) patch(w http.ResponseWriter, r *http.Request) {
	var p wire.StepPatch
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &p) {
		return
	}
	run, err := rt.engine.Patch(r.PathValue("id"), r.PathValue("step"), p)
	reply(w, http.StatusOK, run, err)
}

func reply(w http.ResponseWriter, code int, run wire.RunbookRun, err error) {
	var e *Error
	switch {
	case err == nil:
		server.WriteJSON(w, code, run)
	case errors.As(err, &e):
		server.WriteJSON(w, e.Status(), errorBody{e.Text, e.Problems})
	default:
		server.Fail(w, err)
	}
}
