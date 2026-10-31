package runbook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	// StartWait is how long a step can be in starting with no session. Then the run pauses.
	// The oversire runtime gives the session at the launch, so its steps do not wait (until OVERSIRE-362 for Orca).
	StartWait = 2 * time.Minute
	// SyncWait is the time after the daemon start in which the sources report the sessions again.
	// After it, a step whose session the book does not know ends. The oversire runtime reports its sessions
	// before the engine starts, so its steps do not wait.
	SyncWait = 60 * time.Second
	// MaxNote is the length of a step note in characters.
	MaxNote   = 300
	tick      = 30 * time.Second
	launchTTL = 30 * time.Minute
	// doneGrace is the time between a verdict and the stop of an oversire session, so that the agent can end its turn.
	doneGrace = 10 * time.Second
)

// The reasons of a paused run.
const (
	reasonPaused  = "Paused by you."
	reasonEnded   = "The session ended with no step done."
	reasonNoStart = "The step session did not start."
	reasonStopped = "Stopped by you."
)

// Launcher starts and stops the step sessions on a runtime of this machine (orders.Runner).
type Launcher interface {
	Launch(ctx context.Context, rt string, o runtime.Order, r wire.LaunchRequest) (runtime.Started, error)
	Stop(ctx context.Context, rt string, o runtime.Order, s wire.SessionRef) error
}

// Picker gives the runtime of a run (runtime.Registry).
type Picker interface {
	Pick(ctx context.Context, id string) (string, error)
}

// Sessions is the session book (sessions.Book).
type Sessions interface {
	Get(id string) (wire.DaemonSession, bool)
	ByKey(runtime, key string) (wire.DaemonSession, bool)
	Watch() (<-chan string, func())
}

// Kind is the kind of an Error. The routes give it as the HTTP status.
type Kind int

const (
	NotFound Kind = iota + 1
	Conflict
	Invalid
)

// Error is a refused call. Problems are the broken rules of a runbook.
type Error struct {
	Kind     Kind
	Text     string
	Problems []Problem
}

func (e *Error) Error() string { return e.Text }

func refuse(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Text: fmt.Sprintf(format, a...)}
}

// Engine runs each runbook run as a state machine. It keeps the runs in the store, so a run goes on after a restart.
type Engine struct {
	Log *slog.Logger

	store    *Store
	launch   Launcher
	runtimes Picker
	sessions Sessions
	machine  string
	notify   func(wire.RunbookRun)
	clock    func() time.Time
	since    time.Time
	grace    time.Duration

	mu        sync.Mutex
	runs      map[string]*wire.RunbookRun
	launching map[string]bool // the start keys of the launches that run now
	wg        sync.WaitGroup
}

// NewEngine loads the runs of the store. notify gets each change of a run.
func NewEngine(store *Store, launch Launcher, runtimes Picker, sessions Sessions, machine string, notify func(wire.RunbookRun), clock func() time.Time) *Engine {
	if clock == nil {
		clock = time.Now
	}
	if notify == nil {
		notify = func(wire.RunbookRun) {}
	}
	e := &Engine{Log: slog.Default(), store: store, launch: launch, runtimes: runtimes, sessions: sessions, machine: machine,
		notify: notify, clock: clock, since: clock(), grace: doneGrace, runs: map[string]*wire.RunbookRun{}, launching: map[string]bool{}}
	runs, err := store.Load()
	if err != nil {
		e.Log.Warn("runbook load", "err", err)
	}
	for i := range runs {
		e.runs[runs[i].ID] = &runs[i]
	}
	return e
}

// Run follows the sessions of the live steps: on each change in the book and each 30 s.
func (e *Engine) Run(ctx context.Context) error {
	ids, cancel := e.sessions.Watch()
	defer cancel()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		e.check()
		select {
		case <-ctx.Done():
			return nil
		case <-ids:
		case <-t.C:
		}
	}
}

func (e *Engine) check() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for _, r := range e.runs {
		if r.State.Open() && e.follow(r, now) {
			e.changed(r)
		}
	}
}

