package runbook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

var t0 = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// launcher starts one fake session for each launch. fail makes the launch of a step fail.
type launcher struct {
	mu    sync.Mutex
	reqs  []wire.LaunchRequest
	rts   []string
	stops []wire.SessionRef
	fail  map[string]error
	turn  bool
}

func (l *launcher) Launch(_ context.Context, rt string, o runtime.Order, r wire.LaunchRequest) (runtime.Started, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !strings.HasPrefix(o.ID, "runbook-") || o.ExpiresAt.IsZero() {
		return runtime.Started{}, fmt.Errorf("bad order %+v", o)
	}
	l.reqs = append(l.reqs, r)
	l.rts = append(l.rts, rt)
	step := strings.Split(r.Key, ".")[1]
	if err := l.fail[step]; err != nil {
		return runtime.Started{}, err
	}
	n := len(l.reqs)
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: rt, ID: "local"}, ID: "ws-" + r.Item.Key}, ID: fmt.Sprintf("term-%d", n)}
	return runtime.Started{Session: s, PaneKey: fmt.Sprintf("pane-%d", n), TurnStarted: l.turn}, nil
}

func (l *launcher) Stop(_ context.Context, _ string, _ runtime.Order, s wire.SessionRef) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stops = append(l.stops, s)
	return nil
}

func (l *launcher) last() wire.LaunchRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[len(l.reqs)-1]
}

func (l *launcher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

type picker string

func (p picker) Pick(_ context.Context, id string) (string, error) {
	if id == "" {
		return string(p), nil
	}
	if id != "orca" && id != "oversire" {
		return "", errors.New("no such runtime: " + id)
	}
	return id, nil
}

// book is a fake session book. set sends the id to the watchers.
type book struct {
	mu   sync.Mutex
	byID map[string]wire.DaemonSession
	subs []chan string
}

func newFakeBook() *book { return &book{byID: map[string]wire.DaemonSession{}} }

func (b *book) Get(id string) (wire.DaemonSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.byID[id]
	return s, ok
}

func (b *book) ByKey(rt, key string) (wire.DaemonSession, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.byID {
		if s.Key == key && s.Runtime == rt {
			return s, true
		}
	}
	return wire.DaemonSession{}, false
}

func (b *book) Watch() (<-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := make(chan string, 16)
	b.subs = append(b.subs, c)
	return c, func() {}
}

func (b *book) set(s wire.DaemonSession) {
	b.mu.Lock()
	b.byID[s.ID] = s
	subs := b.subs
	b.mu.Unlock()
	for _, c := range subs {
		c <- s.ID
	}
}

func (b *book) drop(id string) {
	b.mu.Lock()
	delete(b.byID, id)
	b.mu.Unlock()
}

type rig struct {
	e      *Engine
	l      *launcher
	b      *book
	c      *clock
	dir    string
	mu     sync.Mutex
	frames []wire.RunbookRun
}

func newRig(t *testing.T, dir string) *rig {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	g := &rig{l: &launcher{fail: map[string]error{}, turn: true}, b: newFakeBook(), c: &clock{t: t0}, dir: dir}
	g.e = g.reopen()
	return g
}

// reopen makes a new engine on the same folder, as a daemon restart does.
func (g *rig) reopen() *Engine {
	e := NewEngine(NewStore(g.dir), g.l, picker("orca"), g.b, "mbp", func(r wire.RunbookRun) {
		g.mu.Lock()
		g.frames = append(g.frames, r)
		g.mu.Unlock()
	}, g.c.now)
	e.grace = 0
	g.e = e
	return e
}

func (g *rig) frameCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.frames)
}

// run gives the run after the background launches end.
func (g *rig) run(t *testing.T, id string) wire.RunbookRun {
	t.Helper()
	g.e.wg.Wait()
	r, ok := g.e.Get(id)
	if !ok {
		t.Fatalf("no run %s", id)
	}
	return r
}

