package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/ci"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const ciUsage = `usage:
  oversire ci status [REF] [--repo R] [--fresh] [--json]
  oversire ci ls [--repo R] [--branch B] [--sha S] [--pr N] [--workflow W] [--state S] [--limit N] [--json]
  oversire ci show RUN [--repo R] [--jobs] [--attempt N] [--fresh] [--json]
  oversire ci why RUN [--repo R] [--json]
  oversire ci explain RUN [--repo R] [--attempt N] [--job J] [--force] [--json]
  oversire ci logs RUN|JOB [--repo R] [--failed] [--lines N] [--fresh] [--json]
  oversire ci workflows [--repo R] [--fresh] [--json]
  oversire ci wait [REF] [--repo R] [--timeout 30m] [--fail-fast] [--settle 60s] [--workflow W]... [--log-lines 40] [--json|--events]
REF is a SHA, a branch, #PR, a PR URL or a run URL. The default is HEAD. R is owner/name; the default is the origin of the git folder.
Exit codes of wait: 0 success, 1 failed or cancelled, 2 usage, 3 no daemon or no app, 4 timeout, 5 no runs.`

// Exit codes of ci wait.
const (
	exitCIFailed     = 1
	exitCIUsage      = 2
	exitCINoSource   = 3
	exitCITimeout    = 4
	exitCINoRuns     = 5
	maxCIPoll        = 300 * time.Second
	ciHTTPTimeout    = 330 * time.Second
	defaultCILogs    = 40
	defaultCITimeout = 30 * time.Minute
)

// ciStaleGrace is how long wait accepts stale answers before it exits with 3.
var ciStaleGrace = 2 * time.Minute

// ciStalePoll is the poll time of wait while the answers are stale.
var ciStalePoll = 30 * time.Second

func ciUsageErr() error { return &exitError{exitCIUsage, errors.New(ciUsage)} }

// ci reads the CI state from the daemon.
func (c *cli) ci(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ciUsageErr()
	}
	rest := args[1:]
	switch args[0] {
	case "status":
		return c.ciStatus(ctx, rest)
	case "ls":
		return c.ciLs(ctx, rest)
	case "show":
		return c.ciShow(ctx, rest)
	case "why":
		return c.ciWhy(ctx, rest)
	case "explain":
		return c.ciExplain(ctx, rest)
	case "logs":
		return c.ciLogs(ctx, rest)
	case "workflows":
		return c.ciWorkflows(ctx, rest)
	case "wait":
		return c.ciWait(ctx, rest)
	}
	return ciUsageErr()
}

// ciClient calls the CI routes of the daemon. Its HTTP client waits longer than a long poll.
type ciClient struct{ d daemonClient }

func newCIClient(d daemonClient) ciClient {
	d.http = &http.Client{Timeout: ciHTTPTimeout}
	return ciClient{d}
}

// get calls a CI route. A daemon that does not answer, or an app that is not open, gives exit 3.
func (cl ciClient) get(ctx context.Context, path string, q url.Values, out any) error {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := cl.d.call(ctx, http.MethodGet, path, nil, out)
	var de *daemonError
	switch {
	case errors.Is(err, ticket.ErrNoDaemon):
		return &exitError{exitCINoSource, err}
	case errors.As(err, &de) && de.Status == http.StatusServiceUnavailable:
		return &exitError{exitCINoSource, err}
	}
	return err
}

// post calls a CI route with a body. A 504 gives exit 4.
func (cl ciClient) post(ctx context.Context, path string, body, out any) error {
	err := cl.d.call(ctx, http.MethodPost, path, body, out)
	var de *daemonError
	switch {
	case errors.Is(err, ticket.ErrNoDaemon):
		return &exitError{exitCINoSource, err}
	case errors.As(err, &de) && de.Status == http.StatusServiceUnavailable:
		return &exitError{exitCINoSource, err}
	case errors.As(err, &de) && de.Status == http.StatusGatewayTimeout:
		return &exitError{exitCITimeout, err}
	}
	return err
}

func (cl ciClient) Explain(ctx context.Context, id string, req wire.CIExplainRequest) (e wire.CIExplanation, err error) {
	err = cl.post(ctx, "/v1/ci/runs/"+url.PathEscape(id)+"/explain", req, &e)
	return e, err
}

func repoQuery(repo string, fresh bool) url.Values {
	q := url.Values{"repo": {repo}}
	if fresh {
		q.Set("fresh", "1")
	}
	return q
}

func (cl ciClient) Status(ctx context.Context, repo, ref string, fresh bool) (r wire.CIRollup, err error) {
	q := repoQuery(repo, fresh)
	q.Set("ref", ref)
	err = cl.get(ctx, "/v1/ci/status", q, &r)
	return r, err
}

func (cl ciClient) Runs(ctx context.Context, q url.Values) (p wire.CIRunsPage, err error) {
	err = cl.get(ctx, "/v1/ci/runs", q, &p)
	return p, err
}

func (cl ciClient) Run(ctx context.Context, repo, id string, attempt int, fresh bool) (a wire.CIRunAnswer, err error) {
	q := repoQuery(repo, fresh)
	if attempt > 0 {
		q.Set("attempt", strconv.Itoa(attempt))
	}
	err = cl.get(ctx, "/v1/ci/runs/"+url.PathEscape(id), q, &a)
	return a, err
}

func (cl ciClient) Failures(ctx context.Context, repo, id string) (a wire.CIFailuresAnswer, err error) {
	err = cl.get(ctx, "/v1/ci/runs/"+url.PathEscape(id)+"/failures", repoQuery(repo, false), &a)
	return a, err
}

func (cl ciClient) Log(ctx context.Context, repo, job string, lines int, fresh bool) (v wire.CILogView, err error) {
	q := repoQuery(repo, fresh)
	if lines > 0 {
		q.Set("lines", strconv.Itoa(lines))
	}
	err = cl.get(ctx, "/v1/ci/jobs/"+url.PathEscape(job)+"/log", q, &v)
	return v, err
}

func (cl ciClient) Workflows(ctx context.Context, repo string, fresh bool) (w wire.CIWorkflows, err error) {
	err = cl.get(ctx, "/v1/ci/workflows", repoQuery(repo, fresh), &w)
	return w, err
}

// waitQuery holds the parameters of one long poll.
type waitQuery struct {
	Repo, Ref, Since string
	Timeout, Settle  time.Duration
	FailFast         bool
	Started          time.Time
	Workflows        []string
}

func (cl ciClient) Wait(ctx context.Context, w waitQuery) (r wire.CIRollup, err error) {
	q := url.Values{"repo": {w.Repo}, "ref": {w.Ref},
		"timeout": {strconv.Itoa(int(w.Timeout / time.Second))}, "settle": {strconv.Itoa(int(w.Settle / time.Second))},
		"started": {strconv.FormatInt(w.Started.Unix(), 10)}}
	if w.Since != "" {
		q.Set("since", w.Since)
	}
	if w.FailFast {
		q.Set("failFast", "1")
	}
	for _, name := range w.Workflows {
		q.Add("workflow", name)
	}
	err = cl.get(ctx, "/v1/ci/wait", q, &r)
	return r, err
}

// gitDir reads the repo and the commit of a git folder. run is injected for tests.
type gitDir struct {
	dir string
	run func(ctx context.Context, dir string, args ...string) (string, error)
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// Repo gives owner/name from the origin remote.
func (g gitDir) Repo(ctx context.Context) (string, error) {
	out, err := g.run(ctx, g.dir, "remote", "get-url", "origin")
	if repo, ok := parseRemote(out); err == nil && ok {
		return repo, nil
	}
	return "", errors.New("Cannot read the repo from the origin remote. Give it with --repo owner/name.")
}

// Head gives the SHA of HEAD.
func (g gitDir) Head(ctx context.Context) (string, error) {
	out, err := g.run(ctx, g.dir, "rev-parse", "HEAD")
	if err != nil || out == "" {
		return "", errors.New("Cannot read HEAD. Give the ref.")
	}
	return out, nil
}

var scpRemote = regexp.MustCompile(`^(?:[^@/\s]+@)?[^/:\s]+:(.+)$`)

// parseRemote gives owner/name from a git remote URL. A GitLab subgroup path stays whole.
func parseRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" || u.Scheme == "file" {
			return "", false
		}
		path = u.Path
	} else if m := scpRemote.FindStringSubmatch(remote); m != nil {
		path = m[1]
	} else {
		return "", false
	}
	path = strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", false
		}
	}
	return path, true
}

func (c *cli) ciFlags(name string, args []string, setup func(fs *flag.FlagSet)) (*flag.FlagSet, *string, *bool, []string, error) {
	fs := flag.NewFlagSet("ci "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "owner/name")
	asJSON := fs.Bool("json", false, "print JSON")
	if setup != nil {
		setup(fs)
	}
	pos, err := workerParse(fs, args)
	return fs, repo, asJSON, pos, err
}

// ciRepo gives the repo of --repo, or of the git folder. A value account:path gives path.
func (c *cli) ciRepo(ctx context.Context, flagRepo string) (string, error) {
	if flagRepo != "" {
		if i := strings.Index(flagRepo, ":"); i >= 0 && !strings.Contains(flagRepo[:i], "/") {
			flagRepo = flagRepo[i+1:]
		}
		if !strings.Contains(flagRepo, "/") {
			return "", &exitError{exitCIUsage, errors.New("Give the repo as owner/name.")}
		}
		return flagRepo, nil
	}
	return c.gitDir().Repo(ctx)
}

func (c *cli) gitDir() gitDir {
	wd, _ := c.workdir()
	run := c.git
	if run == nil {
		run = runGit
	}
	return gitDir{dir: wd, run: run}
}

// ciRef gives the ref argument, or the SHA of HEAD.
func (c *cli) ciRef(ctx context.Context, pos []string) (string, error) {
	switch len(pos) {
	case 0:
		return c.gitDir().Head(ctx)
	case 1:
		return pos[0], nil
	}
	return "", ciUsageErr()
}

var (
	runIDRe = regexp.MustCompile(`^[0-9]+$`)
	jobURL  = regexp.MustCompile(`/job/([0-9]+)`)
)

// ciRunID reads a run id or a run URL.
func ciRunID(s string) (string, error) {
	r, err := ci.ParseRef(s)
	if err != nil || r.Kind != ci.RefRun {
		return "", &exitError{exitCIUsage, errors.New("Give a run id or a run URL.")}
	}
	return r.Value, nil
}

func ciJobID(s string) (string, error) {
	if m := jobURL.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if runIDRe.MatchString(s) {
		return s, nil
	}
	return "", &exitError{exitCIUsage, errors.New("Give a job id or a job URL.")}
}

// ciSetup gives the client and the repo of a command.
func (c *cli) ciSetup(ctx context.Context, flagRepo string) (ciClient, string, error) {
	repo, err := c.ciRepo(ctx, flagRepo)
	if err != nil {
		return ciClient{}, "", err
	}
	d, err := c.daemonClient()
	if err != nil {
		return ciClient{}, "", err
	}
	return newCIClient(d), repo, nil
}

func (c *cli) ciJSON(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *cli) ciPrinter() ciPrinter { return ciPrinter{w: c.out, now: time.Now} }

func (c *cli) ciStatus(ctx context.Context, args []string) error {
	var fresh *bool
	_, repoFlag, asJSON, pos, err := c.ciFlags("status", args, func(fs *flag.FlagSet) { fresh = fs.Bool("fresh", false, "ask the app to fetch now") })
	if err != nil {
		return &exitError{exitCIUsage, err}
	}
	ref, err := c.ciRef(ctx, pos)
	if err != nil {
		return err
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	r, err := cl.Status(ctx, repo, ref, *fresh)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.ciJSON(r)
	}
	return c.ciPrinter().Rollup(r)
}

func (c *cli) ciLs(ctx context.Context, args []string) error {
	var branch, sha, workflow, state *string
	var pr, limit *int
	_, repoFlag, asJSON, pos, err := c.ciFlags("ls", args, func(fs *flag.FlagSet) {
		branch, sha = fs.String("branch", "", "branch"), fs.String("sha", "", "commit SHA")
		workflow, state = fs.String("workflow", "", "workflow name"), fs.String("state", "", "pending, running, success, failure or cancelled")
		pr, limit = fs.Int("pr", 0, "PR number"), fs.Int("limit", 0, "the most runs")
	})
	if err != nil || len(pos) > 0 {
		return ciUsageErr()
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	q := url.Values{"repo": {repo}}
	for k, v := range map[string]string{"branch": *branch, "sha": *sha, "workflow": *workflow, "state": *state} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if *pr > 0 {
		q.Set("pr", strconv.Itoa(*pr))
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	p, err := cl.Runs(ctx, q)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.ciJSON(p)
	}
	return c.ciPrinter().Runs(p)
}

func (c *cli) ciShow(ctx context.Context, args []string) error {
	var jobs, fresh *bool
	var attempt *int
	_, repoFlag, asJSON, pos, err := c.ciFlags("show", args, func(fs *flag.FlagSet) {
		jobs, fresh = fs.Bool("jobs", false, "print the jobs"), fs.Bool("fresh", false, "ask the app to fetch now")
		attempt = fs.Int("attempt", 0, "the attempt")
	})
	if err != nil || len(pos) != 1 {
		return ciUsageErr()
	}
	id, err := ciRunID(pos[0])
	if err != nil {
		return err
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	a, err := cl.Run(ctx, repo, id, *attempt, *fresh)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.ciJSON(a)
	}
	return c.ciPrinter().Run(a, repo, *jobs)
}

func (c *cli) ciWhy(ctx context.Context, args []string) error {
	_, repoFlag, asJSON, pos, err := c.ciFlags("why", args, nil)
	if err != nil || len(pos) != 1 {
		return ciUsageErr()
	}
	id, err := ciRunID(pos[0])
	if err != nil {
		return err
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	a, err := cl.Failures(ctx, repo, id)
	if err != nil {
		return err
	}
	a.Failures = cl.withTails(ctx, repo, a.Failures, defaultCILogs)
	if *asJSON {
		return c.ciJSON(a)
	}
	return c.ciPrinter().Failures(a, repo)
}

func (c *cli) ciExplain(ctx context.Context, args []string) error {
	f := newAppFlags("ci explain")
	repoFlag := f.fs.String("repo", "", "owner/name")
	attempt := f.fs.Int("attempt", 0, "the attempt")
	job := f.fs.String("job", "", "a job id")
	force := f.fs.Bool("force", false, "ask the AI engine again")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || *attempt < 0 {
		return ciUsageErr()
	}
	id, err := ciRunID(pos[0])
	if err != nil {
		return err
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	e, err := cl.Explain(ctx, id, wire.CIExplainRequest{Repo: repo, Attempt: *attempt, Job: *job, Force: *force, Agent: f.agent()})
	if err != nil {
		return err
	}
	if *f.json {
		return c.ciJSON(e)
	}
	return c.ciPrinter().Explanation(e)
}

func (c *cli) ciLogs(ctx context.Context, args []string) error {
	var failed, fresh *bool
	var lines *int
	_, repoFlag, asJSON, pos, err := c.ciFlags("logs", args, func(fs *flag.FlagSet) {
		failed, fresh = fs.Bool("failed", false, "the argument is a run; print the log of each failed job"), fs.Bool("fresh", false, "ask the app to fetch now")
		lines = fs.Int("lines", 0, "the last N lines; 0 is the daemon default")
	})
	if err != nil || len(pos) != 1 || *lines < 0 {
		return ciUsageErr()
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	var views []wire.CILogView
	if *failed {
		id, err := ciRunID(pos[0])
		if err != nil {
			return err
		}
		a, err := cl.Failures(ctx, repo, id)
		if err != nil {
			return err
		}
		for _, f := range a.Failures {
			v, err := cl.Log(ctx, repo, f.JobID, *lines, *fresh)
			if err != nil {
				return err
			}
			views = append(views, v)
		}
		if len(views) == 0 {
			fmt.Fprintf(c.out, "Run %s has no failed job.\n", id)
		}
	} else {
		id, err := ciJobID(pos[0])
		if err != nil {
			return err
		}
		v, err := cl.Log(ctx, repo, id, *lines, *fresh)
		if err != nil {
			return err
		}
		views = append(views, v)
	}
	if *asJSON {
		return c.ciJSON(views)
	}
	for _, v := range views {
		if *failed {
			fmt.Fprintf(c.out, "== job %s ==\n", v.JobID)
		}
		for _, l := range v.Lines {
			fmt.Fprintln(c.out, l)
		}
	}
	return nil
}

func (c *cli) ciWorkflows(ctx context.Context, args []string) error {
	var fresh *bool
	_, repoFlag, asJSON, pos, err := c.ciFlags("workflows", args, func(fs *flag.FlagSet) { fresh = fs.Bool("fresh", false, "ask the app to fetch now") })
	if err != nil || len(pos) > 0 {
		return ciUsageErr()
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	w, err := cl.Workflows(ctx, repo, *fresh)
	if err != nil {
		return err
	}
	if *asJSON {
		return c.ciJSON(w)
	}
	return c.ciPrinter().Workflows(w)
}

// withTails adds the end of the job log to each failure that has none. A failed log call keeps the failure as it is.
func (cl ciClient) withTails(ctx context.Context, repo string, fs []wire.CIFailureView, lines int) []wire.CIFailureView {
	out := make([]wire.CIFailureView, len(fs))
	for i, f := range fs {
		if len(f.LogTail) == 0 && f.JobID != "" && lines > 0 {
			if v, err := cl.Log(ctx, repo, f.JobID, lines, false); err == nil {
				f.LogTail = v.Lines
			}
		}
		if lines > 0 && len(f.LogTail) > lines {
			f.LogTail = f.LogTail[len(f.LogTail)-lines:]
		}
		out[i] = f
	}
	return out
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// ciWait blocks until the CI of a ref ends. It prints the progress, and the failures at the end.
func (c *cli) ciWait(ctx context.Context, args []string) error {
	var timeout, settle *time.Duration
	var failFast, events *bool
	var logLines *int
	var workflows stringList
	_, repoFlag, asJSON, pos, err := c.ciFlags("wait", args, func(fs *flag.FlagSet) {
		timeout = fs.Duration("timeout", defaultCITimeout, "the longest wait")
		settle = fs.Duration("settle", time.Minute, "how long to wait for a first run")
		failFast, events = fs.Bool("fail-fast", false, "end at the first failure"), fs.Bool("events", false, "print one JSON line for each change")
		logLines = fs.Int("log-lines", defaultCILogs, "log lines for each failed job")
		fs.Var(&workflows, "workflow", "only this workflow; repeat for more")
	})
	if err != nil || *timeout <= 0 || *settle < 0 || *logLines < 0 || *asJSON && *events {
		return ciUsageErr()
	}
	ref, err := c.ciRef(ctx, pos)
	if err != nil {
		return err
	}
	cl, repo, err := c.ciSetup(ctx, *repoFlag)
	if err != nil {
		return err
	}
	w := ciWaiter{cl: cl, c: c, repo: repo, ref: ref, timeout: *timeout, settle: *settle, failFast: *failFast,
		workflows: workflows, logLines: *logLines, json: *asJSON, events: *events, pr: c.ciPrinter()}
	return w.run(ctx)
}

// ciWaiter holds the state of one ci wait.
type ciWaiter struct {
	cl              ciClient
	c               *cli
	repo, ref       string
	timeout, settle time.Duration
	failFast        bool
	workflows       []string
	logLines        int
	json, events    bool
	pr              ciPrinter
	prev            *wire.CIRollup
	staleSince      time.Time
}

func (w *ciWaiter) run(ctx context.Context) error {
	started := time.Now()
	deadline := started.Add(w.timeout)
	var since string
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return w.timedOut()
		}
		poll := min(maxCIPoll, max(left.Round(time.Second), time.Second))
		if !w.staleSince.IsZero() {
			poll = min(poll, ciStalePoll)
		}
		began := time.Now()
		roll, err := w.cl.Wait(ctx, waitQuery{Repo: w.repo, Ref: w.ref, Since: since, Timeout: poll, Settle: w.settle,
			FailFast: w.failFast, Started: started, Workflows: w.workflows})
		if err != nil {
			return err
		}
		if err := w.checkStale(roll); err != nil {
			return err
		}
		if w.prev == nil || roll.Rev != since {
			w.progress(roll)
		}
		since, w.prev = roll.Rev, &roll
		if roll.Done {
			return w.finish(ctx, roll)
		}
		// An answer that comes at once must not spin.
		if gap := waitFloor - time.Since(began); gap > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(gap, max(time.Until(deadline), 0))):
			}
		}
	}
}

// checkStale gives exit 3 when the answers stay stale for ciStaleGrace.
func (w *ciWaiter) checkStale(r wire.CIRollup) error {
	if !r.Stale {
		w.staleSince = time.Time{}
		return nil
	}
	if w.staleSince.IsZero() {
		w.staleSince = time.Now()
	}
	if time.Since(w.staleSince) >= ciStaleGrace {
		return &exitError{exitCINoSource, errors.New("The OverSire app is not open, so the CI state is old. Open it, or check CI on the web.")}
	}
	return nil
}

func (w *ciWaiter) progress(r wire.CIRollup) {
	switch {
	case w.events:
		for _, e := range ciEvents(w.prev, r) {
			w.c.ciJSONLine(e)
		}
	case !w.json:
		w.pr.Progress(r)
	}
}

func (w *ciWaiter) timedOut() error {
	state := wire.CINone
	if w.prev != nil {
		state = w.prev.State
	}
	if w.json && w.prev != nil {
		_ = w.c.ciJSON(w.prev)
	}
	return &exitError{exitCITimeout, fmt.Errorf("Timed out after %s. The CI state is %s.", w.timeout, state)}
}

// finish prints the end of the wait and gives the exit code.
func (w *ciWaiter) finish(ctx context.Context, r wire.CIRollup) error {
	switch r.State {
	case wire.CISuccess:
		return w.report(r, nil)
	case wire.CINone:
		err := w.report(r, nil)
		if err == nil {
			err = &exitError{exitCINoRuns, errors.New("No CI run started for " + w.ref + ".")}
		}
		return err
	}
	fails := w.cl.failuresOf(ctx, r, w.logLines)
	if err := w.report(r, fails); err != nil {
		return err
	}
	return &exitError{exitCIFailed, errSilent}
}

// failuresOf gives the failures of the failed runs of r, with log tails.
func (cl ciClient) failuresOf(ctx context.Context, r wire.CIRollup, lines int) []wire.CIFailureView {
	var out []wire.CIFailureView
	for _, run := range r.Runs {
		if !ciFailedState(run.Conclusion) {
			continue
		}
		fs := run.Failures
		if len(fs) == 0 {
			if a, err := cl.Failures(ctx, r.Repo, run.ID); err == nil {
				fs = a.Failures
			}
		}
		out = append(out, cl.withTails(ctx, r.Repo, fs, lines)...)
	}
	return out
}

func (w *ciWaiter) report(r wire.CIRollup, fails []wire.CIFailureView) error {
	switch {
	case w.json:
		for i := range r.Runs {
			r.Runs[i].Failures = nil
		}
		return w.c.ciJSON(struct {
			wire.CIRollup
			FailureViews []wire.CIFailureView `json:"failureViews,omitempty"`
		}{r, fails})
	case w.events:
		for _, f := range fails {
			w.c.ciJSONLine(ciEvent{Type: "failure", Job: f.Job, JobID: f.JobID, Step: f.Step, Messages: f.Messages, LogTail: f.LogTail, URL: f.URL})
		}
		return nil
	}
	w.pr.Rollup(r)
	for _, f := range fails {
		w.pr.Failure(f, r.Repo)
	}
	for _, ck := range r.Checks {
		if ciFailedState(string(ck.State)) {
			fmt.Fprintf(w.c.out, "FAILED check %s %s\n", ck.Name, ck.URL)
		}
	}
	return nil
}

func (c *cli) ciJSONLine(v any) { _ = json.NewEncoder(c.out).Encode(v) }

// ciFailedState tells if a conclusion or state is a failure.
func ciFailedState(s string) bool {
	switch s {
	case "failure", "timed_out", "startup_failure", "cancelled":
		return true
	}
	return false
}

// ciEvent is one line of ci wait --events.
type ciEvent struct {
	Type     string   `json:"type"`
	Run      string   `json:"run,omitempty"`
	Workflow string   `json:"workflow,omitempty"`
	Job      string   `json:"job,omitempty"`
	JobID    string   `json:"jobId,omitempty"`
	Step     string   `json:"step,omitempty"`
	State    string   `json:"state,omitempty"`
	Done     *bool    `json:"done,omitempty"`
	Failed   *int     `json:"failed,omitempty"`
	Total    *int     `json:"total,omitempty"`
	Messages []string `json:"messages,omitempty"`
	LogTail  []string `json:"logTail,omitempty"`
	URL      string   `json:"url,omitempty"`
}

func jobState(j wire.CIJob) string {
	if j.Conclusion != "" {
		return j.Conclusion
	}
	return j.Status
}

func runState(r wire.CIRunView) string {
	if r.Conclusion != "" {
		return r.Conclusion
	}
	return r.Status
}

// ciCounts gives the failed and the total number of runs and checks.
func ciCounts(r wire.CIRollup) (failed, total int) {
	for _, run := range r.Runs {
		total++
		if ciFailedState(run.Conclusion) {
			failed++
		}
	}
	for _, ck := range r.Checks {
		total++
		if ciFailedState(string(ck.State)) {
			failed++
		}
	}
	return failed, total
}

// ciEvents gives the changes from prev to next: run lines, job lines, then one rollup line.
// A nil prev is the first answer.
func ciEvents(prev *wire.CIRollup, next wire.CIRollup) []ciEvent {
	old := map[string]wire.CIRunView{}
	oldJobs := map[string]string{}
	if prev != nil {
		for _, r := range prev.Runs {
			old[r.ID] = r
			for _, j := range r.Jobs {
				oldJobs[r.ID+"/"+j.ID] = jobState(j)
			}
		}
	}
	var out []ciEvent
	for _, r := range next.Runs {
		if o, ok := old[r.ID]; !ok || runState(o) != runState(r) {
			out = append(out, ciEvent{Type: "run", Run: r.ID, Workflow: r.Workflow, State: runState(r), URL: r.URL})
		}
		for _, j := range r.Jobs {
			if s, ok := oldJobs[r.ID+"/"+j.ID]; !ok || s != jobState(j) {
				out = append(out, ciEvent{Type: "job", Run: r.ID, Job: j.Name, JobID: j.ID, State: jobState(j), URL: j.URL})
			}
		}
	}
	failed, total := ciCounts(next)
	roll := ciEvent{Type: "rollup", State: string(next.State), Done: &next.Done, Failed: &failed, Total: &total}
	changed := len(out) > 0 || prev == nil || prev.State != next.State || prev.Done != next.Done
	if !changed {
		pf, pt := ciCounts(*prev)
		changed = pf != failed || pt != total
	}
	if changed {
		out = append(out, roll)
	}
	return out
}

// ciPrinter prints the answers as text.
type ciPrinter struct {
	w   io.Writer
	now func() time.Time
}

// Fresh prints a line when the answer is old or the quota is low.
func (p ciPrinter) Fresh(f wire.CIFreshness) {
	if f.Stale && !f.AsOf.IsZero() {
		fmt.Fprintf(p.w, "The CI state is %d min old. The OverSire app is not open, or it does not poll.\n", int(p.now().Sub(f.AsOf)/time.Minute))
	} else if f.Stale {
		fmt.Fprintln(p.w, "The CI state is old. The OverSire app is not open, or it does not poll.")
	}
	if f.Degraded {
		fmt.Fprintln(p.w, "The GitHub quota is low. OverSire fetches less.")
	}
}

func ciDuration(s int) string {
	if s <= 0 {
		return ""
	}
	return (time.Duration(s) * time.Second).String()
}

func short(sha string) string { return sha[:min(len(sha), 7)] }

func (p ciPrinter) runLine(r wire.CIRunView) string {
	name := r.Workflow
	if name == "" {
		name = r.Name
	}
	s := fmt.Sprintf("run %s  %s  %s", r.ID, name, runState(r))
	if d := ciDuration(r.DurationS); d != "" {
		s += "  " + d
	}
	if d := ciDuration(r.ETAS); d != "" && r.Conclusion == "" {
		s += "  eta " + d
	}
	return s
}

func (p ciPrinter) Rollup(r wire.CIRollup) error {
	p.Fresh(r.CIFreshness)
	fmt.Fprintf(p.w, "%s %s (%s): %s\n", r.Repo, short(r.SHA), r.Ref, r.State)
	for _, run := range r.Runs {
		fmt.Fprintf(p.w, "  %s  %s\n", p.runLine(run), run.URL)
	}
	for _, ck := range r.Checks {
		fmt.Fprintf(p.w, "  check %s  %s  %s\n", ck.Name, ck.State, ck.URL)
	}
	if r.State == wire.CINone {
		fmt.Fprintln(p.w, "  No CI run found for this commit.")
	}
	return nil
}

// Progress prints one line for a change in wait.
func (p ciPrinter) Progress(r wire.CIRollup) {
	p.Fresh(r.CIFreshness)
	failed, total := ciCounts(r)
	fmt.Fprintf(p.w, "%s: %d of %d failed\n", r.State, failed, total)
}

func (p ciPrinter) Runs(page wire.CIRunsPage) error {
	p.Fresh(page.CIFreshness)
	if len(page.Runs) == 0 {
		fmt.Fprintln(p.w, "No runs found.")
	}
	for _, r := range page.Runs {
		fmt.Fprintf(p.w, "%s  %s  %s  %s  %s\n", p.runLine(r), short(r.SHA), r.Branch, r.UpdatedAt, r.URL)
	}
	return nil
}

func (p ciPrinter) Run(a wire.CIRunAnswer, repo string, jobs bool) error {
	p.Fresh(a.CIFreshness)
	r := a.Run
	fmt.Fprintf(p.w, "%s  attempt %d  %s %s\n  %s\n", p.runLine(r), r.Attempt, short(r.SHA), r.Branch, r.URL)
	if jobs {
		for _, j := range r.Jobs {
			fmt.Fprintf(p.w, "  job %s  %s  %s\n", j.ID, j.Name, jobState(j))
			for _, s := range j.Steps {
				fmt.Fprintf(p.w, "    step %s  %s\n", s.Name, ciStepState(s))
			}
		}
	}
	for _, f := range r.Failures {
		p.Failure(f, repo)
	}
	return nil
}

func ciStepState(s wire.CIStep) string {
	if s.Conclusion != "" {
		return s.Conclusion
	}
	return s.Status
}

// Failure prints one failed job and the command that reads its log.
func (p ciPrinter) Failure(f wire.CIFailureView, repo string) {
	fmt.Fprintf(p.w, "FAILED job %s", f.Job)
	if f.Step != "" {
		fmt.Fprintf(p.w, ", step %s", f.Step)
	}
	fmt.Fprintln(p.w)
	for _, m := range f.Messages {
		fmt.Fprintf(p.w, "  %s\n", m)
	}
	if len(f.LogTail) > 0 {
		fmt.Fprintln(p.w, "  Log:")
		for _, l := range f.LogTail {
			fmt.Fprintf(p.w, "    %s\n", l)
		}
	}
	if f.URL != "" {
		fmt.Fprintf(p.w, "  %s\n", f.URL)
	}
	if f.JobID != "" && repo != "" {
		fmt.Fprintf(p.w, "  Next: oversire ci logs %s --repo %s\n", f.JobID, repo)
	}
}

func (p ciPrinter) Failures(a wire.CIFailuresAnswer, repo string) error {
	p.Fresh(a.CIFreshness)
	if len(a.Failures) == 0 {
		fmt.Fprintf(p.w, "Run %s has no failed job.\n", a.Run)
	}
	for _, f := range a.Failures {
		p.Failure(f, repo)
	}
	if len(a.Failures) > 0 {
		fmt.Fprintf(p.w, "Next: oversire ci explain %s --repo %s\n", a.Run, repo)
	}
	return nil
}

// Explanation prints the cause, the fix, the confidence and the next command.
func (p ciPrinter) Explanation(e wire.CIExplanation) error {
	fix := e.Fix
	if fix == "" {
		fix = "none"
	}
	origin := "new"
	if e.Source == "cache" {
		origin = "cache"
	}
	fmt.Fprintf(p.w, "Cause: %s\nFix: %s\nConfidence: %s (%s)\n", e.Cause, fix, e.Confidence, origin)
	if e.Job != "" {
		fmt.Fprintf(p.w, "Next: oversire ci logs %s --repo %s\n", e.Job, e.Repo)
	} else {
		fmt.Fprintf(p.w, "Next: oversire ci logs %s --failed --repo %s\n", e.Run, e.Repo)
	}
	return nil
}

func (p ciPrinter) Workflows(w wire.CIWorkflows) error {
	p.Fresh(w.CIFreshness)
	for _, wf := range w.Workflows {
		fmt.Fprintf(p.w, "%s  %s  %s  %s\n", wf.ID, wf.Name, wf.State, wf.Path)
	}
	return nil
}
