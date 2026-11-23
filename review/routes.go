package review

import (
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

func (rt *Routes) Routes() []server.Route {
	member := func(p string, h http.HandlerFunc) server.Route {
		return server.Route{Pattern: p, Access: server.Member, Handle: h}
	}
	local := func(p string, h http.HandlerFunc) server.Route {
		return server.Route{Pattern: p, Access: server.LoopbackOnly, Handle: h}
	}
	app := func(h http.HandlerFunc) http.HandlerFunc {
		return server.RefuseDaemon("Only an app changes a review.", h)
	}
	return []server.Route{
		member("GET /v1/reviews", rt.list),
		member("GET /v1/reviews/{key}", rt.get),
		member("GET /v1/reviews/{key}/diff", rt.diff),
		member("POST /v1/reviews/{key}/comments", app(rt.addComment)),
		member("PATCH /v1/reviews/{key}/comments/{id}", app(rt.editComment)),
		member("DELETE /v1/reviews/{key}/comments/{id}", app(rt.deleteComment)),
		member("PUT /v1/reviews/{key}/viewed", app(rt.viewed)),
		member("POST /v1/reviews/{key}/submit", app(rt.submit)),
		member("POST /v1/reviews/{key}/merge", app(rt.merge)),
		member("POST /v1/reviews/{key}/fix", app(rt.fix)),
		local("POST /v1/reviews/{key}/comments/{id}/reply", rt.reply),
		local("POST /v1/reviews/{key}/ready", rt.ready),
		local("PUT /v1/reviews/{key}/plan", rt.plan),
	}
}

func pathKey(r *http.Request) string { return strings.ToUpper(r.PathValue("key")) }

func queryKind(r *http.Request) wire.ReviewKind { return wire.ReviewKind(r.URL.Query().Get("kind")) }

func (rt *Routes) list(w http.ResponseWriter, r *http.Request) {
	page, err := rt.engine.List(r.Context(), queryKind(r), foldOf(r.URL.Query()))
	reply(w, http.StatusOK, page, err)
}

func (rt *Routes) get(w http.ResponseWriter, r *http.Request) {
	d, err := rt.engine.Get(r.Context(), pathKey(r), queryKind(r), foldOf(r.URL.Query()))
	reply(w, http.StatusOK, d, err)
}

func (rt *Routes) diff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("repo") == "" || q.Get("path") == "" {
		server.WriteError(w, http.StatusBadRequest, "Give the repo and the path of the file.")
		return
	}
	c := q.Get("context")
	if c != "" && c != "full" {
		server.WriteError(w, http.StatusBadRequest, "The context can only be full.")
		return
	}
	d, err := rt.engine.Diff(r.Context(), pathKey(r), q.Get("repo"), q.Get("path"), c == "full")
	reply(w, http.StatusOK, d, err)
}

func (rt *Routes) addComment(w http.ResponseWriter, r *http.Request) {
	var d wire.CommentDraft
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &d) {
		return
	}
	c, err := rt.engine.AddComment(r.Context(), pathKey(r), d)
	reply(w, http.StatusCreated, c, err)
}

func (rt *Routes) editComment(w http.ResponseWriter, r *http.Request) {
	var ed wire.CommentEdit
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &ed) {
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
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &m) {
		return
	}
	none(w, rt.engine.MarkViewed(r.Context(), pathKey(r), m))
}

func (rt *Routes) submit(w http.ResponseWriter, r *http.Request) {
	var s wire.SubmitReview
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &s) {
		return
	}
	round, err := rt.engine.Submit(r.Context(), pathKey(r), s)
	reply(w, http.StatusOK, round, err)
}

func (rt *Routes) merge(w http.ResponseWriter, r *http.Request) {
	var m wire.MergeRequest
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &m) {
		return
	}
	res, err := rt.engine.Merge(r.Context(), pathKey(r), m)
	reply(w, http.StatusOK, res, err)
}

func (rt *Routes) fix(w http.ResponseWriter, r *http.Request) {
	if k := queryKind(r); k != "" && k != wire.ReviewCode {
		server.WriteError(w, http.StatusBadRequest, "Only a code review has a merge conflict to fix.")
		return
	}
	round, err := rt.engine.Fix(r.Context(), pathKey(r), nil)
	reply(w, http.StatusOK, round, err)
}

func (rt *Routes) reply(w http.ResponseWriter, r *http.Request) {
	var rp wire.CommentReply
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &rp) {
		return
	}
	c, err := rt.engine.Reply(pathKey(r), r.PathValue("id"), rp)
	reply(w, http.StatusOK, c, err)
}

func (rt *Routes) ready(w http.ResponseWriter, r *http.Request) {
	var rd wire.ReviewReady
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &rd) {
		return
	}
	s, err := rt.engine.Ready(r.Context(), pathKey(r), rd)
	reply(w, http.StatusOK, s, err)
}

func (rt *Routes) plan(w http.ResponseWriter, r *http.Request) {
	var p wire.PlanDoc
	if !server.Decode(w, r, maxBody, "The body is not valid JSON: ", &p) {
		return
	}
	out, err := rt.engine.PutPlan(pathKey(r), p)
	reply(w, http.StatusOK, out, err)
}

func none(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reply[any](w, 0, nil, err)
}

func reply[T any](w http.ResponseWriter, code int, v T, err error) {
	if err != nil {
		server.Fail(w, err)
		return
	}
	server.WriteJSON(w, code, v)
}