func (g *rig) done(t *testing.T, id, step string, v wire.Verdict, note string) wire.RunbookRun {
	t.Helper()
	g.e.wg.Wait()
	if _, err := g.e.Done(id, step, wire.StepDone{Verdict: v, Note: note}); err != nil {
		t.Fatalf("done %s: %v", step, err)
	}
	return g.run(t, id)
}

var item = wire.ItemRef{Project: wire.ProjectRef{Account: wire.AccountID{Provider: "plane", ID: "ocral"}, ID: "p1", Key: "OVERSIRE"},
	ID: "i1", Key: "OVERSIRE-341", URL: "https://app.plane.so/ocral/browse/OVERSIRE-341/"}

var agents = map[string]wire.AgentLaunch{
	"claude":     {CLI: "claude", Command: "claude -n OVERSIRE-341", Profile: "claude"},
	"claude-max": {CLI: "claude", Model: "claude-opus-5-5", Effort: "max", Profile: "claude-max"},
}

func prompt(s string) string { return s + " Then run {done} --verdict done." }

// small is implement, review (fix: fixes, rounds), fixes.
func small(rounds int) wire.Runbook {
	return wire.Runbook{ID: "small", Name: "Small", Steps: []wire.RunbookStep{
		{ID: "implement", Name: "Implement", Agent: "claude", Prompt: prompt("Implement {key}.")},
		{ID: "review", Name: "Review Implementation", Agent: "claude-max", Prompt: prompt("Review {key}, round {round}. Notes: {notes}."), Review: &wire.Review{Fix: "fixes", Rounds: rounds}},
		{ID: "fixes", Name: "Implement Fixes", Agent: "claude-max", Prompt: prompt("Fix {key}.")},
	}}
}

func linear() wire.Runbook {
	return wire.Runbook{ID: "two", Name: "Two", Steps: []wire.RunbookStep{
		{ID: "plan", Name: "Plan", Agent: "claude", Prompt: prompt("Plan {key}.")},
		{ID: "build", Name: "Build", Agent: "claude-max", Prompt: prompt("Build {key}.")},
	}}
}

func (g *rig) start(t *testing.T, b wire.Runbook) wire.RunbookRun {
	t.Helper()
	return g.startFor(t, b, item.Key)
}

func (g *rig) startFor(t *testing.T, b wire.Runbook, key string) wire.RunbookRun {
	t.Helper()
	it := item
	it.Key = key
	r, err := g.e.Start(context.Background(), wire.RunbookStart{Item: it, Title: "Retry a sync", Repo: "/r/app", Runbook: b, Agents: agents})
	if err != nil {
		t.Fatal(err)
	}
	return g.run(t, r.ID)
}

// trail is "step.round:state" for each step run.
func trail(r wire.RunbookRun) string {
	var parts []string
	for _, s := range r.Steps {
		parts = append(parts, fmt.Sprintf("%s.%d:%s", s.Step, s.Round, s.State))
	}
	return strings.Join(parts, " ")
}

func expect(t *testing.T, r wire.RunbookRun, state wire.RunState, current, steps string) {
	t.Helper()
	if r.State != state || r.Current != current || trail(r) != steps {
		t.Fatalf("run %s current %q reason %q\nsteps %s\nwant  %s %q %s", r.State, r.Current, r.Reason, trail(r), state, current, steps)
	}
}

func kind(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

func TestLinearRunToDone(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	expect(t, r, wire.RunRunning, "plan", "plan.1:working")
	if r.Machine != "mbp" || r.Runtime != "orca" || !strings.HasPrefix(r.ID, "rb_") || len(r.ID) != 11 {
		t.Fatalf("run %+v", r)
	}
	sr := r.Steps[0]
	if sr.DaemonSession != "pane-1" || sr.Terminal != "term-1" || sr.Session == nil || sr.Session.ID != "term-1" || sr.Started == nil {
		t.Fatalf("step run %+v", sr)
	}
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "3 sub-items.")
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")
	if r.Steps[0].Note != "3 sub-items." || r.Steps[0].Ended == nil {
		t.Fatalf("plan %+v", r.Steps[0])
	}
	r = g.done(t, r.ID, "build", wire.VerdictDone, "")
	expect(t, r, wire.RunDone, "", "plan.1:done build.1:done")
	if g.l.count() != 2 || g.frameCount() < 3 {
		t.Fatalf("%d launches, %d frames", g.l.count(), g.frameCount())
	}
	if _, err := g.e.Done(r.ID, "build", wire.StepDone{Verdict: wire.VerdictDone}); kind(err) != Conflict {
		t.Fatalf("done on a done run: %v", err)
	}
}

