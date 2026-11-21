package ci

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	// StaleAfter is the age of the last poll or hint of a repo after which the data counts as stale.
	StaleAfter = 5 * time.Minute
	tick       = 30 * time.Second
	// expectHold is how long a rerun shows as queued while the mirror has no new attempt.
	expectHold = 2 * time.Minute
	docV       = 1
)

var (
	ErrNoCIData  = errors.New("the source app sends no CI data")
	ErrUntracked = errors.New("the repo is not tracked")
	ErrNoJobs    = errors.New("the jobs of the run are not in the index")
)

// textError is a sentinel with the text for the caller.
type textError struct {
	is   error
	text string
}

func (e textError) Error() string        { return e.text }
func (e textError) Is(target error) bool { return target == e.is }

// Feed is the mirror store (mirror.Store).
type Feed interface {
	Head() wire.MirrorHead
	Page(after int64, limit int, epoch string, f mirror.Filter) (wire.MirrorPage, error)
	Source() (mirror.Source, bool)
}

// Target is the commit that a ref names.
type Target struct {
	SHA    string
	PR     int
	Branch string
	RunID  string
}

// JobInfo is what the index knows of a job.
type JobInfo struct {
	Run       string
	Completed bool
}

// expected is a run attempt that a rerun asked for and that the mirror does not have yet.
type expected struct {
	attempt int
	until   time.Time
}

// Index is the CI view of the mirror feed in memory. It also writes the runs to the Book.
type Index struct {
	Log *slog.Logger

	feed   Feed
	book   *Book
	online func() bool
	now    func() time.Time
	wake   chan struct{}

	mu      sync.RWMutex
	epoch   string
	after   int64
	prs     map[string][]wire.CIPullRef // by repo
	jobs    map[string]wire.CIJobsDoc   // by doc id
	checks  map[string]wire.CIChecksDoc // by doc id
	state   *wire.CIStateDoc
	expect  map[string]expected // by repo/run
	changed chan struct{}
}

// NewIndex makes the index. online tells if the source app is connected.
func NewIndex(feed Feed, book *Book, online func() bool, now func() time.Time) *Index {
	if online == nil {
		online = func() bool { return false }
	}
	if now == nil {
		now = time.Now
	}
	x := &Index{Log: slog.Default(), feed: feed, book: book, online: online, now: now, wake: make(chan struct{}, 1), changed: make(chan struct{}),
		expect: map[string]expected{}}
	x.clear()
	return x
}

func (x *Index) clear() {
	x.prs, x.jobs, x.checks, x.state = map[string][]wire.CIPullRef{}, map[string]wire.CIJobsDoc{}, map[string]wire.CIChecksDoc{}, nil
}

// Wake tells the index to read the feed. It does not block.
func (x *Index) Wake() {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Changed gives a channel that closes at the next change of the index.
func (x *Index) Changed() <-chan struct{} {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.changed
}

// Run reads the feed at the start, after each Wake and after each tick, until ctx ends.
func (x *Index) Run(ctx context.Context) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		x.sync()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-x.wake:
		case <-t.C:
		}
	}
}

var feedKinds = []wire.MirrorKind{wire.MirrorRuns, wire.MirrorPRs, wire.MirrorCIJobs, wire.MirrorCIChecks, wire.MirrorCIState}

// sync reads all pages after the last cursor.
func (x *Index) sync() {
	resets := 0
	for {
		p, err := x.feed.Page(x.after, 0, x.epoch, mirror.Filter{WithCI: true, Kinds: feedKinds})
		if err != nil {
			x.Log.Error("ci: read the mirror", "err", err)
			return
		}
		if p.Reset {
			x.drop(p.Epoch)
			if resets++; resets > 3 {
				return
			}
			continue
		}
		x.apply(p)
		if !p.More {
			return
		}
	}
}

func (x *Index) drop(epoch string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.clear()
	x.epoch, x.after = epoch, 0
	x.signal()
}

// signal wakes the waiters. The caller holds mu.
func (x *Index) signal() {
	close(x.changed)
	x.changed = make(chan struct{})
}

func (x *Index) apply(p wire.MirrorPage) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.epoch, x.after = p.Epoch, p.Next
	for _, d := range p.Docs {
		if d.V > docV {
			continue
		}
		if err := x.applyDoc(d); err != nil {
			x.Log.Warn("ci: skip a doc", "kind", d.Kind, "id", d.ID, "err", err)
		}
	}
	if len(p.Docs) > 0 {
		x.signal()
	}
}

