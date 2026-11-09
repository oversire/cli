//go:build !windows

package oversire

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/review"
	"github.com/oversire/cli/wire"
)

// session is the OverSire side of one visor session: the order, the item, the transcript,
// the open requests with their expiry, and the state of the reports.
type session struct {
	id  string
	rt  *Runtime
	dir string
	// workdir is the visor workdir id of a session that this daemon started.
	workdir string
	// req is the launch request. It is empty for a session from before a restart of the daemon.
	req wire.LaunchRequest
	// overseer is the session id of the overseer. An overseen session hides its asks until the overseer escalates.
	overseer string
	cancel   context.CancelFunc
	// ended closes when the session is done or failed.
	ended chan struct{}

	mu       sync.Mutex
	info     wire.RunnerSession
	vid      string
	vstate   agentvisor.State
	vendorID string
	// slot is true while the session holds a slot.
	slot bool
	// moving is true while a handover runs.
	moving bool
	// live is false during the replay of the events at Boot: no reports and no expiry timers.
	live    bool
	log     *transcript
	turns   *turnLog
	partial string
	last    string
	lastErr string
	exitErr string
	// lastInput is the text of the last user input. output is true when agent events came after it.
	lastInput string
	output    bool
	// failure fails the session at its exit, also after a clean exit.
	failure     string
	stopped     bool
	permissions []*openPermission
	// question is the open AskUserQuestion request. rounds counts the questions of the session.
	question *openQuestion
	rounds   int
	// tokens counts Result.Usage of each turn.
	tokens int64
	// escalated shows the open asks to the user, also when the session is overseen.
	escalated bool

	// scan keeps one paths scan at a time. lastPaths is the set of the last report.
	scan      sync.Mutex
	lastPaths []string
}

func newSession(rt *Runtime, info wire.RunnerSession, dir string) *session {
	return &session{
		id: info.ID, rt: rt, dir: dir, info: info, cancel: func() {}, ended: make(chan struct{}),
		log: &transcript{}, turns: newTurnLog(),
	}
}

// sessionOf makes a session from the labels of a visor session that this daemon did not start.
func sessionOf(rt *Runtime, vs agentvisor.Session) *session {
	l := vs.Labels
	repo := l[labelRepo]
	var item *wire.ItemRef
	if raw := l[labelItem]; raw != "" {
		var it wire.ItemRef
		if json.Unmarshal([]byte(raw), &it) == nil {
			item = &it
		}
	}
	info := wire.RunnerSession{
		ID: l[LabelSession], Runner: l[labelRunner], Order: l[labelOrder], Agent: vs.Harness, Repo: repo, Branch: l[labelBranch], Item: item,
		Session:   sessionRef(repo, l[labelKey], l[LabelSession]),
		State:     wire.SessionStarting,
		StartedAt: vs.CreatedAt, UpdatedAt: vs.UpdatedAt,
	}
	s := newSession(rt, info, vs.WorkingDir)
	s.vstate, s.vendorID = vs.State, vs.VendorID
	s.overseer = l[labelOverseer]
	return s
}

func sessionRef(repo, key, id string) wire.SessionRef {
	return wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: Local}, ID: filepath.Base(repo) + "/" + key}, ID: id}
}

// run waits for a slot and starts the session in the visor. A session with no slot at expires fails.
func (s *session) run(ctx context.Context, expires time.Time) {
	wait, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.cancel = cancel
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		cancel()
	}
	if !expires.IsZero() {
		var stop func()
		wait, stop = context.WithDeadline(wait, expires)
		defer stop()
	}
	if err := s.rt.slots.Acquire(wait); err != nil {
		s.end("The order expired before a session slot was free.")
		return
	}
	s.mu.Lock()
	s.slot = true
	if s.stopped {
		s.finish("")
		s.mu.Unlock()
		return
	}
	s.change(wire.SessionStarting)
	s.mu.Unlock()
	vs, err := s.rt.host.Start(ctx, s.spec())
	if err != nil {
		s.end(err.Error())
		return
	}
	s.rt.bind(s, vs)
	s.mu.Lock()
	stopped = s.stopped
	s.mu.Unlock()
	if stopped {
		_ = s.rt.host.Stop(ctx, vs.ID)
	}
}