// Start checks s, makes the run and starts its first step.
func (e *Engine) Start(ctx context.Context, s wire.RunbookStart) (wire.RunbookRun, error) {
	if p := validateStart(s); len(p) > 0 {
		return wire.RunbookRun{}, &Error{Kind: Invalid, Text: "The runbook is not valid.", Problems: p}
	}
	rt, err := e.runtimes.Pick(ctx, s.Runtime)
	if err != nil {
		return wire.RunbookRun{}, refuse(Invalid, "%s", err.Error())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.runs {
		if r.State.Open() && r.Item.Key == s.Item.Key {
			return wire.RunbookRun{}, refuse(Conflict, "%s has the open run %s.", s.Item.Key, r.ID)
		}
	}
	id := newID()
	for e.runs[id] != nil {
		id = newID()
	}
	now := e.now()
	r := &wire.RunbookRun{ID: id, Item: s.Item, Title: s.Title, Machine: e.machine, Runtime: rt, Repo: s.Repo, Runbook: s.Runbook,
		Agents: s.Agents, Notes: s.Notes, State: wire.RunRunning, Steps: []wire.StepRun{}, Started: now, Updated: now}
	first := max(r.Runbook.Index(s.StartAt), 0)
	for _, st := range r.Runbook.Steps[:first] {
		r.Steps = append(r.Steps, wire.StepRun{Step: st.ID, Round: 1, State: wire.StepSkipped})
	}
	*r = clone(r) // the run owns its runbook and agents, so a patch does not change the caller's copy
	e.runs[id] = r
	e.begin(r, r.Runbook.Steps[first].ID, 1)
	e.changed(r)
	return clone(r), nil
}

// Done sets the verdict of the current step and goes to the next step.
// A step whose session ended with no verdict takes a late verdict too.
func (e *Engine) Done(run, step string, d wire.StepDone) (wire.RunbookRun, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, st, err := e.step(run, step)
	if err != nil {
		return wire.RunbookRun{}, err
	}
	if step != r.Current {
		cur := r.Current
		if c := r.Runbook.Step(cur); c != nil {
			cur = fmt.Sprintf("%s (%s)", cur, c.Name)
		}
		return wire.RunbookRun{}, refuse(Conflict, "The step %s is not the current step. The current step is %s.", step, cur)
	}
	sr := last(r, step)
	if sr == nil || !sr.State.Live() && sr.State != wire.StepEnded {
		return wire.RunbookRun{}, refuse(Conflict, "The step %s does not run now.", step)
	}
	state, err := verdictState(*st, d.Verdict)
	if err != nil {
		return wire.RunbookRun{}, err
	}
	live := sr.State.Live()
	now := e.now()
	sr.State, sr.Note, sr.Ended = state, clip(strings.TrimSpace(d.Note), MaxNote), &now
	if live && r.Runtime == wire.RuntimeOversire && sr.Session != nil {
		e.stop(r.Runtime, *sr.Session, e.grace)
	}
	switch {
	case state == wire.StepBlocked:
		reason := sr.Note
		if reason == "" {
			reason = "The agent says that the step is blocked."
		}
		e.pause(r, reason)
	case live && r.State == wire.RunPaused:
		// Paused by the user: the run stops after this step.
		if state == wire.StepGaps && sr.Round > st.Review.Rounds {
			r.Reason = roundsText(st.Review.Rounds)
		}
	default:
		r.State, r.Reason = wire.RunRunning, ""
		e.advance(r, false)
	}
	e.changed(r)
	return clone(r), nil
}

// Approve starts the step that waits at the gate.
func (e *Engine) Approve(run string) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		if r.State != wire.RunGate {
			return refuse(Conflict, "The run does not wait at a gate. It is %s.", r.State)
		}
		e.begin(r, r.Current, nextRound(r, r.Current))
		return nil
	})
}

// Pause pauses the run. A live step goes on, and the run stops after it.
func (e *Engine) Pause(run string) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		if r.State != wire.RunRunning {
			return refuse(Conflict, "Only a running run can pause. The run is %s.", r.State)
		}
		e.pause(r, reasonPaused)
		return nil
	})
}

// Resume goes on with a paused run. A step that ended, failed or is blocked starts again as a new round.
// After the round limit of a review, the fix step and the review run one more round.
func (e *Engine) Resume(run string) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		if r.State != wire.RunPaused {
			return refuse(Conflict, "The run is not paused. It is %s.", r.State)
		}
		sr := last(r, r.Current)
		switch {
		case sr != nil && sr.State.Live():
			r.State, r.Reason = wire.RunRunning, ""
		case sr == nil || sr.State == wire.StepEnded || sr.State == wire.StepFailed || sr.State == wire.StepBlocked || sr.State == wire.StepWaits:
			e.begin(r, r.Current, nextRound(r, r.Current))
		default:
			r.State, r.Reason = wire.RunRunning, ""
			e.advance(r, true)
		}
		return nil
	})
}

