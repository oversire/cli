package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/membercall"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	defaultRuns = 30
	maxRuns     = 200
	freshWait   = 10 * time.Second
)

var (
	ErrMissingCapability = errors.New("the CI provider lacks the capability")
	ErrTooSoon           = errors.New("a fresh read ran lately")
)

// capabilityError tells that the CI of a repo lacks a capability.
type capabilityError struct{ ci, capability string }

func (e capabilityError) Error() string      { return e.ci + " has no " + e.capability + "." }
func (capabilityError) Is(target error) bool { return target == ErrMissingCapability }

// tooSoonError tells that a fresh read of the same thing ran less than a window ago.
type tooSoonError struct {
	retry, window time.Duration
	what          string
}

func (e tooSoonError) wait() int { return int(math.Ceil(e.retry.Seconds())) }
func (e tooSoonError) Error() string {
	return fmt.Sprintf("Wait %d s. OverSire fetched this %s %d s ago.", e.wait(), e.what, int(e.window.Seconds())-e.wait())
}
func (tooSoonError) Is(target error) bool { return target == ErrTooSoon }

// appError is a result of the app that is not ok.
type appError struct {
	status  wire.AppCallStatus
	summary string
}

func (e *appError) Error() string { return e.summary }

// Caller sends a member call to the source app (membercall.Relay).
type Caller interface {
	Do(ctx context.Context, call wire.MemberCall) (wire.AppCallResult, error)
}

// app sends member calls to the source app.
type app struct {
	calls Caller
	self  func() wire.GroupMember
}

// call sends one member call to the source app and gives the data of an ok result.
func (a app) call(ctx context.Context, action string, args any) (json.RawMessage, error) {
	return a.callAs(ctx, action, args, "")
}

// callAs is call for an agent ("<harness> <session>"). An empty agent means a human.
func (a app) callAs(ctx context.Context, action string, args any, agent string) (json.RawMessage, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	res, err := a.calls.Do(ctx, wire.MemberCall{Action: action, Args: b, From: wire.MemberCaller{Machine: a.self().Machine}, CLI: &wire.MemberCLI{Agent: agent}})
	if err != nil {
		return nil, err
	}
	if res.Status != wire.AppCallOK {
		text := res.Summary
		if text == "" {
			text = "The OverSire app could not do " + action + "."
		}
		return nil, &appError{status: res.Status, summary: text}
	}
	return res.Data, nil
}

// Routes serves GET /v1/ci.
type Routes struct {
	app
	x    *Index
	logs *Logs
	lim  *Limiter
	fwd  *Forward

	freshWait time.Duration
}

func NewRoutes(x *Index, logs *Logs, calls Caller, lim *Limiter, fwd *Forward, self func() wire.GroupMember) *Routes {
	return &Routes{app: app{calls: calls, self: self}, x: x, logs: logs, lim: lim, fwd: fwd, freshWait: freshWait}
}

// Routes gives the routes. A member daemon sends each request to home.
func (r *Routes) Routes() []server.Route {
	routes := []server.Route{
		{Pattern: "GET /v1/ci/status", Access: server.Member, Handle: r.status},
		{Pattern: "GET /v1/ci/wait", Access: server.Member, Handle: r.wait},
		{Pattern: "GET /v1/ci/runs", Access: server.Member, Handle: r.runs},
		{Pattern: "GET /v1/ci/runs/{id}", Access: server.Member, Handle: r.run},
		{Pattern: "GET /v1/ci/runs/{id}/failures", Access: server.Member, Handle: r.failures},
		{Pattern: "GET /v1/ci/jobs/{id}/log", Access: server.Member, Handle: r.log},
		{Pattern: "GET /v1/ci/workflows", Access: server.Member, Handle: r.workflows},
	}
	for i := range routes {
		routes[i].Handle = r.fwd.Wrap(routes[i].Handle)
	}
	return routes
}

// query reads the parameters of a request.
type query struct{ url.Values }

func invalid(format string, a ...any) error { return server.Refuse(server.Invalid, format, a...) }

func (q query) repo() (string, error) { return checkRepo(q.Get("repo")) }