func TestLaunchRequest(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, small(1))
	req := g.l.last()
	want := wire.LaunchRequest{Repo: "/r/app", Item: &item, Agent: "claude", Command: "claude -n OVERSIRE-341", Profile: "claude",
		Title: "OVERSIRE-341 · 1 Implement", Key: "runbook:" + r.ID + ".implement.1",
		Prompt: "Implement OVERSIRE-341. Then run oversire step done --run " + r.ID + " --step implement --verdict done."}
	if req.Item == nil || *req.Item != item {
		t.Fatalf("item %+v", req.Item)
	}
	req.Item = want.Item
	if req != want {
		t.Fatalf("request\n%+v\nwant\n%+v", req, want)
	}
	g.done(t, r.ID, "implement", wire.VerdictDone, "Store first.")
	req = g.l.last()
	if req.Agent != "claude" || req.Model != "claude-opus-5-5" || req.Effort != "max" || req.Profile != "claude-max" || req.Command != "" {
		t.Fatalf("agent fields %+v", req)
	}
	if req.Title != "OVERSIRE-341 · 2 Review Implementation" || !strings.Contains(req.Prompt, "round 1. Notes: Implement: Store first..") {
		t.Fatalf("review request %q %q", req.Title, req.Prompt)
	}
	g.done(t, r.ID, "review", wire.VerdictGaps, "No test.")
	g.done(t, r.ID, "fixes", wire.VerdictDone, "Added the test.")
	req = g.l.last()
	if req.Title != "OVERSIRE-341 · 2 Review Implementation · r2" || req.Key != "runbook:"+r.ID+".review.2" ||
		!strings.Contains(req.Prompt, "round 2. Notes: Implement: Store first.; Review Implementation: No test.; Implement Fixes: Added the test..") {
		t.Fatalf("round 2 request %q %q %q", req.Title, req.Key, req.Prompt)
	}
}

func TestReviewPassSkipsTheFix(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, small(1))
	g.done(t, r.ID, "implement", wire.VerdictDone, "")
	r = g.done(t, r.ID, "review", wire.VerdictPass, "No gaps.")
	expect(t, r, wire.RunDone, "", "implement.1:done review.1:pass fixes.1:skipped")
}

func TestGapsFixAndReviewAgain(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, small(1))
	g.done(t, r.ID, "implement", wire.VerdictDone, "")
	r = g.done(t, r.ID, "review", wire.VerdictGaps, "No test.")
	expect(t, r, wire.RunRunning, "fixes", "implement.1:done review.1:gaps fixes.1:working")
	r = g.done(t, r.ID, "fixes", wire.VerdictDone, "")
	expect(t, r, wire.RunRunning, "review", "implement.1:done review.1:gaps fixes.1:done review.2:working")
	r = g.done(t, r.ID, "review", wire.VerdictPass, "")
	expect(t, r, wire.RunDone, "", "implement.1:done review.1:gaps fixes.1:done review.2:pass")
}

func TestGapsOverTheLimitPause(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, small(1))
	g.done(t, r.ID, "implement", wire.VerdictDone, "")
	g.done(t, r.ID, "review", wire.VerdictGaps, "")
	g.done(t, r.ID, "fixes", wire.VerdictDone, "")
	r = g.done(t, r.ID, "review", wire.VerdictGaps, "Still no test.")
	expect(t, r, wire.RunPaused, "review", "implement.1:done review.1:gaps fixes.1:done review.2:gaps")
	if r.Reason != "Review still finds gaps after 1 round." {
		t.Fatal(r.Reason)
	}
	// Resume gives one more round.
	if _, err := g.e.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "fixes", "implement.1:done review.1:gaps fixes.1:done review.2:gaps fixes.2:working")

	g = newRig(t, "")
	r = g.start(t, small(0))
	g.done(t, r.ID, "implement", wire.VerdictDone, "")
	r = g.done(t, r.ID, "review", wire.VerdictGaps, "")
	if r.State != wire.RunPaused || r.Reason != "Review finds gaps, and the runbook gives it no fix round." {
		t.Fatalf("rounds 0: %s %q", r.State, r.Reason)
	}
}