func (x *Index) applyDoc(d wire.MirrorDoc) error {
	switch d.Kind {
	case wire.MirrorRuns:
		if d.Deleted {
			return nil
		}
		var b wire.CIRunsDoc
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return err
		}
		return x.book.PutRuns(d.ID, b.Runs)
	case wire.MirrorPRs:
		if d.Deleted {
			delete(x.prs, d.ID)
			return nil
		}
		var b wire.CIPRsDoc
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return err
		}
		x.prs[d.ID] = b.PRs
	case wire.MirrorCIJobs:
		if d.Deleted {
			delete(x.jobs, d.ID)
			return nil
		}
		var b wire.CIJobsDoc
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return err
		}
		x.jobs[d.ID] = b
	case wire.MirrorCIChecks:
		if d.Deleted {
			delete(x.checks, d.ID)
			return nil
		}
		var b wire.CIChecksDoc
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return err
		}
		x.checks[d.ID] = b
	case wire.MirrorCIState:
		if d.Deleted {
			x.state = nil
			return nil
		}
		var b wire.CIStateDoc
		if err := json.Unmarshal(d.Body, &b); err != nil {
			return err
		}
		x.state = &b
	}
	return nil
}

// SourceMachine gives the machine of the source app, for the texts.
func (x *Index) SourceMachine() string {
	if src, ok := x.feed.Source(); ok && src.Machine != "" {
		return src.Machine
	}
	return "the source machine"
}

// Repo gives the CI state of a tracked repo. The name may differ in case. The answer holds the name of the app.
func (x *Index) Repo(repo string) (wire.CIRepoState, error) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	if x.state == nil {
		return wire.CIRepoState{}, textError{ErrNoCIData, "The OverSire app on " + x.SourceMachine() + " sends no CI data. Update the app."}
	}
	for _, s := range x.state.Repos {
		if strings.EqualFold(s.Repo, repo) {
			return s, nil
		}
	}
	return wire.CIRepoState{}, textError{ErrUntracked, "OverSire does not track " + repo + ". Add the repo in Settings › Code hosts."}
}

// Freshness gives the freshness fields of the answers for a repo.
func (x *Index) Freshness(repo string) wire.CIFreshness {
	x.mu.RLock()
	defer x.mu.RUnlock()
	f := wire.CIFreshness{Source: wire.CISourcePoll}
	if x.state != nil {
		f.AsOf, f.Source, f.Degraded = x.state.AsOf, x.state.Source, x.state.Degraded
		for _, s := range x.state.Repos {
			if strings.EqualFold(s.Repo, repo) && s.PolledAt != nil {
				f.AsOf = *s.PolledAt
			}
		}
	}
	f.Stale = !x.online() || x.now().Sub(f.AsOf) > StaleAfter
	return f
}

func notFound(format string, a ...any) error { return server.Refuse(server.NotFound, format, a...) }

// repoRuns gives all runs of a repo in the Book, newest first.
func (x *Index) repoRuns(repo string) []wire.CIRun {
	runs, err := x.book.Runs(RunQuery{Repo: repo})
	if err != nil {
		x.Log.Error("ci: read the runs", "err", err)
	}
	return runs
}

func (x *Index) pr(repo string, n int) (wire.CIPullRef, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, p := range x.prs[repo] {
		if p.Number == n {
			return p, true
		}
	}
	return wire.CIPullRef{}, false
}

func (x *Index) prOfBranch(repo, branch string) int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, p := range x.prs[repo] {
		if p.HeadRefName == branch {
			return p.Number
		}
	}
	return 0
}

// Resolve turns a ref into a commit.
func (x *Index) Resolve(repo string, r Ref) (Target, error) {
	st, err := x.Repo(repo)
	if err != nil {
		return Target{}, err
	}
	repo = st.Repo
	runs := x.repoRuns(repo)
	switch r.Kind {
	case RefPR:
		p, ok := x.pr(repo, r.PR)
		if !ok {
			return Target{}, notFound("OverSire has no PR #%d in %s.", r.PR, repo)
		}
		t := Target{SHA: p.HeadSha, PR: p.Number, Branch: p.HeadRefName}
		if t.SHA == "" {
			t.SHA = newestSHA(runs, p.HeadRefName)
		}
		return t, nil
	case RefRun:
		for _, run := range runs {
			if run.ID == r.Value {
				return Target{SHA: run.HeadSha, Branch: run.HeadBranch, RunID: run.ID, PR: x.prOfBranch(repo, run.HeadBranch)}, nil
			}
		}
		return Target{}, notFound("OverSire has no run %s in %s.", r.Value, repo)
	case RefSHA:
		if sha, ok := x.fullSHA(repo, runs, r.Value); ok {
			return Target{SHA: sha}, nil
		}
		if len(r.Value) == 40 {
			return Target{SHA: r.Value}, nil
		}
	}
	return Target{SHA: newestSHA(runs, r.Value), Branch: r.Value, PR: x.prOfBranch(repo, r.Value)}, nil
}

