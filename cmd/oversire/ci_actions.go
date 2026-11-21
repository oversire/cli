package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

// runArg is a run or job from the command line: an id, or a run or job URL of the code host.
type runArg struct {
	Repo    string // from a URL, else empty
	Run     string
	Job     string
	Attempt int // 0 means latest; not sent, the app acts on the latest attempt
}

var runPath = regexp.MustCompile(`^/(.+?)/actions/runs/([0-9]+)(?:/attempts/([0-9]+))?(?:/job/([0-9]+))?/?$`)

func parseRunArg(s string) (runArg, error) {
	s = strings.TrimSpace(s)
	if runIDRe.MatchString(s) {
		return runArg{Run: s}, nil
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		if m := runPath.FindStringSubmatch(u.Path); m != nil {
			a := runArg{Repo: m[1], Run: m[2], Job: m[4]}
			a.Attempt, _ = strconv.Atoi(m[3])
			return a, nil
		}
	}
	return runArg{}, &exitError{exitCIUsage, errors.New("Give a run id, a run URL or a job URL.")}
}

// Action posts a CI write. No daemon, no app, home offline or an old daemon give exit 3. Each other refusal gives exit 1,
// also a 504: the change can be made already.
func (cl ciClient) Action(ctx context.Context, path string, in wire.CIActionRequest) (res wire.CIActionResult, err error) {
	err = cl.call(ctx, http.MethodPost, path, in, &res)
	if err == nil {
		return res, nil
	}
	if _, ok := errors.AsType[*exitError](err); ok {
		return res, err
	}
	if de, ok := errors.AsType[*daemonError](err); ok && de.Status == http.StatusNotFound && de.Plain {
		return res, &exitError{exitCINoSource, errors.New("The daemon has no CI writes. Update it: oversire update")}
	}
	return res, &exitError{exitCIFailed, err}
}

// ciWriteFlags are the flags of each CI write.
type ciWriteFlags struct {
	*appFlags
	repo    *string
	wait    *bool
	timeout *time.Duration
}

func newCIWriteFlags(name string, waits bool) ciWriteFlags {
	f := ciWriteFlags{appFlags: newAppFlags("ci " + name)}
	f.repo = f.fs.String("repo", "", "owner/name")
	if waits {
		f.wait = f.fs.Bool("wait", false, "wait until the CI of the run ends")
		f.timeout = f.fs.Duration("timeout", defaultCITimeout, "the longest wait")
	}
	return f
}

// ciWriteSetup gives the client and the repo. The repo of a URL wins over --repo.
func (c *cli) ciWriteSetup(ctx context.Context, f ciWriteFlags, urlRepo string) (ciClient, string, error) {
	repo := *f.repo
	if urlRepo != "" {
		repo = urlRepo
	}
	return c.ciSetup(ctx, repo)
}

// ciRunWrite parses `<cmd> RUN` and its flags. setup adds the flags of the command.
func (c *cli) ciRunWrite(name string, waits bool, args []string, setup func(fs ciWriteFlags)) (ciWriteFlags, runArg, error) {
	f := newCIWriteFlags(name, waits)
	if setup != nil {
		setup(f)
	}
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return f, runArg{}, ciUsageErr()
	}
	if waits && *f.timeout <= 0 {
		return f, runArg{}, ciUsageErr()
	}
	a, err := parseRunArg(pos[0])
	return f, a, err
}

func (c *cli) ciRerun(ctx context.Context, args []string) error {
	var failed *bool
	var job *string
	f, a, err := c.ciRunWrite("rerun", true, args, func(f ciWriteFlags) {
		failed = f.fs.Bool("failed", false, "rerun only the failed jobs")
		job = f.fs.String("job", "", "rerun only this job")
	})
	if err != nil {
		return err
	}
	if *job != "" {
		if a.Job, err = ciJobID(*job); err != nil {
			return err
		}
	}
	if *failed && a.Job != "" {
		return &exitError{exitCIUsage, errors.New("Give --failed or a job, not both.")}
	}
	cl, repo, err := c.ciWriteSetup(ctx, f, a.Repo)
	if err != nil {
		return err
	}
	in := wire.CIActionRequest{Repo: repo, Run: a.Run, FailedOnly: *failed, Agent: f.agent()}
	path := "/v1/ci/runs/" + url.PathEscape(a.Run) + "/rerun"
	if a.Job != "" {
		path = "/v1/ci/jobs/" + url.PathEscape(a.Job) + "/rerun"
	}
	res, err := cl.Action(ctx, path, in)
	if err != nil {
		return err
	}
	return c.ciWritten(ctx, cl, f, repo, res, a.Run, nil)
}

func (c *cli) ciCancel(ctx context.Context, args []string) error {
	f, a, err := c.ciRunWrite("cancel", false, args, nil)
	if err != nil {
		return err
	}
	cl, repo, err := c.ciWriteSetup(ctx, f, a.Repo)
	if err != nil {
		return err
	}
	res, err := cl.Action(ctx, "/v1/ci/runs/"+url.PathEscape(a.Run)+"/cancel", wire.CIActionRequest{Repo: repo, Run: a.Run, Agent: f.agent()})
	if err != nil {
		return err
	}
	return c.ciWritten(ctx, cl, f, repo, res, "", nil)
}