func TestGateAndApprove(t *testing.T) {
	g := newRig(t, "")
	b := linear()
	b.Steps[1].PauseBefore = true
	r := g.start(t, b)
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
	expect(t, r, wire.RunGate, "build", "plan.1:done build.1:waits")
	if g.l.count() != 1 {
		t.Fatal("the gate launched")
	}
	if _, err := g.e.Done(r.ID, "build", wire.StepDone{Verdict: wire.VerdictDone}); kind(err) != Conflict {
		t.Fatalf("done at the gate: %v", err)
	}
	for _, f := range []func(string) (wire.RunbookRun, error){g.e.Pause, g.e.Resume} {
		if _, err := f(r.ID); kind(err) != Conflict {
			t.Fatalf("pause or resume at the gate: %v", err)
		}
	}
	if _, err := g.e.Approve(r.ID); err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")
	if _, err := g.e.Approve(r.ID); kind(err) != Conflict {
		t.Fatalf("approve of a running run: %v", err)
	}
}

func TestBlockedPausesAndResumeRunsANewRound(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	r = g.done(t, r.ID, "plan", wire.VerdictBlocked, "The item has no scope.")
	expect(t, r, wire.RunPaused, "plan", "plan.1:blocked")
	if r.Reason != "The item has no scope." {
		t.Fatal(r.Reason)
	}
	if _, err := g.e.Resume(r.ID); err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "plan", "plan.1:blocked plan.2:working")
	if g.l.last().Title != "OVERSIRE-341 · 1 Plan · r2" {
		t.Fatal(g.l.last().Title)
	}
}

func TestSessionFollow(t *testing.T) {
	g := newRig(t, "")
	g.l.turn = false
	r := g.start(t, linear())
	expect(t, r, wire.RunRunning, "plan", "plan.1:starting")
	cases := []struct {
		s    wire.DaemonSession
		want wire.StepState
	}{
		{wire.DaemonSession{State: wire.PhaseDone, Reason: wire.ReasonTurnEnd}, wire.StepStarting}, // the start of Claude, before the prompt
		{wire.DaemonSession{State: wire.PhaseWorking}, wire.StepWorking},
		{wire.DaemonSession{State: wire.PhaseWaiting, Reason: wire.ReasonQuestion}, wire.StepAsks},
		{wire.DaemonSession{State: wire.PhaseWaiting, Reason: wire.ReasonPermission}, wire.StepAsks},
		{wire.DaemonSession{State: wire.PhaseWaiting, Reason: wire.ReasonTurnEnd}, wire.StepWorking},
		{wire.DaemonSession{State: wire.PhaseDone, Reason: wire.ReasonQuestion, Prompt: "x"}, wire.StepAsks},
		{wire.DaemonSession{State: wire.PhaseDone, Reason: wire.ReasonTurnEnd, Prompt: "x"}, wire.StepEnded},
	}
	for _, c := range cases {
		c.s.ID, c.s.Runtime = "pane-1", "orca"
		g.b.set(c.s)
		g.e.check()
		r = g.run(t, r.ID)
		if r.Steps[0].State != c.want {
			t.Fatalf("%s %s: %s, want %s", c.s.State, c.s.Reason, r.Steps[0].State, c.want)
		}
	}
	if r.State != wire.RunPaused || r.Reason != reasonEnded {
		t.Fatalf("run %s %q", r.State, r.Reason)
	}
	// A late verdict of the session that ended goes on.
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:starting")
}

