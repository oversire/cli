package review

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const maxBody = 1 << 20

// Routes serves the reviews of the engine. The reads and the writes of the user are for members.
// The agent calls (reply, ready, plan) are only on loopback. A daemon of the group only reads.
type Routes struct{ engine *Engine }

func NewRoutes(e *Engine) *Routes { return &Routes{engine: e} }

type errorBody struct {
	Error string `json:"error"`
}

func (rt *Routes) Routes() []server.Route {
	member := func(p string, h http.HandlerFunc) server.Route {
		return server.Route{Pattern: p, Access: server.Member, Handle: h}
	}
	local := func(p string, h http.HandlerFunc) server.Route {
		return server.Route{Pattern: p, Access: server.LoopbackOnly, Handle: h}
	}
	return []server.Route{
		member("GET /v1/reviews", rt.list),
		member("GET /v1/reviews/{key}", rt.get),
		member("GET /v1/reviews/{key}/diff", rt.diff),
		member("POST /v1/reviews/{key}/comments", rt.app(rt.addComment)),
		member("PATCH /v1/reviews/{key}/comments/{id}", rt.app(rt.editComment)),
		member("DELETE /v1/reviews/{key}/comments/{id}", rt.app(rt.deleteComment)),
		member("PUT /v1/reviews/{key}/viewed", rt.app(rt.viewed)),
		member("POST /v1/reviews/{key}/submit", rt.app(rt.submit)),
		member("POST /v1/reviews/{key}/merge", rt.app(rt.merge)),
		member("POST /v1/reviews/{key}/fix", rt.app(rt.fix)),
		local("POST /v1/reviews/{key}/comments/{id}/reply", rt.reply),
		local("POST /v1/reviews/{key}/ready", rt.ready),
		local("PUT /v1/reviews/{key}/plan", rt.plan),
	}
}

// app refuses a daemon credential: only an app or loopback changes a review.
func (rt *Routes) app(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, ok := server.CallerFrom(r.Context()); ok && c.Kind == string(wire.DeviceDaemon) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "Only an app changes a review."})
			return
		}
		next(w, r)
	}
}

func pathKey(r *http.Request) string { return strings.ToUpper(r.PathValue("key")) }

func queryKind(r *http.Request) wire.ReviewKind { return wire.ReviewKind(r.URL.Query().Get("kind")) }

func (rt *Routes) list(w http.ResponseWriter, r *http.Request) {
	page, err := rt.engine.List(r.Context(), queryKind(r))
	reply(w, http.StatusOK, page, err)
}

func (rt *Routes) get(w http.ResponseWriter, r *http.Request) {
	d, err := rt.engine.Get(r.Context(), pathKey(r), queryKind(r))
	reply(w, http.StatusOK, d, err)
}

func (rt *Routes) diff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("repo") == "" || q.Get("path") == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "Give the repo and the path of the file."})
		return
	}
	d, err := rt.engine.Diff(r.Context(), pathKey(r), q.Get("repo"), q.Get("path"))
	reply(w, http.StatusOK, d, err)
}

func (rt *Routes) addComment(w http.ResponseWriter, r *http.Request) {
	var d wire.CommentDraft
	if !decode(w, r, &d) {
		return
	}
	c, err := rt.engine.AddComment(r.Context(), pathKey(r), d)
	reply(w, http.StatusCreated, c, err)
}

func (rt *Routes) editComment(w http.ResponseWriter, r *http.Request) {
	var ed wire.CommentEdit
	if !decode(w, r, &ed) {
		return
	}
	c, err := rt.engine.EditComment(pathKey(r), r.PathValue("id"), ed)
	reply(w, http.StatusOK, c, err)
}

func (rt *Routes) deleteComment(w http.ResponseWriter, r *http.Request) {
	none(w, rt.engine.DeleteComment(pathKey(r), r.PathValue("id"), queryKind(r)))
}

func (rt *Routes) viewed(w http.ResponseWriter, r *http.Request) {
	var m wire.ViewedMark
	if !decode(w, r, &m) {
		return
	}
	none(w, rt.engine.MarkViewed(pathKey(r), m))
}

func (rt *Routes) submit(w http.ResponseWriter, r *http.Request) {
	var s wire.SubmitReview
	if !decode(w, r, &s) {
		return
	}
	round, err := rt.engine.Submit(r.Context(), pathKey(r), s)
	reply(w, http.StatusOK, round, err)
}

func (rt *Routes) merge(w http.ResponseWriter, r *http.Request) {
	var m wire.MergeRequest
	if !decode(w, r, &m) {
		return
	}
	res, err := rt.engine.Merge(r.Context(), pathKey(r), m)
	reply(w, http.StatusOK, res, err)
}

func (rt *Routes) fix(w http.ResponseWriter, r *http.Request) {
	if k := queryKind(r); k != "" && k != wire.ReviewCode {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "Only a code review has a merge conflict to fix."})
		return
	}
	round, err := rt.engine.Fix(r.Context(), pathKey(r), nil)
	reply(w, http.StatusOK, round, err)
}

func (rt *Routes) reply(w http.ResponseWriter, r *http.Request) {
	var rp wire.CommentReply
	if !decode(w, r, &rp) {
		return
	}
	c, err := rt.engine.Reply(pathKey(r), r.PathValue("id"), rp)
	reply(w, http.StatusOK, c, err)
}

func (rt *Routes) ready(w http.ResponseWriter, r *http.Request) {
	var rd wire.ReviewReady
	if !decode(w, r, &rd) {
		return
	}
	s, err := rt.engine.Ready(r.Context(), pathKey(r), rd)
	reply(w, http.StatusOK, s, err)
}

func (rt *Routes) plan(w http.ResponseWriter, r *http.Request) {
	var p wire.PlanDoc
	if !decode(w, r, &p) {
		return
	}
	out, err := rt.engine.PutPlan(pathKey(r), p)
	reply(w, http.StatusOK, out, err)
}

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

func none(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reply[any](w, 0, nil, err)
}

func reply[T any](w http.ResponseWriter, code int, v T, err error) {
	var e *Error
	switch {
	case err == nil:
		writeJSON(w, code, v)
	case errors.As(err, &e):
		status := map[Kind]int{NotFound: http.StatusNotFound, Conflict: http.StatusConflict, Invalid: http.StatusBadRequest}[e.Kind]
		writeJSON(w, status, errorBody{Error: e.Text})
	default:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