// Stop ends the run and stops the session of the live step. The workspace stays.
func (e *Engine) Stop(ctx context.Context, run string) (wire.RunbookRun, error) {
	e.mu.Lock()
	r, err := e.get(run)
	if err != nil {
		e.mu.Unlock()
		return wire.RunbookRun{}, err
	}
	if !r.State.Open() {
		e.mu.Unlock()
		return wire.RunbookRun{}, refuse(Conflict, "The run is %s already.", r.State)
	}
	var session *wire.SessionRef
	if sr := live(r); sr != nil {
		now := e.now()
		sr.State, sr.Ended, session = wire.StepEnded, &now, sr.Session
	}
	r.State, r.Reason = wire.RunStopped, reasonStopped
	e.changed(r)
	out, rt := clone(r), r.Runtime
	e.mu.Unlock()
	if session != nil {
		if err := e.launch.Stop(ctx, rt, e.order("stop"), *session); err != nil {
			e.Log.Warn("runbook stop", "run", run, "err", err)
		}
	}
	return out, nil
}

// Retry starts a new round of the step. The step becomes the current step.
func (e *Engine) Retry(run, step string) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		if r.Runbook.Step(step) == nil {
			return refuse(NotFound, "The runbook has no step %s.", step)
		}
		if sr := live(r); sr != nil {
			return refuse(Conflict, "The step %s runs now. Stop or skip it first.", sr.Step)
		}
		e.begin(r, step, nextRound(r, step))
		return nil
	})
}

// Skip skips the current step, or a later step before it starts.
func (e *Engine) Skip(run, step string) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		i := r.Runbook.Index(step)
		if i < 0 {
			return refuse(NotFound, "The runbook has no step %s.", step)
		}
		now := e.now()
		if step != r.Current {
			if i < r.Runbook.Index(r.Current) {
				return refuse(Conflict, "The step %s is done. Retry it to run it again.", step)
			}
			r.Steps = append(r.Steps, wire.StepRun{Step: step, Round: nextRound(r, step), State: wire.StepSkipped})
			return nil
		}
		sr := last(r, step)
		switch {
		case sr == nil || final(sr.State):
			r.Steps = append(r.Steps, wire.StepRun{Step: step, Round: nextRound(r, step), State: wire.StepSkipped, Ended: &now})
		default:
			if sr.State.Live() && sr.Session != nil {
				e.stop(r.Runtime, *sr.Session, 0)
			}
			sr.State, sr.Ended = wire.StepSkipped, &now
		}
		r.State, r.Reason = wire.RunRunning, ""
		e.advance(r, false)
		return nil
	})
}

// Patch changes the agent or the prompt of a step that did not start, in the copy of the run.
func (e *Engine) Patch(run, step string, p wire.StepPatch) (wire.RunbookRun, error) {
	return e.change(run, func(r *wire.RunbookRun) error {
		st := r.Runbook.Step(step)
		if st == nil {
			return refuse(NotFound, "The runbook has no step %s.", step)
		}
		for _, sr := range r.Steps {
			if sr.Step == step && sr.Started != nil {
				return refuse(Conflict, "The step %s started. Only a step that did not start can change.", step)
			}
		}
		switch {
		case p.Agent == "" && p.Launch != nil:
			return refuse(Invalid, "A launch needs an agent name.")
		case p.Agent == "" && p.Prompt == "":
			return refuse(Invalid, "The patch changes nothing.")
		case p.Agent != "" && p.Launch == nil:
			if _, ok := r.Agents[p.Agent]; !ok {
				return refuse(Invalid, "The run has no agent %s. Send its launch too.", p.Agent)
			}
		}
		if p.Prompt != "" {
			if probs := Validate(wire.Runbook{Name: r.Runbook.Name, Steps: []wire.RunbookStep{{ID: step, Prompt: p.Prompt}}}); len(probs) > 0 {
				return &Error{Kind: Invalid, Text: "The prompt is not valid.", Problems: probs}
			}
			st.Prompt = p.Prompt
		}
		if p.Launch != nil {
			if r.Agents == nil {
				r.Agents = map[string]wire.AgentLaunch{}
			}
			r.Agents[p.Agent] = *p.Launch
		}
		if p.Agent != "" {
			st.Agent = p.Agent
		}
		return nil
	})
}