func TestSessionGoneOrFailed(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	g.b.set(wire.DaemonSession{ID: "pane-1", Runtime: "orca", State: wire.PhaseWorking})
	g.b.drop("pane-1")
	g.e.check()
	if r = g.run(t, r.ID); r.Steps[0].State != wire.StepWorking {
		t.Fatal("a gone session in the sync time ended the step")
	}
	g.c.add(SyncWait)
	g.e.check()
	r = g.run(t, r.ID)
	expect(t, r, wire.RunPaused, "plan", "plan.1:ended")

	g = newRig(t, "")
	r = g.start(t, linear())
	g.b.set(wire.DaemonSession{ID: "pane-1", Runtime: "oversire", State: wire.PhaseFailed, Reason: wire.ReasonError})
	g.e.check()
	r = g.run(t, r.ID)
	expect(t, r, wire.RunPaused, "plan", "plan.1:ended")
}

func TestLaunchFails(t *testing.T) {
	g := newRig(t, "")
	g.l.fail["build"] = errors.New("Orca has no workspace at /r/app")
	r := g.start(t, linear())
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
	expect(t, r, wire.RunPaused, "build", "plan.1:done build.1:failed")
	if r.Reason != "The step did not start: Orca has no workspace at /r/app" {
		t.Fatal(r.Reason)
	}
	delete(g.l.fail, "build")
	g.e.Resume(r.ID)
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:failed build.2:working")
}

func TestStop(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	r, err := g.e.Stop(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, r, wire.RunStopped, "plan", "plan.1:ended")
	if len(g.l.stops) != 1 || g.l.stops[0].ID != "term-1" {
		t.Fatalf("stops %+v", g.l.stops)
	}
	if _, err := g.e.Stop(context.Background(), r.ID); kind(err) != Conflict {
		t.Fatalf("stop again: %v", err)
	}
	if _, err := g.e.Stop(context.Background(), "rb_00000000"); kind(err) != NotFound {
		t.Fatalf("stop of no run: %v", err)
	}
}

func TestRetryAddsARound(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	if _, err := g.e.Retry(r.ID, "plan"); kind(err) != Conflict {
		t.Fatalf("retry of a live step: %v", err)
	}
	g.done(t, r.ID, "plan", wire.VerdictDone, "")
	r = g.done(t, r.ID, "build", wire.VerdictBlocked, "No access.")
	if _, err := g.e.Retry(r.ID, "plan"); err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "plan", "plan.1:done build.1:blocked plan.2:working")
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:blocked plan.2:done build.2:working")
	if _, err := g.e.Retry(r.ID, "nope"); kind(err) != NotFound {
		t.Fatalf("retry of no step: %v", err)
	}
}

func TestSkip(t *testing.T) {
	g := newRig(t, "")
	b := linear()
	b.Steps = append(b.Steps, wire.RunbookStep{ID: "ship", Name: "Ship", Agent: "claude", Prompt: prompt("Ship.")})
	r := g.start(t, b)
	if _, err := g.e.Skip(r.ID, "build"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.e.Skip(r.ID, "plan"); err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "ship", "plan.1:skipped build.1:skipped ship.1:working")
	if len(g.l.stops) != 1 || g.l.stops[0].ID != "term-1" {
		t.Fatalf("the skip did not stop the session: %+v", g.l.stops)
	}
	if _, err := g.e.Skip(r.ID, "plan"); kind(err) != Conflict {
		t.Fatalf("skip of a past step: %v", err)
	}
	r = g.done(t, r.ID, "ship", wire.VerdictDone, "")
	expect(t, r, wire.RunDone, "", "plan.1:skipped build.1:skipped ship.1:done")
}