// checkRepo checks that repo is owner/name.
func checkRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) < 2 || slicesHasEmpty(parts) {
		return "", invalid("Give the repo as owner/name.")
	}
	return repo, nil
}

func slicesHasEmpty(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return true
		}
	}
	return false
}

// intIn reads an integer from lo to hi. A missing parameter gives def.
func (q query) intIn(name string, def, lo, hi int) (int, error) {
	s := q.Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, invalid("Give %s as a number from %d to %d.", name, lo, hi)
	}
	return n, nil
}

func (q query) flag(name string) bool {
	v := q.Get(name)
	return v == "1" || v == "true"
}

func (r *Routes) fail(w http.ResponseWriter, err error) { failWith(w, err, r.x.SourceMachine()) }

// failWith writes the status of an error. source is the machine of the source app.
func failWith(w http.ResponseWriter, err error, source string) {
	var soon tooSoonError
	var app *appError
	switch {
	case errors.Is(err, ErrUntracked):
		server.WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNoCIData):
		server.WriteError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrMissingCapability):
		server.WriteError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.As(err, &soon):
		w.Header().Set("Retry-After", strconv.Itoa(soon.wait()))
		server.WriteError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, membercall.ErrNoSource):
		server.WriteError(w, http.StatusServiceUnavailable, "The OverSire app on "+source+" is not open.")
	case errors.Is(err, membercall.ErrBusy):
		server.WriteError(w, http.StatusTooManyRequests, source+" is busy. Try again.")
	case errors.Is(err, context.DeadlineExceeded):
		server.WriteError(w, http.StatusGatewayTimeout, "The OverSire app on "+source+" did not answer in time. Try again.")
	case errors.As(err, &app) && app.status == wire.AppCallNotAllowed:
		server.WriteError(w, http.StatusUnprocessableEntity, app.summary)
	case errors.As(err, &app):
		server.WriteError(w, http.StatusBadGateway, app.summary)
	default:
		server.Fail(w, err)
	}
}

// limited runs fetch when the window of key is free. A fetch that fails frees the window.
func (r *Routes) limited(key, what string, fetch func() error) error {
	if retry, ok := r.lim.Allow(key); !ok {
		return tooSoonError{retry: retry, window: r.lim.Window(), what: what}
	}
	if err := fetch(); err != nil {
		r.lim.Forget(key)
		return err
	}
	return nil
}

// refresh asks the app to fetch the runs of the repo once. It then waits for the next change of the index.
func (r *Routes) refresh(ctx context.Context, repo string) error {
	return r.limited("refresh:"+repo, "repo", func() error {
		changed := r.x.Changed()
		if _, err := r.call(ctx, "ci.refresh", map[string]any{"repo": repo}); err != nil {
			return err
		}
		r.x.Wake()
		select {
		case <-changed:
		case <-time.After(r.freshWait):
		case <-ctx.Done():
		}
		return nil
	})
}

// fetchJobs asks the app for the jobs of one attempt.
func (r *Routes) fetchJobs(ctx context.Context, repo, run string, attempt int, fresh bool) (doc wire.CIJobsDoc, err error) {
	err = r.limited("run:"+repo+"/"+run, "run", func() error {
		data, err := r.call(ctx, "ci.jobs", map[string]any{"repo": repo, "runId": run, "attempt": attempt, "fresh": fresh})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return &appError{status: wire.AppCallFailed, summary: "The OverSire app sent no jobs for run " + run + "."}
		}
		return nil
	})
	return doc, err
}

func (r *Routes) status(w http.ResponseWriter, req *http.Request) {
	q := query{req.URL.Query()}
	repo, err := q.repo()
	if err != nil {
		r.fail(w, err)
		return
	}
	text := q.Get("ref")
	ref, err := ParseRef(text)
	if err != nil {
		r.fail(w, invalid("Give the ref."))
		return
	}
	st, err := r.x.Repo(repo)
	if err != nil {
		r.fail(w, err)
		return
	}
	if q.flag("fresh") {
		if err := r.refresh(req.Context(), st.Repo); err != nil {
			r.fail(w, err)
			return
		}
	}
	t, err := r.x.Resolve(st.Repo, ref)
	if err != nil {
		r.fail(w, err)
		return
	}
	roll, err := r.x.Rollup(st.Repo, t, nil)
	if err != nil {
		r.fail(w, err)
		return
	}
	roll.Ref = text
	server.WriteJSON(w, http.StatusOK, roll)
}