func (s *session) spec() agentvisor.StartSpec {
	labels := map[string]string{
		LabelRuntime: ID, LabelSession: s.id, labelOrder: s.info.Order, labelRepo: s.info.Repo, labelBranch: s.info.Branch,
		labelKey: s.key(), labelRunner: s.info.Runner,
	}
	mode := agentwire.PermissionAsk
	promptKind := ""
	if s.overseer != "" {
		// Mode auto: the library allows each request, so the worker does not stop at each edit.
		labels[labelOverseer], mode, promptKind = s.overseer, agentwire.PermissionAuto, string(wire.OriginOverseer)
	}
	if s.info.Item != nil {
		if data, err := json.Marshal(s.info.Item); err == nil {
			labels[labelItem] = string(data)
		}
	}
	return agentvisor.StartSpec{
		Kind: agentvisor.KindWire, Title: s.key(), Harness: s.req.Agent, Model: s.req.Model, Effort: s.req.Effort,
		Workdir: s.workdir, WorkingDir: s.dir, Instructions: s.instructions(), Prompt: s.req.Prompt,
		PromptKind:  promptKind,
		Permissions: agentvisor.Permissions{Mode: mode},
		Labels:      labels,
		Env:         s.env(),
		Restart:     agentvisor.RestartPolicy{Mode: agentvisor.RestartResume, Prompt: restartPrompt},
	}
}

// env is the env of the launch, then OVERSIRE_SESSION. The hook command drops the calls of a session with it,
// so the book has no copy of it.
func (s *session) env() map[string]string {
	env := maps.Clone(s.req.Env)
	if env == nil {
		env = map[string]string{}
	}
	env["OVERSIRE_SESSION"] = s.id
	return env
}

// key is the workspace key: the part of the workspace id after the repo name.
func (s *session) key() string {
	_, key, _ := strings.Cut(s.info.Session.Workspace.ID, "/")
	return key
}

func (s *session) instructions() string {
	text := fmt.Sprintf("You work in the git worktree %s on the branch %s. Commit your changes to this branch.", s.dir, s.info.Branch)
	if s.req.InPlace {
		text = fmt.Sprintf("You work in the folder %s.", s.dir)
	}
	if s.req.Item != nil {
		text += fmt.Sprintf(" The task is %s: %s", s.req.Item.Key, s.req.Item.URL)
	}
	return text
}

// apply changes the session for one visor event.
func (s *session) apply(e agentvisor.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.VendorID != "" {
		s.vendorID = e.VendorID
	}
	if s.info.State.Ended() {
		return
	}
	if !e.At.IsZero() {
		s.info.UpdatedAt = e.At.UTC()
	}
	switch e.Type {
	case agentvisor.EventSessionState:
		s.vstate = e.State
		s.onState()
	case agentvisor.EventUser:
		// A user event with no Input mark is the echo of the vendor. A send that the visor writes
		// again after a state error gives the same input twice, with no agent output between.
		if e.Input && (e.Text != s.lastInput || s.output) {
			s.lastInput, s.output = e.Text, false
			s.flushPartial()
			s.log.add("> " + e.Text)
			s.addTurn(wire.RoleUser, e.Text, e.At, originOf(e.Kind))
		}
	case agentvisor.EventRequestClosed:
		s.closeRequest(e.RequestID)
	default:
		if ev, ok := e.Wire(); ok {
			s.output = true
			s.handle(ev, e.At)
		}
	}
}

// onState follows a new visor state. Call it with s.mu held.
func (s *session) onState() {
	switch s.vstate {
	case agentvisor.StateExited:
		s.finish(cmp.Or(s.failure, s.exitErr))
	case agentvisor.StateLost:
		if s.live && s.vendorID == "" {
			s.lose()
			return
		}
		s.show()
	case agentvisor.StateStarting, agentvisor.StateRunning:
		s.exitErr = ""
		s.show()
	default:
		s.show()
	}
}

// lose fails a session that the visor lost and cannot resume. Call it with s.mu held.
func (s *session) lose() {
	vid := s.vid
	s.finish("The agent process stopped before it gave a session id, so the session cannot resume.")
	go func() { _ = s.rt.host.Stop(context.Background(), vid) }()
}