func TestPatch(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	if _, err := g.e.Patch(r.ID, "plan", wire.StepPatch{Prompt: prompt("Other.")}); kind(err) != Conflict {
		t.Fatalf("patch after the start: %v", err)
	}
	if _, err := g.e.Patch(r.ID, "build", wire.StepPatch{Agent: "codex"}); kind(err) != Invalid {
		t.Fatalf("patch with an unknown agent: %v", err)
	}
	_, err := g.e.Patch(r.ID, "build", wire.StepPatch{Prompt: "No done {nope}."})
	var e *Error
	if !errors.As(err, &e) || e.Kind != Invalid || len(e.Problems) != 2 || e.Problems[0].Step != "build" {
		t.Fatalf("patch with a bad prompt: %v", err)
	}
	if _, err := g.e.Patch(r.ID, "build", wire.StepPatch{}); kind(err) != Invalid {
		t.Fatalf("empty patch: %v", err)
	}
	if _, err := g.e.Patch(r.ID, "nope", wire.StepPatch{Agent: "claude"}); kind(err) != NotFound {
		t.Fatalf("patch of no step: %v", err)
	}
	r, err = g.e.Patch(r.ID, "build", wire.StepPatch{Agent: "codex", Launch: &wire.AgentLaunch{CLI: "codex", Profile: "codex"}, Prompt: prompt("Build only the store of {key}.")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Runbook.Steps[1].Agent != "codex" || r.Agents["codex"].CLI != "codex" {
		t.Fatalf("patched run %+v", r.Runbook.Steps[1])
	}
	g.done(t, r.ID, "plan", wire.VerdictDone, "")
	if req := g.l.last(); req.Agent != "codex" || req.Profile != "codex" || !strings.HasPrefix(req.Prompt, "Build only the store of OVERSIRE-341.") {
		t.Fatalf("request after the patch %+v", req)
	}
}

func TestStartRefusals(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	_, err := g.e.Start(context.Background(), wire.RunbookStart{Item: item, Repo: "/r/app", Runbook: linear(), Agents: agents})
	if kind(err) != Conflict || !strings.Contains(err.Error(), r.ID) {
		t.Fatalf("second open run: %v", err)
	}
	bad := linear()
	bad.Steps[1].Agent = "nobody"
	_, err = g.e.Start(context.Background(), wire.RunbookStart{Item: item, Repo: "/r/app", Runbook: bad, Agents: agents})
	var e *Error
	if !errors.As(err, &e) || e.Kind != Invalid || len(e.Problems) != 1 || e.Problems[0].Step != "build" {
		t.Fatalf("bad agent: %v %+v", err, e)
	}
	other := item
	other.Key = "OVERSIRE-9"
	if _, err := g.e.Start(context.Background(), wire.RunbookStart{Item: other, Repo: "/r/app", Runtime: "conductor", Runbook: linear(), Agents: agents}); kind(err) != Invalid {
		t.Fatalf("unknown runtime: %v", err)
	}
	g.e.Stop(context.Background(), r.ID)
	g.c.add(time.Minute)
	if _, err := g.e.Start(context.Background(), wire.RunbookStart{Item: item, Repo: "/r/app", Runtime: "oversire", Runbook: linear(), Agents: agents}); err != nil {
		t.Fatalf("a run after a stopped run: %v", err)
	}
	g.e.wg.Wait()
	if g.l.rts[len(g.l.rts)-1] != "oversire" {
		t.Fatal(g.l.rts)
	}
	if list := g.e.List("OVERSIRE-341"); len(list) != 2 || list[0].Runtime != "oversire" {
		t.Fatalf("list %+v", list)
	}
}

func TestStartAt(t *testing.T) {
	g := newRig(t, "")
	r, err := g.e.Start(context.Background(), wire.RunbookStart{Item: item, Repo: "/r/app", Runbook: small(1), Agents: agents, StartAt: "review", Notes: "Implement: done by hand."})
	if err != nil {
		t.Fatal(err)
	}
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "review", "implement.1:skipped review.1:working")
	if p := g.l.last().Prompt; !strings.Contains(p, "Notes: Implement: done by hand..") {
		t.Fatal(p)
	}
	if _, err := g.e.Start(context.Background(), wire.RunbookStart{Item: wire.ItemRef{Key: "X-1"}, Repo: "/r", Runbook: small(1), Agents: agents, StartAt: "nope"}); kind(err) != Invalid {
		t.Fatalf("start at no step: %v", err)
	}
}

func TestWrongVerdicts(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, small(1))
	for _, c := range []struct {
		step string
		v    wire.Verdict
		want Kind
	}{
		{"implement", wire.VerdictPass, Invalid},
		{"implement", "maybe", Invalid},
		{"review", wire.VerdictPass, Conflict},
		{"nope", wire.VerdictDone, NotFound},
	} {
		if _, err := g.e.Done(r.ID, c.step, wire.StepDone{Verdict: c.v}); kind(err) != c.want {
			t.Errorf("%s %s: %v", c.step, c.v, err)
		}
	}
	_, err := g.e.Done(r.ID, "review", wire.StepDone{Verdict: wire.VerdictPass})
	if err == nil || err.Error() != "The step review is not the current step. The current step is implement (Implement)." {
		t.Fatal(err)
	}
	g.done(t, r.ID, "implement", wire.VerdictDone, "")
	if _, err := g.e.Done(r.ID, "review", wire.StepDone{Verdict: wire.VerdictDone}); kind(err) != Invalid {
		t.Fatalf("done on a review: %v", err)
	}
	r = g.done(t, r.ID, "review", wire.VerdictGaps, strings.Repeat("é", 400))
	if n := len([]rune(r.Steps[1].Note)); n != MaxNote {
		t.Fatalf("note of %d characters", n)
	}
}