// runQuery reads the filters of a list of runs.
func (q query) runQuery() (rq RunQuery, err error) {
	if rq.Repo, err = q.repo(); err != nil {
		return rq, err
	}
	if rq.Limit, err = q.intIn("limit", defaultRuns, 1, maxRuns); err != nil {
		return rq, err
	}
	if rq.PR, err = q.intIn("pr", 0, 1, math.MaxInt32); err != nil {
		return rq, err
	}
	rq.Branch, rq.SHA, rq.Workflow = q.Get("branch"), q.Get("sha"), q.Get("workflow")
	switch rq.State = wire.CIRollupState(q.Get("state")); rq.State {
	case "", wire.CIPending, wire.CIRunning, wire.CISuccess, wire.CIFailure, wire.CICancelled:
		return rq, nil
	}
	return rq, invalid("Give state as pending, running, success, failure or cancelled.")
}

func (r *Routes) runs(w http.ResponseWriter, req *http.Request) {
	rq, err := query{req.URL.Query()}.runQuery()
	if err != nil {
		r.fail(w, err)
		return
	}
	page, err := r.x.Runs(rq)
	if err != nil {
		r.fail(w, err)
		return
	}
	server.WriteJSON(w, http.StatusOK, page)
}

func (r *Routes) run(w http.ResponseWriter, req *http.Request) {
	q := query{req.URL.Query()}
	repo, err := q.repo()
	if err != nil {
		r.fail(w, err)
		return
	}
	attempt, err := q.intIn("attempt", 0, 1, math.MaxInt32)
	if err != nil {
		r.fail(w, err)
		return
	}
	st, err := r.x.Repo(repo)
	if err != nil {
		r.fail(w, err)
		return
	}
	id := req.PathValue("id")
	ans, err := r.x.RunAnswer(st.Repo, id, attempt)
	if err != nil {
		r.fail(w, err)
		return
	}
	if _, has := r.x.JobsDoc(st.Repo, id, ans.Run.Attempt); !has || q.flag("fresh") {
		doc, err := r.fetchJobs(req.Context(), st.Repo, id, ans.Run.Attempt, q.flag("fresh"))
		if err != nil {
			r.fail(w, err)
			return
		}
		ans.Run.Jobs, ans.Run.Failures = doc.Jobs, doc.Failures
	}
	server.WriteJSON(w, http.StatusOK, ans)
}

func (r *Routes) failures(w http.ResponseWriter, req *http.Request) {
	q := query{req.URL.Query()}
	repo, err := q.repo()
	if err != nil {
		r.fail(w, err)
		return
	}
	attempt, err := q.intIn("attempt", 0, 1, math.MaxInt32)
	if err != nil {
		r.fail(w, err)
		return
	}
	id := req.PathValue("id")
	ans, err := r.x.Failures(repo, id, attempt)
	if errors.Is(err, ErrNoJobs) {
		var doc wire.CIJobsDoc
		if doc, err = r.fetchJobs(req.Context(), ans.Repo, id, ans.Attempt, false); err == nil {
			ans.Failures = doc.Failures
			if ans.Failures == nil {
				ans.Failures = []wire.CIFailureView{}
			}
		}
	}
	if err != nil {
		r.fail(w, err)
		return
	}
	server.WriteJSON(w, http.StatusOK, ans)
}