// List gives the runs of the item key, or all runs for an empty item, newest first.
func (e *Engine) List(item string) []wire.RunbookRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []wire.RunbookRun{}
	for _, r := range e.runs {
		if item == "" || r.Item.Key == item {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (e *Engine) Get(run string) (wire.RunbookRun, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.runs[run]
	if !ok {
		return wire.RunbookRun{}, false
	}
	return clone(r), true
}

// change runs f on an open run under the lock, then saves and tells the change.
func (e *Engine) change(run string, f func(r *wire.RunbookRun) error) (wire.RunbookRun, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.get(run)
	if err != nil {
		return wire.RunbookRun{}, err
	}
	if !r.State.Open() {
		return wire.RunbookRun{}, refuse(Conflict, "The run is %s.", r.State)
	}
	if err := f(r); err != nil {
		return wire.RunbookRun{}, err
	}
	e.changed(r)
	return clone(r), nil
}

func (e *Engine) get(run string) (*wire.RunbookRun, error) {
	r, ok := e.runs[run]
	if !ok {
		return nil, refuse(NotFound, "There is no run %s.", run)
	}
	return r, nil
}

// step gives the open run and its step.
func (e *Engine) step(run, step string) (*wire.RunbookRun, *wire.RunbookStep, error) {
	r, err := e.get(run)
	if err != nil {
		return nil, nil, err
	}
	if !r.State.Open() {
		return nil, nil, refuse(Conflict, "The run is %s.", r.State)
	}
	st := r.Runbook.Step(step)
	if st == nil {
		return nil, nil, refuse(NotFound, "The runbook has no step %s.", step)
	}
	return r, st, nil
}

// advance goes to the step after the current one, which has a final state. force passes the round limit of a review.
func (e *Engine) advance(r *wire.RunbookRun, force bool) {
	i := r.Runbook.Index(r.Current)
	st := r.Runbook.Steps[i]
	sr := last(r, st.ID)
	if st.Review != nil && sr.State == wire.StepGaps {
		if sr.Round > st.Review.Rounds && !force {
			e.pause(r, roundsText(st.Review.Rounds))
			return
		}
		e.begin(r, st.Review.Fix, sr.Round)
		return
	}
	if review := fixOf(r.Runbook, st.ID); review != nil {
		if sr.State == wire.StepComplete {
			e.begin(r, review.ID, sr.Round+1)
			return
		}
		i = r.Runbook.Index(review.ID)
	}
	for _, next := range r.Runbook.Steps[i+1:] {
		prev := last(r, next.ID)
		if fixOf(r.Runbook, next.ID) != nil || prev != nil && prev.State == wire.StepSkipped {
			if prev == nil {
				r.Steps = append(r.Steps, wire.StepRun{Step: next.ID, Round: 1, State: wire.StepSkipped})
			}
			continue
		}
		if next.PauseBefore {
			r.Current, r.State, r.Reason = next.ID, wire.RunGate, ""
			r.Steps = append(r.Steps, wire.StepRun{Step: next.ID, Round: nextRound(r, next.ID), State: wire.StepWaits})
			return
		}
		e.begin(r, next.ID, nextRound(r, next.ID))
		return
	}
	r.Current, r.State, r.Reason = "", wire.RunDone, ""
}

// begin makes the step round current and starts its session in the background.
func (e *Engine) begin(r *wire.RunbookRun, step string, round int) {
	now := e.now()
	// A retry of another step drops the round that waits at the gate.
	if w := last(r, r.Current); w != nil && w.State == wire.StepWaits && r.Current != step {
		gate := *w
		r.Steps = slices.DeleteFunc(r.Steps, func(x wire.StepRun) bool { return x == gate })
	}
	sr := wire.StepRun{Step: step, Round: round, State: wire.StepStarting, Started: &now}
	if l := last(r, step); l != nil && l.State == wire.StepWaits && l.Round == round {
		*l = sr
	} else {
		r.Steps = append(r.Steps, sr)
	}
	r.Current, r.State, r.Reason = step, wire.RunRunning, ""
	req := e.request(r, step, round)
	e.launching[req.Key] = true
	e.wg.Add(1)
	go e.start(r.ID, step, round, r.Runtime, req)
}

func (e *Engine) start(run, step string, round int, rt string, req wire.LaunchRequest) {
	defer e.wg.Done()
	order := runtime.Order{ID: "runbook-" + strings.TrimPrefix(req.Key, "runbook:"), ExpiresAt: e.clock().Add(launchTTL)}
	st, err := e.launch.Launch(context.Background(), rt, order, req)
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.launching, req.Key)
	r := e.runs[run]
	var sr *wire.StepRun
	if r != nil {
		sr = roundOf(r, step, round)
	}
	if sr == nil || sr.State != wire.StepStarting {
		// The run went on while the session started: a stop or a skip.
		if err == nil {
			e.stop(rt, st.Session, 0)
		}
		return
	}
	now := e.now()
	if err != nil {
		e.Log.Warn("runbook launch", "run", run, "step", step, "round", round, "err", err)
		sr.State, sr.Ended = wire.StepFailed, &now
		e.pause(r, "The step did not start: "+err.Error())
		e.changed(r)
		return
	}
	s := st.Session
	sr.Session, sr.Terminal, sr.DaemonSession = &s, s.ID, st.PaneKey
	if sr.DaemonSession == "" {
		if ds, ok := e.sessions.ByKey(rt, req.Key); ok {
			sr.DaemonSession = ds.ID
		}
	}
	if st.TurnStarted {
		sr.State = wire.StepWorking
	}
	e.follow(r, now)
	e.changed(r)
}

// follow sets the state of the live step from its session. It tells if the run changed.
func (e *Engine) follow(r *wire.RunbookRun, now time.Time) bool {
	sr := live(r)
	if sr == nil {
		return false
	}
	before := *sr
	key := stepKey(r.ID, sr.Step, sr.Round)
	wait := r.Runtime != wire.RuntimeOversire
	if sr.DaemonSession == "" {
		if e.launching[key] {
			return false
		}
		if ds, ok := e.sessions.ByKey(r.Runtime, key); ok {
			sr.DaemonSession = ds.ID
		} else if !wait || sr.Started == nil || now.Sub(*sr.Started) >= StartWait {
			sr.State, sr.Ended = wire.StepFailed, &now
			e.pause(r, reasonNoStart)
			return true
		} else {
			return false
		}
	}
	s, ok := e.sessions.Get(sr.DaemonSession)
	if !ok {
		// The sources report a session some time after the daemon start and after the launch.
		if wait && (now.Sub(e.since) < SyncWait || sr.Started != nil && now.Sub(*sr.Started) < SyncWait) {
			return *sr != before
		}
		e.end(r, sr, now)
		return true
	}
	switch s.State {
	case wire.PhaseWorking:
		sr.State = wire.StepWorking
	case wire.PhaseWaiting:
		sr.State = wire.StepWorking
		if s.Reason == wire.ReasonQuestion || s.Reason == wire.ReasonPermission {
			sr.State = wire.StepAsks
		}
	case wire.PhaseDone:
		// A Claude session is done before its first prompt. Only a session with a prompt ended its turn.
		switch {
		case s.Reason == wire.ReasonQuestion:
			sr.State = wire.StepAsks
		case s.Runtime == wire.RuntimeOversire || s.Prompt != "":
			e.end(r, sr, now)
			return true
		}
	case wire.PhaseFailed:
		e.end(r, sr, now)
		return true
	}
	return *sr != before
}

// end ends a live step that has no verdict and pauses the run.
func (e *Engine) end(r *wire.RunbookRun, sr *wire.StepRun, now time.Time) {
	sr.State, sr.Ended = wire.StepEnded, &now
	e.pause(r, reasonEnded)
}

func (e *Engine) pause(r *wire.RunbookRun, reason string) {
	r.State, r.Reason = wire.RunPaused, reason
}

// stop stops a step session in the background after wait.
func (e *Engine) stop(rt string, s wire.SessionRef, wait time.Duration) {
	e.wg.Go(func() {
		time.Sleep(wait)
		if err := e.launch.Stop(context.Background(), rt, e.order("stop"), s); err != nil {
			e.Log.Warn("runbook session stop", "session", s.ID, "err", err)
		}
	})
}

func (e *Engine) order(kind string) runtime.Order {
	return runtime.Order{ID: "runbook-" + kind + "-" + randomHex(8), ExpiresAt: e.clock().Add(launchTTL)}
}

func (e *Engine) request(r *wire.RunbookRun, step string, round int) wire.LaunchRequest {
	st := r.Runbook.Step(step)
	a := r.Agents[st.Agent]
	title := fmt.Sprintf("%s · %d %s", r.Item.Key, r.Runbook.Index(step)+1, st.Name)
	if round > 1 {
		title += fmt.Sprintf(" · r%d", round)
	}
	item := r.Item
	return wire.LaunchRequest{Repo: r.Repo, Item: &item, Agent: a.CLI, Command: a.Command, Model: a.Model, Effort: a.Effort, Profile: a.Profile,
		Title: title, Key: stepKey(r.ID, step, round), Prompt: render(r, *st, round)}
}

// changed saves the run and tells the change.
func (e *Engine) changed(r *wire.RunbookRun) {
	r.Updated = e.now()
	if err := e.store.Save(*r); err != nil {
		e.Log.Error("runbook save", "run", r.ID, "err", err)
	}
	e.notify(clone(r))
}

func (e *Engine) now() time.Time { return e.clock().UTC().Truncate(time.Second) }

// stepKey is the start key of a step round. Run and step ids have no dot, so the key parses back.
func stepKey(run, step string, round int) string {
	return fmt.Sprintf("runbook:%s.%s.%d", run, step, round)
}

func verdictState(st wire.RunbookStep, v wire.Verdict) (wire.StepState, error) {
	if v == wire.VerdictBlocked {
		return wire.StepBlocked, nil
	}
	if st.Review != nil {
		switch v {
		case wire.VerdictPass:
			return wire.StepPass, nil
		case wire.VerdictGaps:
			return wire.StepGaps, nil
		}
		return "", refuse(Invalid, "The review step %s takes the verdict pass, gaps or blocked.", st.ID)
	}
	switch v {
	case wire.VerdictDone:
		return wire.StepComplete, nil
	case wire.VerdictSkipped:
		return wire.StepSkipped, nil
	}
	return "", refuse(Invalid, "The step %s takes the verdict done, skipped or blocked.", st.ID)
}

func roundsText(rounds int) string {
	switch rounds {
	case 0:
		return "Review finds gaps, and the runbook gives it no fix round."
	case 1:
		return "Review still finds gaps after 1 round."
	}
	return fmt.Sprintf("Review still finds gaps after %d rounds.", rounds)
}

// final is true for a state with a verdict.
func final(s wire.StepState) bool {
	return s == wire.StepPass || s == wire.StepGaps || s == wire.StepComplete || s == wire.StepSkipped
}

// last gives the last round of the step, else nil.
func last(r *wire.RunbookRun, step string) *wire.StepRun {
	for i := len(r.Steps) - 1; i >= 0; i-- {
		if r.Steps[i].Step == step {
			return &r.Steps[i]
		}
	}
	return nil
}

func roundOf(r *wire.RunbookRun, step string, round int) *wire.StepRun {
	for i := len(r.Steps) - 1; i >= 0; i-- {
		if r.Steps[i].Step == step && r.Steps[i].Round == round {
			return &r.Steps[i]
		}
	}
	return nil
}

// live gives the round of the current step when its session is open, else nil.
func live(r *wire.RunbookRun) *wire.StepRun {
	if sr := last(r, r.Current); sr != nil && sr.State.Live() {
		return sr
	}
	return nil
}

// nextRound is the round after the last round of the step. A round that waits at a gate does not count.
func nextRound(r *wire.RunbookRun, step string) int {
	n := 0
	for _, sr := range r.Steps {
		if sr.Step == step && sr.State != wire.StepWaits {
			n = max(n, sr.Round)
		}
	}
	return n + 1
}

// clone is a deep copy, so that a caller can read it outside the lock.
func clone(r *wire.RunbookRun) wire.RunbookRun {
	var out wire.RunbookRun
	b, _ := json.Marshal(r)
	_ = json.Unmarshal(b, &out)
	return out
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func newID() string { return "rb_" + randomHex(4) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