func newestSHA(runs []wire.CIRun, branch string) string {
	for _, run := range runs {
		if run.HeadBranch == branch {
			return run.HeadSha
		}
	}
	return ""
}

// fullSHA finds the full SHA of a known run, PR or check that starts with prefix.
func (x *Index) fullSHA(repo string, runs []wire.CIRun, prefix string) (string, bool) {
	for _, run := range runs {
		if strings.HasPrefix(run.HeadSha, prefix) {
			return run.HeadSha, true
		}
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, p := range x.prs[repo] {
		if strings.HasPrefix(p.HeadSha, prefix) {
			return p.HeadSha, true
		}
	}
	for _, c := range x.checks {
		if strings.EqualFold(c.Repo, repo) && strings.HasPrefix(c.Sha, prefix) {
			return c.Sha, true
		}
	}
	return "", false
}

// JobsDoc gives the ciJobs doc of one attempt of a run.
func (x *Index) JobsDoc(repo, run string, attempt int) (wire.CIJobsDoc, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	d, ok := x.jobs[repo+"/"+run+"/"+strconv.Itoa(max(attempt, 1))]
	return d, ok
}

// Job gives what the jobs docs hold of a job. The newest attempt wins.
func (x *Index) Job(repo, jobID string) (JobInfo, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var best wire.CIJobsDoc
	var info JobInfo
	var found bool
	for _, d := range x.jobs {
		if !strings.EqualFold(d.Repo, repo) || found && d.Attempt < best.Attempt {
			continue
		}
		for _, j := range d.Jobs {
			if j.ID == jobID {
				best, found = d, true
				info = JobInfo{Run: d.Run, Completed: j.Status == "completed"}
			}
		}
	}
	return info, found
}

func (x *Index) checksOf(repo, sha string) []wire.CICheck {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.checks[repo+"/"+sha].Checks
}

// view makes the answer view of a run with its duration, its ETA and the jobs of its attempt.
func (x *Index) view(r wire.CIRun, history []wire.CIRun) wire.CIRunView {
	v := runView(r)
	now := x.now()
	start := parseTime(r.StartedAt)
	if start.IsZero() {
		start = parseTime(r.CreatedAt)
	}
	if !start.IsZero() {
		end := now
		if r.Status == "completed" {
			end = parseTime(r.UpdatedAt)
		}
		if d := end.Sub(start); d > 0 {
			v.DurationS = int(d.Seconds())
		}
	}
	if r.Status != "completed" {
		if d, ok := eta(history, workflowOf(r), now); ok {
			v.ETAS = int(d.Seconds())
		}
	}
	return v
}

func (x *Index) withJobs(repo string, v wire.CIRunView) wire.CIRunView {
	if d, ok := x.JobsDoc(repo, v.ID, v.Attempt); ok {
		v.Jobs, v.Failures = d.Jobs, d.Failures
	}
	return v
}

// ExpectAttempt tells that a rerun of the run starts this attempt. Until the mirror has it, or for 2 min, Rollup gives the
// run as queued, so a wait does not end on the old attempt.
func (x *Index) ExpectAttempt(repo, run string, attempt int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	for k, e := range x.expect {
		if !now.Before(e.until) {
			delete(x.expect, k)
		}
	}
	x.expect[repo+"/"+run] = expected{attempt: attempt, until: now.Add(expectHold)}
	x.signal()
}

// held gives v as a queued run of the expected attempt while the hold is on.
func (x *Index) held(repo string, v wire.CIRunView) wire.CIRunView {
	x.mu.RLock()
	e, ok := x.expect[repo+"/"+v.ID]
	x.mu.RUnlock()
	if !ok || v.Attempt >= e.attempt || !x.now().Before(e.until) {
		return v
	}
	return wire.CIRunView{ID: v.ID, Attempt: e.attempt, Name: v.Name, Workflow: v.Workflow, Status: "queued", Branch: v.Branch, SHA: v.SHA,
		Event: v.Event, URL: v.URL}
}

// Rollup gives the state of the runs and checks of the commit of t. workflows limits the runs to the named workflows.
func (x *Index) Rollup(repo string, t Target, workflows []string) (wire.CIRollup, error) {
	st, err := x.Repo(repo)
	if err != nil {
		return wire.CIRollup{}, err
	}
	repo = st.Repo
	hist := x.repoRuns(repo)
	set := runSet{}
	if t.SHA != "" {
		q := RunQuery{SHA: t.SHA}
		for _, r := range hist {
			if q.matches(r) {
				set = append(set, x.withJobs(repo, x.view(r, hist)))
			}
		}
	}
	set = set.latest().keepWorkflows(workflows)
	for i, v := range set {
		set[i] = x.held(repo, v)
	}
	sort.Slice(set, func(i, j int) bool {
		if set[i].Workflow != set[j].Workflow {
			return set[i].Workflow < set[j].Workflow
		}
		return set[i].ID < set[j].ID
	})
	checks := []wire.CICheck{}
	if t.SHA != "" {
		checks = append(checks, x.checksOf(repo, t.SHA)...)
	}
	state, done := set.state(checks)
	out := wire.CIRollup{Repo: repo, SHA: t.SHA, PR: t.PR, State: state, Done: done, Runs: set, Checks: checks, CIFreshness: x.Freshness(repo)}
	if out.Runs == nil {
		out.Runs = []wire.CIRunView{}
	}
	out.Rev = rev(out)
	return out, nil
}

// Runs lists the runs that match q, newest first.
func (x *Index) Runs(q RunQuery) (wire.CIRunsPage, error) {
	st, err := x.Repo(q.Repo)
	if err != nil {
		return wire.CIRunsPage{}, err
	}
	q.Repo = st.Repo
	if q.PR != 0 {
		p, ok := x.pr(q.Repo, q.PR)
		if !ok {
			return wire.CIRunsPage{}, notFound("OverSire has no PR #%d in %s.", q.PR, q.Repo)
		}
		if p.HeadSha != "" {
			q.SHA = p.HeadSha
		} else {
			q.Branch = p.HeadRefName
		}
	}
	hist := x.repoRuns(q.Repo)
	out := wire.CIRunsPage{Repo: q.Repo, Runs: []wire.CIRunView{}, CIFreshness: x.Freshness(q.Repo)}
	for _, r := range hist {
		if q.matches(r) {
			out.Runs = append(out.Runs, x.view(r, hist))
			if q.Limit > 0 && len(out.Runs) == q.Limit {
				break
			}
		}
	}
	return out, nil
}

// attempt picks the attempt to show: the one asked, else the one of the run.
func attempt(asked int, r wire.CIRun) int {
	if asked > 0 {
		return asked
	}
	return max(r.Attempt, 1)
}

// RunAnswer gives one run, with the jobs and the failures of the attempt when the jobs doc exists.
func (x *Index) RunAnswer(repo, id string, att int) (wire.CIRunAnswer, error) {
	st, err := x.Repo(repo)
	if err != nil {
		return wire.CIRunAnswer{}, err
	}
	repo = st.Repo
	run, ok, err := x.book.Run(repo, id)
	if err != nil {
		return wire.CIRunAnswer{}, err
	}
	if !ok {
		return wire.CIRunAnswer{}, notFound("OverSire has no run %s in %s.", id, repo)
	}
	v := x.view(run, x.repoRuns(repo))
	if want := attempt(att, run); want != v.Attempt {
		d, ok := x.JobsDoc(repo, id, want)
		if !ok {
			return wire.CIRunAnswer{}, notFound("OverSire has no attempt %d of run %s in %s.", want, id, repo)
		}
		v.Attempt, v.Status, v.Conclusion, v.URL = d.Attempt, d.Status, d.Conclusion, d.URL
		v.DurationS, v.ETAS = 0, 0
	}
	return wire.CIRunAnswer{Run: x.withJobs(repo, v), CIFreshness: x.Freshness(repo)}, nil
}

// Failures gives the failures of an attempt. ErrNoJobs: the index has no jobs doc for the attempt. The answer then holds
// the repo, the run and the attempt.
func (x *Index) Failures(repo, id string, att int) (wire.CIFailuresAnswer, error) {
	st, err := x.Repo(repo)
	if err != nil {
		return wire.CIFailuresAnswer{}, err
	}
	repo = st.Repo
	run, ok, err := x.book.Run(repo, id)
	if err != nil {
		return wire.CIFailuresAnswer{}, err
	}
	if !ok {
		return wire.CIFailuresAnswer{}, notFound("OverSire has no run %s in %s.", id, repo)
	}
	att = attempt(att, run)
	d, ok := x.JobsDoc(repo, id, att)
	if !ok {
		return wire.CIFailuresAnswer{Repo: repo, Run: id, Attempt: att}, ErrNoJobs
	}
	f := d.Failures
	if f == nil {
		f = []wire.CIFailureView{}
	}
	return wire.CIFailuresAnswer{Repo: repo, Run: id, Attempt: att, Failures: f, CIFreshness: x.Freshness(repo)}, nil
}