func (c *cli) ciApprove(ctx context.Context, args []string) error {
	var envs stringsFlag
	var comment *string
	f, a, err := c.ciRunWrite("approve", false, args, func(f ciWriteFlags) {
		f.fs.Var(&envs, "env", "an environment to approve; repeat for more (default: all that wait)")
		comment = f.fs.String("comment", "", "the review comment")
	})
	if err != nil {
		return err
	}
	cl, repo, err := c.ciWriteSetup(ctx, f, a.Repo)
	if err != nil {
		return err
	}
	in := wire.CIActionRequest{Repo: repo, Run: a.Run, Environments: envs, Comment: *comment, Agent: f.agent()}
	res, err := cl.Action(ctx, "/v1/ci/runs/"+url.PathEscape(a.Run)+"/approve", in)
	if err != nil {
		return err
	}
	return c.ciWritten(ctx, cl, f, repo, res, "", nil)
}

func (c *cli) ciDispatch(ctx context.Context, args []string) error {
	f := newCIWriteFlags("dispatch", true)
	ref := f.fs.String("ref", "", "the branch or tag (default: the branch with an upstream, else the default branch)")
	var fields stringsFlag
	f.fs.Var(&fields, "f", "an input key=value; repeat for more")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || pos[0] == "" || *f.timeout <= 0 {
		return ciUsageErr()
	}
	inputs := map[string]string{}
	for _, kv := range fields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return &exitError{exitCIUsage, fmt.Errorf("Give the input %q as key=value.", kv)}
		}
		inputs[k] = v
	}
	if len(inputs) == 0 {
		inputs = nil
	}
	if *ref == "" {
		if *ref, err = c.dispatchRef(ctx); err != nil {
			return err
		}
	}
	cl, repo, err := c.ciWriteSetup(ctx, f, "")
	if err != nil {
		return err
	}
	in := wire.CIActionRequest{Repo: repo, Workflow: pos[0], Ref: *ref, Inputs: inputs, Agent: f.agent()}
	res, err := cl.Action(ctx, "/v1/ci/dispatch", in)
	if err != nil {
		return err
	}
	if !*f.json && !strings.Contains(res.Summary, *ref) {
		fmt.Fprintf(c.out, "Ref: %s\n", *ref)
	}
	return c.ciWritten(ctx, cl, f, repo, res, *ref, []string{pos[0]})
}

// dispatchRef is the branch of the git folder when it has an upstream, else the default branch.
func (c *cli) dispatchRef(ctx context.Context) (string, error) {
	g := c.gitDir()
	if b, up, err := g.Branch(ctx); err == nil && up {
		return b, nil
	}
	if b, err := g.DefaultBranch(ctx); err == nil {
		return b, nil
	}
	return "", &exitError{exitCIUsage, errors.New("Cannot find the branch. Give it with --ref.")}
}

// ciWritten prints the result. With --wait it then waits on the run of the result, else on ref and workflows.
func (c *cli) ciWritten(ctx context.Context, cl ciClient, f ciWriteFlags, repo string, res wire.CIActionResult, ref string, workflows []string) error {
	if *f.json {
		if err := c.ciJSON(res); err != nil {
			return err
		}
	} else {
		text := res.Summary
		if text == "" && res.Noop {
			text = "Nothing to do."
		}
		fmt.Fprintln(c.out, text)
	}
	run := ""
	if res.Run != nil && res.Run.ID != "" {
		run = res.Run.ID
		ref, workflows = run, nil
		if res.Run.Workflow != "" {
			workflows = []string{res.Run.Workflow}
		}
	}
	if f.wait == nil || ref == "" {
		return nil
	}
	if !*f.wait {
		if run != "" && !*f.json {
			fmt.Fprintf(c.out, "Next: oversire ci wait %s --repo %s\n", run, repo)
		}
		return nil
	}
	w := ciWaiter{cl: cl, c: c, repo: repo, ref: ref, timeout: *f.timeout, settle: time.Minute, workflows: workflows,
		logLines: defaultCILogs, json: *f.json, pr: c.ciPrinter()}
	return w.run(ctx)
}

// Branch gives the branch of HEAD. With an upstream, it gives the branch name on the remote.
func (g gitDir) Branch(ctx context.Context) (branch string, upstream bool, err error) {
	branch, err = g.run(ctx, g.dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		return "", false, errors.New("HEAD is not on a branch")
	}
	up, err := g.run(ctx, g.dir, "rev-parse", "--abbrev-ref", "@{upstream}")
	if err != nil || up == "" {
		return branch, false, nil
	}
	if _, name, ok := strings.Cut(up, "/"); ok && name != "" {
		branch = name
	}
	return branch, true, nil
}

// DefaultBranch gives the branch of origin/HEAD.
func (g gitDir) DefaultBranch(ctx context.Context) (string, error) {
	out, err := g.run(ctx, g.dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	b := strings.TrimPrefix(out, "origin/")
	if err != nil || b == "" || b == out {
		return "", errors.New("origin has no HEAD")
	}
	return b, nil
}