// handle changes the session for one agent event. Call it with s.mu held.
func (s *session) handle(ev agentwire.Event, at time.Time) {
	if ev.Type == agentwire.EventAssistant && ev.Delta {
		s.partial += ev.Text
		return
	}
	// The full block repeats its deltas (claude, codex). Keep only the deltas after it.
	if rest, ok := strings.CutPrefix(s.partial, ev.Text); ok && ev.Type == agentwire.EventAssistant && ev.Text != "" {
		s.partial = rest
	} else {
		s.flushPartial()
	}
	switch ev.Type {
	case agentwire.EventAssistant:
		s.addText(ev.Text, at)
		// The agent went on, for example after an answer in a terminal.
		s.clearQuestion()
	case agentwire.EventTool:
		if ev.Tool == nil {
			break
		}
		if ev.Tool.Status == "started" {
			s.log.add("· " + ev.Tool.Name + " " + clip(ev.Tool.Input, 200))
		}
		s.turns.tool(ev.Tool, at.UTC())
	case agentwire.EventPermission:
		if ev.Permission == nil {
			break
		}
		if ev.Permission.Tool == askTool && s.askQuestion(ev.Permission, at) {
			break
		}
		s.ask(ev.Permission, at)
	case agentwire.EventResult:
		text := s.last
		if text == "" && ev.Result != nil {
			text = ev.Result.Text
		}
		s.info.Summary = tail(strings.TrimSpace(text), maxText)
		s.info.Error = ""
		if ev.Result != nil {
			// EventUsage of Codex has the total of the thread, so only the usage of the turn counts.
			s.tokens += ev.Result.Usage.Input + ev.Result.Usage.Output
			if ev.Result.IsError {
				s.info.Error = ev.Result.Text
			}
		}
		// The visor closes the open requests at the end of a turn.
		s.dropRequests()
		s.last = ""
		s.show()
		s.publish()
		s.scanPaths()
	case agentwire.EventError:
		s.lastErr = cmp.Or(ev.Error, ev.Code)
		s.log.add("! " + s.lastErr)
		s.addTurn(wire.RoleSystem, s.lastErr, at, "")
	case agentwire.EventExit:
		// The last classified error tells more than the exit status.
		if ev.Error != "" {
			s.exitErr = cmp.Or(s.lastErr, ev.Error)
		}
	}
}

// wake ends the replay of the session: it arms the expiry timers and reports the session.
func (s *session) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live = true
	if s.info.State.Ended() {
		// The report keeps the time of the end.
		if s.rt.report != nil {
			s.rt.report.Report(s.info)
		}
		return
	}
	for _, p := range s.permissions {
		s.arm(p)
	}
	switch {
	case s.vstate == agentvisor.StateExited:
		s.finish(cmp.Or(s.failure, s.exitErr))
	case s.vstate == agentvisor.StateLost && s.vendorID == "":
		s.lose()
	default:
		s.show()
		s.publish()
	}
}

// show sets the state from the visor state and the open requests, and reports a change.
// Call it with s.mu held.
func (s *session) show() {
	if s.info.State.Ended() || s.vid == "" && s.vstate == "" {
		return
	}
	if len(s.permissions) > 0 {
		p := s.permissions[0].prompt
		s.info.Permission = &p
	} else {
		s.info.Permission = nil
	}
	state := wire.SessionWorking
	switch {
	case s.vstate == agentvisor.StateStarting || s.vstate == agentvisor.StateLost:
		state = wire.SessionStarting
	case len(s.permissions) > 0:
		state = wire.SessionWaitingPermission
	case s.question != nil:
		state = wire.SessionWaitingInput
	case s.vstate == agentvisor.StateIdle || s.vstate == agentvisor.StateSuspended:
		state = wire.SessionWaitingInput
	}
	if state != s.info.State {
		s.change(state)
	}
}

// addText keeps one assistant text block. Call it with s.mu held.
func (s *session) addText(text string, at time.Time) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.last = text
	s.log.add(text)
	s.addTurn(wire.RoleAssistant, text, at, "")
}

// addTurn adds a turn to the chat view. Call it with s.mu held.
func (s *session) addTurn(role wire.TurnRole, text string, at time.Time, origin wire.TurnOrigin) {
	s.turns.add(wire.SessionTurn{Role: role, At: at.UTC(), Text: text, Origin: origin})
}

// originOf maps the kind of a visor input to the origin of a user turn.
func originOf(kind string) wire.TurnOrigin {
	switch kind {
	case string(wire.OriginOverseer):
		return wire.OriginOverseer
	case agentvisor.InputKindRestart:
		return wire.OriginSystem
	}
	return wire.OriginUser
}