func TestPauseAfterTheCurrentStep(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	r, err := g.e.Pause(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, r, wire.RunPaused, "plan", "plan.1:working")
	if _, err := g.e.Pause(r.ID); kind(err) != Conflict {
		t.Fatalf("pause again: %v", err)
	}
	r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
	expect(t, r, wire.RunPaused, "plan", "plan.1:done")
	if g.l.count() != 1 || r.Reason != reasonPaused {
		t.Fatalf("%d launches, reason %q", g.l.count(), r.Reason)
	}
	g.e.Resume(r.ID)
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")

	// Resume before the step ends: the step goes on.
	g.e.Pause(r.ID)
	g.e.Resume(r.ID)
	r = g.run(t, r.ID)
	expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")
}

func TestOversireSessionStopsAfterTheVerdict(t *testing.T) {
	g := newRig(t, "")
	r, _ := g.e.Start(context.Background(), wire.RunbookStart{Item: item, Repo: "/r/app", Runtime: "oversire", Runbook: linear(), Agents: agents})
	g.done(t, r.ID, "plan", wire.VerdictDone, "")
	if len(g.l.stops) != 1 || g.l.stops[0].ID != "term-1" {
		t.Fatalf("stops %+v", g.l.stops)
	}
	g = newRig(t, "")
	r = g.start(t, linear())
	g.done(t, r.ID, "plan", wire.VerdictDone, "")
	if len(g.l.stops) != 0 {
		t.Fatal("an Orca terminal closed after the verdict")
	}
}

