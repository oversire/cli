package worker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const maxBody = 1 << 16

// Routes serves the worker API. Only the local CLI calls it, so each route is for loopback.
type Routes struct{ e *Engine }

func NewRoutes(e *Engine) *Routes { return &Routes{e: e} }

func (rt *Routes) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/workers", Access: server.LoopbackOnly, Handle: rt.start},
		{Pattern: "GET /v1/workers/{id}", Access: server.LoopbackOnly, Handle: rt.get},
		{Pattern: "POST /v1/workers/{id}/done", Access: server.LoopbackOnly, Handle: rt.done},
		{Pattern: "POST /v1/workers/{id}/escalate", Access: server.LoopbackOnly, Handle: rt.escalate},
		{Pattern: "POST /v1/workers/{id}/send", Access: server.LoopbackOnly, Handle: rt.send},
		{Pattern: "POST /v1/workers/{id}/answer", Access: server.LoopbackOnly, Handle: rt.answer},
		{Pattern: "POST /v1/workers/{id}/interrupt", Access: server.LoopbackOnly, Handle: rt.interrupt},
		{Pattern: "POST /v1/workers/{id}/stop", Access: server.LoopbackOnly, Handle: rt.stop},
		{Pattern: "GET /v1/workers/{id}/read", Access: server.LoopbackOnly, Handle: rt.read},
		{Pattern: "GET /v1/workers/{id}/wait", Access: server.LoopbackOnly, Handle: rt.wait},
		{Pattern: "GET /v1/workers/{id}/brief", Access: server.LoopbackOnly, Handle: rt.brief},
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

func (rt *Routes) wait(w http.ResponseWriter, r *http.Request) {
	max := MaxWait
	if raw := r.URL.Query().Get("max"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "The max is not a duration."})
			return
		}
		max = d
	}
	out, err := rt.e.Wait(r.Context(), r.PathValue("id"), max)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (rt *Routes) brief(w http.ResponseWriter, r *http.Request) {
	out, err := rt.e.Brief(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// decode reads the JSON body into v. It answers 400 and gives false when the body is bad.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err == nil {
		err = wire.Decode(body, v)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The body is not valid JSON: " + err.Error()})
		return false
	}
	return true
}

// reply answers 204 for a call without a result, else the error.
func reply(w http.ResponseWriter, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (rt *Routes) start(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if !decode(w, r, &req) {
		return
	}
	rec, reused, err := rt.e.Start(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Started{Record: rec, Reused: reused})
}

func (rt *Routes) get(w http.ResponseWriter, r *http.Request) {
	rec, err := rt.e.Get(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (rt *Routes) done(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &b) {
		return
	}
	rec, err := rt.e.Done(r.Context(), r.PathValue("id"), b.Verdict, b.Note)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (rt *Routes) escalate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Question string `json:"question"`
	}
	if !decode(w, r, &b) {
		return
	}
	reply(w, rt.e.Escalate(r.Context(), r.PathValue("id"), b.Question, ReasonOverseer))
}

func (rt *Routes) send(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &b) {
		return
	}
	reply(w, rt.e.Send(r.Context(), r.PathValue("id"), b.Text))
}

func (rt *Routes) answer(w http.ResponseWriter, r *http.Request) {
	var a Answer
	if !decode(w, r, &a) {
		return
	}
	reply(w, rt.e.Answer(r.Context(), r.PathValue("id"), a))
}

func (rt *Routes) interrupt(w http.ResponseWriter, r *http.Request) {
	reply(w, rt.e.Interrupt(r.Context(), r.PathValue("id")))
}

func (rt *Routes) stop(w http.ResponseWriter, r *http.Request) {
	reply(w, rt.e.Stop(r.Context(), r.PathValue("id")))
}

func (rt *Routes) commit(w http.ResponseWriter, r *http.Request) {
	var c Cursor
	if !decode(w, r, &c) {
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