func (s *session) flushPartial() {
	if s.partial != "" {
		s.addText(s.partial, s.rt.now())
		s.partial = ""
	}
}

// started gives the visor session id, or an error when the session ended or has no visor session yet.
func (s *session) started() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.State.Ended() {
		return "", fmt.Errorf("The session %s has ended.", s.id)
	}
	if s.vid == "" {
		return "", fmt.Errorf("The session %s has not started yet.", s.id)
	}
	return s.vid, nil
}

// say sends a user message. A message during a turn joins the turn or waits for its end.
// The visor resumes a suspended session first. An empty origin means user.
func (s *session) say(ctx context.Context, text string, origin wire.TurnOrigin) error {
	vid, err := s.started()
	if err != nil {
		return err
	}
	// A send at the end of the holder can see the old state (agent-visor 0.2.1). It goes again.
	for range 5 {
		if _, err = s.rt.host.Send(ctx, vid, agentvisor.Input{Text: text, Kind: string(cmp.Or(origin, wire.OriginUser))}); !errors.Is(err, agentvisor.ErrState) {
			return err
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *session) interrupt(ctx context.Context) error {
	vid, err := s.started()
	if err != nil {
		return err
	}
	return s.rt.host.Interrupt(ctx, vid)
}

func (s *session) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.info.State.Ended() {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	vid := s.vid
	cancel := s.cancel
	s.mu.Unlock()
	if vid == "" {
		cancel()
	} else if err := s.rt.host.Stop(ctx, vid); err != nil {
		return err
	}
	select {
	case <-s.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dropQueued fails a session that waits for a slot, because the daemon stops.
func (s *session) dropQueued() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vid == "" && !s.info.State.Ended() {
		s.finish("The daemon stopped before the session started.")
	}
}

// end makes the session done, or failed with reason. Only the first call has an effect.
func (s *session) end(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finish(reason)
}

// finish is end with s.mu held. A stopped session is done.
func (s *session) finish(reason string) {
	if s.info.State.Ended() {
		return
	}
	if s.stopped {
		reason = ""
	}
	s.flushPartial()
	s.dropRequests()
	state := wire.SessionDone
	if reason != "" {
		state = wire.SessionFailed
		s.info.Error = reason
		s.log.add("! " + reason)
		if reason != s.lastErr {
			s.addTurn(wire.RoleSystem, reason, s.rt.now(), "")
		}
	}
	s.change(state)
	s.turns.close()
	if s.slot {
		s.slot = false
		s.rt.slots.Release()
	}
	s.cancel()
	close(s.ended)
	s.scanPaths()
}

// scanPaths reports the changed paths in the background. Call it with s.mu held.
func (s *session) scanPaths() {
	if s.live {
		go s.reportPaths()
	}
}

// reportPaths sends the changed paths of an item session, only when the set changed since the last report.
func (s *session) reportPaths() {
	pr, ok := s.rt.report.(PathsReporter)
	s.mu.Lock()
	item, repo, order := s.info.Item, s.info.Repo, s.info.Order
	s.mu.Unlock()
	if !ok || item == nil || s.rt.git == nil {
		return
	}
	s.scan.Lock()
	defer s.scan.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	paths, err := review.NewGit(s.rt.git).Changed(ctx, filepath.Clean(repo), s.dir)
	if err != nil {
		return
	}
	p := wire.NewItemPaths(*item, paths, s.rt.now())
	if slices.Equal(p.Paths, s.lastPaths) {
		return
	}
	s.lastPaths = p.Paths
	pr.ReportPaths(order, p)
}

// change sets the state and reports it. Call it with s.mu held.
func (s *session) change(state wire.RunnerSessionState) {
	s.info.State = state
	s.publish()
}

// publish reports the session. Call it with s.mu held, so that the reports keep their order.
func (s *session) publish() {
	if !s.live {
		return
	}
	if s.info.Permission == nil && s.info.Question == nil {
		s.escalated = false
	}
	s.info.UpdatedAt = s.rt.now().UTC()
	if s.rt.report == nil {
		return
	}
	out := s.info
	if s.overseer != "" && !s.escalated {
		out.Permission, out.Question = nil, nil
	}
	s.rt.report.Report(out)
}