func TestRestart(t *testing.T) {
	t.Run("starting with no session", func(t *testing.T) {
		g := newRig(t, "")
		g.l.fail["plan"] = errors.New("crash")
		r := g.start(t, linear())
		// The daemon stopped during the launch: the file says starting, with no session.
		raw, _ := g.e.Get(r.ID)
		raw.State, raw.Reason, raw.Steps[0].State, raw.Steps[0].Ended = wire.RunRunning, "", wire.StepStarting, nil
		g.e.store.Save(raw)
		e := g.reopen()
		e.check()
		if r = g.run(t, r.ID); r.Steps[0].State != wire.StepStarting {
			t.Fatal("paused before the wait")
		}
		g.b.set(wire.DaemonSession{ID: "p9", Runtime: "orca", Key: stepKey(r.ID, "plan", 1), State: wire.PhaseWorking})
		e.check()
		if r = g.run(t, r.ID); r.Steps[0].DaemonSession != "p9" || r.Steps[0].State != wire.StepWorking {
			t.Fatalf("the key did not find the session: %+v", r.Steps[0])
		}

		g.b.drop("p9")
		raw.Steps[0].State = wire.StepStarting
		g.e.store.Save(raw)
		e = g.reopen()
		g.c.add(StartWait)
		e.check()
		r = g.run(t, r.ID)
		expect(t, r, wire.RunPaused, "plan", "plan.1:failed")
		if r.Reason != reasonNoStart {
			t.Fatal(r.Reason)
		}
	})
	t.Run("working", func(t *testing.T) {
		g := newRig(t, "")
		r := g.start(t, linear())
		e := g.reopen()
		e.check()
		if r = g.run(t, r.ID); r.Steps[0].State != wire.StepWorking {
			t.Fatal("ended in the sync time")
		}
		g.b.set(wire.DaemonSession{ID: "pane-1", Runtime: "orca", State: wire.PhaseWaiting, Reason: wire.ReasonQuestion})
		g.c.add(SyncWait)
		e.check()
		if r = g.run(t, r.ID); r.Steps[0].State != wire.StepAsks {
			t.Fatal(trail(r))
		}
		r = g.done(t, r.ID, "plan", wire.VerdictDone, "")
		expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")

		e = g.reopen()
		g.c.add(SyncWait)
		e.check()
		r = g.run(t, r.ID)
		expect(t, r, wire.RunPaused, "build", "plan.1:done build.1:ended")
	})
	t.Run("gate, paused, done and stopped", func(t *testing.T) {
		g := newRig(t, "")
		b := linear()
		b.Steps[1].PauseBefore = true
		gate := g.start(t, b)
		g.done(t, gate.ID, "plan", wire.VerdictDone, "")
		raw := g.startFor(t, linear(), "OVERSIRE-2")
		e := g.reopen()
		if _, err := e.Pause(raw.ID); err != nil {
			t.Fatal(err)
		}
		e.Done(raw.ID, "plan", wire.StepDone{Verdict: wire.VerdictDone})
		e.wg.Wait()

		e = g.reopen()
		g.c.add(time.Hour)
		e.check()
		r := g.run(t, gate.ID)
		expect(t, r, wire.RunGate, "build", "plan.1:done build.1:waits")
		r = g.run(t, raw.ID)
		expect(t, r, wire.RunPaused, "plan", "plan.1:done")
		e.Approve(gate.ID)
		r = g.run(t, gate.ID)
		expect(t, r, wire.RunRunning, "build", "plan.1:done build.1:working")
		e.Resume(raw.ID)
		r = g.done(t, raw.ID, "build", wire.VerdictDone, "")
		expect(t, r, wire.RunDone, "", "plan.1:done build.1:done")
		e.Stop(context.Background(), gate.ID)

		e = g.reopen()
		e.check()
		if r = g.run(t, raw.ID); r.State != wire.RunDone {
			t.Fatal(r.State)
		}
		if r = g.run(t, gate.ID); r.State != wire.RunStopped {
			t.Fatal(r.State)
		}
		if n := g.l.count(); n != 4 {
			t.Fatalf("%d launches", n)
		}
	})
}

func TestRunWakesOnTheBook(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.e.Run(ctx)
	g.b.set(wire.DaemonSession{ID: "pane-1", Runtime: "orca", State: wire.PhaseWaiting, Reason: wire.ReasonPermission})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ = g.e.Get(r.ID); r.Steps[0].State == wire.StepAsks {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(trail(r))
}

func TestStoreKeepsEachRun(t *testing.T) {
	g := newRig(t, "")
	r := g.start(t, linear())
	runs, err := NewStore(g.dir).Load()
	if err != nil || len(runs) != 1 || runs[0].ID != r.ID || runs[0].Updated.IsZero() {
		t.Fatalf("%+v %v", runs, err)
	}
	if runs, err := NewStore(t.TempDir() + "/none").Load(); err != nil || len(runs) != 0 {
		t.Fatalf("no folder: %v %v", runs, err)
	}
}

func TestPatchKeepsTheStartCopy(t *testing.T) {
	g := newRig(t, "")
	b := linear()
	r := g.start(t, b)
	if _, err := g.e.Patch(r.ID, "build", wire.StepPatch{Agent: "x", Launch: &wire.AgentLaunch{CLI: "codex"}, Prompt: prompt("Other.")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := agents["x"]; ok || b.Steps[1].Prompt != prompt("Build {key}.") {
		t.Fatal("the patch changed the start")
	}
}