func (r *Routes) workflows(w http.ResponseWriter, req *http.Request) {
	q := query{req.URL.Query()}
	repo, err := q.repo()
	if err != nil {
		r.fail(w, err)
		return
	}
	st, err := r.x.Repo(repo)
	if err != nil {
		r.fail(w, err)
		return
	}
	wf, ok, err := r.x.book.Workflows(st.Repo)
	if err != nil {
		r.fail(w, err)
		return
	}
	if !ok || q.flag("fresh") {
		err = r.limited("workflows:"+st.Repo, "repo", func() error {
			data, err := r.call(req.Context(), "ci.workflows", map[string]any{"repo": st.Repo})
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &wf); err != nil {
				return &appError{status: wire.AppCallFailed, summary: "The OverSire app sent no workflows."}
			}
			wf.Repo = st.Repo
			return r.x.book.PutWorkflows(wf)
		})
		if err != nil {
			r.fail(w, err)
			return
		}
	}
	if wf.Workflows == nil {
		wf.Workflows = []wire.CIWorkflow{}
	}
	wf.CIFreshness = r.x.Freshness(st.Repo)
	server.WriteJSON(w, http.StatusOK, wf)
}

// waitParams are the parameters of GET /v1/ci/wait.
type waitParams struct {
	repo, ref, since string
	timeout, settle  time.Duration
	failFast         bool
	started          time.Time
	workflows        []string
}

func (q query) waitParams(now time.Time) (p waitParams, err error) {
	if p.repo, err = q.repo(); err != nil {
		return p, err
	}
	if p.ref = q.Get("ref"); p.ref == "" {
		return p, invalid("Give the ref.")
	}
	timeout, err := q.intIn("timeout", 60, 1, 300)
	if err != nil {
		return p, err
	}
	settle, err := q.intIn("settle", 60, 0, 600)
	if err != nil {
		return p, err
	}
	p.since, p.failFast, p.workflows = q.Get("since"), q.flag("failFast"), q.Values["workflow"]
	p.timeout, p.settle, p.started = time.Duration(timeout)*time.Second, time.Duration(settle)*time.Second, now
	if s := q.Get("started"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return p, invalid("Give started as Unix seconds.")
		}
		p.started = time.Unix(n, 0)
	}
	return p, nil
}

// wait holds the request until the rollup differs from since, is done, or the timeout ends. It holds no lock while it waits.
func (r *Routes) wait(w http.ResponseWriter, req *http.Request) {
	p, err := query{req.URL.Query()}.waitParams(r.x.now())
	if err != nil {
		r.fail(w, err)
		return
	}
	ref, err := ParseRef(p.ref)
	if err != nil {
		r.fail(w, invalid("Give the ref."))
		return
	}
	st, err := r.x.Repo(p.repo)
	if err != nil {
		r.fail(w, err)
		return
	}
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	for {
		changed := r.x.Changed()
		t, err := r.x.Resolve(st.Repo, ref)
		if err != nil {
			r.fail(w, err)
			return
		}
		roll, err := r.x.Rollup(st.Repo, t, p.workflows)
		if err != nil {
			r.fail(w, err)
			return
		}
		roll.Ref = p.ref
		settleAt := p.started.Add(p.settle)
		if roll.State == wire.CINone && r.x.now().After(settleAt) || p.failFast && roll.State == wire.CIFailure {
			roll.Done = true
		}
		if roll.Rev != p.since || roll.Done {
			server.WriteJSON(w, http.StatusOK, roll)
			return
		}
		var settled <-chan time.Time
		if roll.State == wire.CINone {
			timer := time.NewTimer(settleAt.Sub(r.x.now()) + time.Millisecond)
			defer timer.Stop()
			settled = timer.C
		}
		select {
		case <-changed:
		case <-settled:
		case <-deadline.C:
			server.WriteJSON(w, http.StatusOK, roll)
			return
		case <-req.Context().Done():
			return
		}
	}
}

func (r *Routes) log(w http.ResponseWriter, req *http.Request) {
	q := query{req.URL.Query()}
	repo, err := q.repo()
	var lines int
	if err == nil {
		lines, err = q.intIn("lines", defaultLines, 1, maxLogLines)
	}
	if err != nil {
		r.fail(w, err)
		return
	}
	view, err := r.logs.Get(req.Context(), repo, req.PathValue("id"), lines, q.flag("fresh"))
	if err != nil {
		r.fail(w, err)
		return
	}
	server.WriteJSON(w, http.StatusOK, view)
}
