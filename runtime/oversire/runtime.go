// Package oversire runs agent sessions without Orca. Each session gets a git worktree, and
// agent-wire drives the harness CLI over its structured protocol.
package oversire

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	ID                       = "oversire"
	DefaultSlots             = 2
	DefaultPermissionTimeout = 10 * time.Minute
	// Local is the workspace account id: the sessions run on this machine.
	Local = "local"

	keepLines = 1000
	maxText   = 500
	forget    = 24 * time.Hour
)

// Reporter receives each new state of a session. Report must not block.
type Reporter interface {
	Report(s wire.RunnerSession)
}

// ReportFunc is a Reporter.
type ReportFunc func(s wire.RunnerSession)

func (f ReportFunc) Report(s wire.RunnerSession) { f(s) }

// PathsReporter receives the changed paths of the worktree of an item session after each turn.
// A Reporter can also be a PathsReporter. ReportPaths must not block.
type PathsReporter interface {
	ReportPaths(order string, p wire.ChangedPaths)
}

// Harnesses starts agent-wire sessions. *agentwire.Runtime is the real one.
type Harnesses interface {
	Driver(h agentwire.Harness) (agentwire.Driver, bool)
	Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error)
}

type Options struct {
	// Runner is the device id of this runner.
	Runner string
	// DataDir holds worktrees/ and sessions/.
	DataDir string
	// Slots is the maximum of sessions that run at the same time. Zero is DefaultSlots.
	Slots int
	// PermissionTimeout is the time to answer a permission prompt. Zero is DefaultPermissionTimeout.
	PermissionTimeout time.Duration
	// Fake permits the agent "fake", for tests.
	Fake bool
}

type Runtime struct {
	opts      Options
	agents    Harnesses
	worktrees *Worktrees
	slots     *Slots
	report    Reporter
	now       func() time.Time
	// digests gives the transcript reader of a harness.
	digests func(harness string) (digest.Reader, error)

	mu       sync.Mutex
	sessions map[string]*session
}

func New(opts Options, agents Harnesses, git Runner, report Reporter) *Runtime {
	if opts.Slots <= 0 {
		opts.Slots = DefaultSlots
	}
	if opts.PermissionTimeout <= 0 {
		opts.PermissionTimeout = DefaultPermissionTimeout
	}
	return &Runtime{
		opts: opts, agents: agents, report: report, now: time.Now, digests: digest.ForUser,
		worktrees: NewWorktrees(filepath.Join(opts.DataDir, "worktrees"), git),
		slots:     NewSlots(opts.Slots),
		sessions:  map[string]*session{},
	}
}

func (rt *Runtime) ID() string { return ID }

var (
	workspaceKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sessionID    = regexp.MustCompile(`^s-[0-9a-f]+$`)
)

// Start makes the worktree and gives the session at once. The agent starts when a slot is free.
// A session that has no slot at o.ExpiresAt fails. agent-wire has no command line, so r.Command has no effect.
func (rt *Runtime) Start(ctx context.Context, o runtime.Order, r wire.LaunchRequest) (wire.SessionRef, error) {
	var none wire.SessionRef
	key := r.Name
	if r.Item != nil && r.Item.Key != "" {
		key = r.Item.Key
	}
	if !workspaceKey.MatchString(key) {
		return none, fmt.Errorf("The workspace name %q is not valid. Give an item or a name.", key)
	}
	if !filepath.IsAbs(r.Repo) {
		return none, fmt.Errorf("The repo %q is not a local path.", r.Repo)
	}
	h := agentwire.Harness(r.Agent)
	if _, ok := rt.agents.Driver(h); !ok || (h == agentwire.Fake && !rt.opts.Fake) {
		return none, fmt.Errorf("The oversire runtime does not know the agent %q.", r.Agent)
	}
	repo := filepath.Clean(r.Repo)
	branch := r.Branch
	if branch == "" {
		branch = strings.ToLower(key)
	}
	dir, err := rt.worktrees.Ensure(ctx, repo, key, branch)
	if err != nil {
		return none, err
	}
	id := "s-" + randomHex(8)
	log, err := openLog(rt.logPath(id))
	if err != nil {
		return none, err
	}
	turns, err := openTurns(rt.turnsPath(id))
	if err != nil {
		_ = log.Close()
		return none, err
	}
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: Local}, ID: filepath.Base(repo) + "/" + key}, ID: id}
	now := rt.now().UTC()
	s := &session{
		id: id, rt: rt, req: r, dir: dir, ended: make(chan struct{}),
		log: &transcript{file: log}, turns: turns,
		info: wire.RunnerSession{ID: id, Runner: rt.opts.Runner, Session: ref, Order: o.ID, Agent: r.Agent, Repo: r.Repo, Branch: branch, Item: r.Item, State: wire.SessionQueued, StartedAt: now, UpdatedAt: now},
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	rt.mu.Lock()
	rt.prune()
	rt.sessions[id] = s
	rt.mu.Unlock()

	s.mu.Lock()
	s.publish()
	s.mu.Unlock()
	go s.run(ctx, o.ExpiresAt)
	return ref, nil
}

func (rt *Runtime) Stop(ctx context.Context, ref wire.SessionRef) error {
	s, err := rt.session(ref)
	if err != nil {
		return err
	}
	return s.stop(ctx)
}

// Read gives the last lines of the transcript. After a restart of the runner it reads the transcript file.
func (rt *Runtime) Read(_ context.Context, ref wire.SessionRef, lines int) ([]string, error) {
	if s, err := rt.session(ref); err == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.log.tail(lines), nil
	}
	if !sessionID.MatchString(ref.ID) {
		return nil, fmt.Errorf("The oversire runtime has no session %s.", ref.ID)
	}
	f, err := os.Open(rt.logPath(ref.ID))
	if err != nil {
		return nil, fmt.Errorf("The oversire runtime has no session %s.", ref.ID)
	}
	defer f.Close()
	t := &transcript{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		t.add(sc.Text())
	}
	return t.tail(lines), sc.Err()
}

// Send answers the open permission prompt or question of p, else gives the text to the agent as a user message.
func (rt *Runtime) Send(ctx context.Context, p wire.SendPayload) error {
	s, err := rt.session(p.Session)
	if err != nil {
		return err
	}
	if p.Permission != nil {
		return s.answer(ctx, p.Permission.ID, p.Permission.Allow, "")
	}
	if p.Answer != nil {
		return s.answerQuestion(ctx, *p.Answer)
	}
	return s.say(ctx, p.Text)
}

// History reads the transcript of the harness session. After a restart of the runner it reads the session file.
func (rt *Runtime) History(_ context.Context, ref wire.SessionRef, b wire.HistoryBudget) (wire.SessionDigest, error) {
	var m sessionMeta
	s, err := rt.session(ref)
	if err == nil {
		s.mu.Lock()
		m = sessionMeta{Agent: s.req.Agent, HarnessSession: s.harnessID}
		s.mu.Unlock()
	} else if !sessionID.MatchString(ref.ID) {
		return wire.SessionDigest{}, err
	} else if data, rerr := os.ReadFile(rt.metaPath(ref.ID)); rerr != nil || json.Unmarshal(data, &m) != nil {
		return wire.SessionDigest{}, err
	}
	if m.HarnessSession == "" {
		return wire.SessionDigest{}, fmt.Errorf("The session %s has no %s session id yet.", ref.ID, m.Agent)
	}
	r, err := rt.digests(m.Agent)
	if err != nil {
		return wire.SessionDigest{}, err
	}
	d, err := r.Digest(m.HarnessSession, &b, wire.DigestFromRunner)
	if errors.Is(err, digest.ErrNoSession) {
		return d, fmt.Errorf("The runner found no %s transcript for the session %s (%s).", m.Agent, ref.ID, m.HarnessSession)
	}
	return d, err
}

// Close stops all sessions. The worktrees stay.
func (rt *Runtime) Close(ctx context.Context) {
	rt.mu.Lock()
	all := make([]*session, 0, len(rt.sessions))
	for _, s := range rt.sessions {
		all = append(all, s)
	}
	rt.mu.Unlock()
	for _, s := range all {
		_ = s.stop(ctx)
	}
}

// prune forgets the sessions that ended a day ago. Read still finds their transcript file.
// Call it with rt.mu held.
func (rt *Runtime) prune() {
	old := rt.now().Add(-forget)
	for id, s := range rt.sessions {
		s.mu.Lock()
		gone := s.info.State.Ended() && s.info.UpdatedAt.Before(old)
		s.mu.Unlock()
		if gone {
			delete(rt.sessions, id)
		}
	}
}

func (rt *Runtime) session(ref wire.SessionRef) (*session, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.sessions[ref.ID]
	if !ok {
		return nil, fmt.Errorf("The oversire runtime has no session %s.", ref.ID)
	}
	return s, nil
}

func (rt *Runtime) logPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".log")
}

func (rt *Runtime) metaPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".json")
}

// sessionMeta lets History find the harness session after a restart of the runner.
type sessionMeta struct {
	Agent          string `json:"agent"`
	HarnessSession string `json:"harnessSession"`
}

type session struct {
	id     string
	rt     *Runtime
	req    wire.LaunchRequest
	dir    string
	cancel func()
	// ended closes when the session is done or failed.
	ended chan struct{}

	mu      sync.Mutex
	info    wire.RunnerSession
	aw      agentwire.Session
	log     *transcript
	turns   *turnLog
	partial string
	last    string
	lastErr string
	// failure fails the session at its exit, also after a clean exit.
	failure     string
	stopped     bool
	permissions []*openPermission
	// question is the open AskUserQuestion request. rounds counts the questions of the session.
	question *openQuestion
	rounds   int
	// harnessID is the session id of the harness, for History.
	harnessID string

	// scan keeps one paths scan at a time. lastPaths is the set of the last report.
	scan      sync.Mutex
	lastPaths []string
}

func (s *session) run(ctx context.Context, expires time.Time) {
	wait := ctx
	if !expires.IsZero() {
		var cancel func()
		wait, cancel = context.WithDeadline(ctx, expires)
		defer cancel()
	}
	if err := s.rt.slots.Acquire(wait); err != nil {
		s.end("The order expired before a session slot was free.")
		return
	}
	defer s.rt.slots.Release()
	if !s.set(wire.SessionStarting) {
		s.end("")
		return
	}
	aw, err := s.rt.agents.Start(ctx, agentwire.StartRequest{
		Harness:      agentwire.Harness(s.req.Agent),
		WorkingDir:   s.dir,
		Instructions: s.instructions(),
		Permissions:  agentwire.PermissionPolicy{Mode: agentwire.PermissionAsk},
	})
	if err != nil {
		s.end(err.Error())
		return
	}
	s.mu.Lock()
	s.aw = aw
	stopped := s.stopped
	s.log.add("> " + s.req.Prompt)
	s.addTurn(wire.RoleUser, s.req.Prompt)
	s.mu.Unlock()
	if stopped {
		go aw.Close(ctx)
	} else if err := aw.Prompt(ctx, agentwire.Prompt{Text: s.req.Prompt}); err != nil {
		s.mu.Lock()
		s.failure = err.Error()
		s.mu.Unlock()
		go aw.Close(ctx)
	} else {
		s.set(wire.SessionWorking)
	}
	for ev := range aw.Events() {
		s.handle(ev)
	}
}

func (s *session) instructions() string {
	text := fmt.Sprintf("You work in the git worktree %s on the branch %s. Commit your changes to this branch.", s.dir, s.info.Branch)
	if s.req.Item != nil {
		text += fmt.Sprintf(" The task is %s: %s", s.req.Item.Key, s.req.Item.URL)
	}
	return text
}

func (s *session) handle(ev agentwire.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.SessionID != "" && ev.SessionID != s.harnessID {
		s.harnessID = ev.SessionID
		s.saveMeta()
	}
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
		s.addText(ev.Text)
		// The agent went on, for example after an answer in a terminal.
		s.clearQuestion()
		if s.info.State == wire.SessionWaitingInput {
			s.change(wire.SessionWorking)
		}
	case agentwire.EventTool:
		if ev.Tool == nil {
			break
		}
		if ev.Tool.Status == "started" {
			s.log.add("· " + ev.Tool.Name + " " + clip(ev.Tool.Input, 200))
		}
		s.turns.tool(ev.Tool, s.rt.now().UTC())
	case agentwire.EventPermission:
		if ev.Permission == nil {
			break
		}
		if ev.Permission.Tool == askTool && s.askQuestion(ev.Permission) {
			break
		}
		s.ask(ev.Permission)
	case agentwire.EventResult:
		text := s.last
		if text == "" && ev.Result != nil {
			text = ev.Result.Text
		}
		s.info.Summary = tail(strings.TrimSpace(text), maxText)
		s.info.Error = ""
		if ev.Result != nil && ev.Result.IsError {
			s.info.Error = ev.Result.Text
		}
		s.question = nil
		s.info.Question = nil
		if len(s.permissions) == 0 {
			s.info.State = wire.SessionWaitingInput
		}
		s.last = ""
		s.publish()
		go s.reportPaths()
	case agentwire.EventError:
		s.lastErr = ev.Error
		if s.lastErr == "" {
			s.lastErr = ev.Code
		}
		s.log.add("! " + s.lastErr)
		s.addTurn(wire.RoleSystem, s.lastErr)
	case agentwire.EventExit:
		// The last classified error tells more than the exit status.
		reason := s.failure
		if reason == "" && ev.Error != "" {
			reason = cmp.Or(s.lastErr, ev.Error)
		}
		s.finish(reason)
	}
}

// saveMeta writes the session file. Call it with s.mu held.
func (s *session) saveMeta() {
	data, _ := json.Marshal(sessionMeta{Agent: s.req.Agent, HarnessSession: s.harnessID})
	_ = os.WriteFile(s.rt.metaPath(s.id), data, 0o600)
}

// addText keeps one assistant text block.
func (s *session) addText(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.last = text
	s.log.add(text)
	s.addTurn(wire.RoleAssistant, text)
}

// addTurn adds a turn to the chat view. Call it with s.mu held.
func (s *session) addTurn(role wire.TurnRole, text string) {
	s.turns.add(wire.SessionTurn{Role: role, At: s.rt.now().UTC(), Text: text})
}

func (s *session) flushPartial() {
	if s.partial != "" {
		s.addText(s.partial)
		s.partial = ""
	}
}

// say sends a user message. A message during a turn joins the turn or waits for its end.
func (s *session) say(ctx context.Context, text string) error {
	s.mu.Lock()
	aw, state := s.aw, s.info.State
	s.mu.Unlock()
	if state.Ended() {
		return fmt.Errorf("The session %s has ended.", s.id)
	}
	if aw == nil {
		return fmt.Errorf("The session %s has not started yet.", s.id)
	}
	if _, err := agentwire.Send(ctx, aw, agentwire.Prompt{Text: text}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log.add("> " + text)
	s.addTurn(wire.RoleUser, text)
	if s.info.State == wire.SessionWaitingInput {
		s.change(wire.SessionWorking)
	}
	return nil
}

func (s *session) interrupt(ctx context.Context) error {
	s.mu.Lock()
	aw, state := s.aw, s.info.State
	s.mu.Unlock()
	if state.Ended() {
		return fmt.Errorf("The session %s has ended.", s.id)
	}
	if aw == nil {
		return fmt.Errorf("The session %s has not started yet.", s.id)
	}
	return aw.Interrupt(ctx)
}

func (s *session) stop(ctx context.Context) error {
	s.mu.Lock()
	if s.info.State.Ended() {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	aw := s.aw
	s.mu.Unlock()
	if aw == nil {
		s.cancel()
	} else {
		_ = aw.Close(ctx)
	}
	select {
	case <-s.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// set changes the state, unless the session is stopped. It gives false for a stopped session.
func (s *session) set(state wire.RunnerSessionState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.change(state)
	return true
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
	for _, p := range s.permissions {
		p.timer.Stop()
	}
	s.permissions = nil
	s.info.Permission = nil
	s.question = nil
	s.info.Question = nil
	state := wire.SessionDone
	if reason != "" {
		state = wire.SessionFailed
		s.info.Error = reason
		s.log.add("! " + reason)
		if reason != s.lastErr {
			s.addTurn(wire.RoleSystem, reason)
		}
	}
	s.change(state)
	s.log.close()
	s.turns.close()
	s.cancel()
	close(s.ended)
	go s.reportPaths()
}

// reportPaths sends the changed paths of an item session, only when the set changed since the last report.
func (s *session) reportPaths() {
	pr, ok := s.rt.report.(PathsReporter)
	if !ok || s.req.Item == nil {
		return
	}
	s.scan.Lock()
	defer s.scan.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	paths, err := s.rt.worktrees.Changed(ctx, filepath.Clean(s.req.Repo), s.dir)
	if err != nil {
		return
	}
	p := wire.NewItemPaths(*s.req.Item, paths, s.rt.now())
	if slices.Equal(p.Paths, s.lastPaths) {
		return
	}
	s.lastPaths = p.Paths
	pr.ReportPaths(s.info.Order, p)
}

// change sets the state and reports it. Call it with s.mu held.
func (s *session) change(state wire.RunnerSessionState) {
	s.info.State = state
	s.publish()
}

// publish reports the session. Call it with s.mu held, so that the reports keep their order.
func (s *session) publish() {
	s.info.UpdatedAt = s.rt.now().UTC()
	if s.rt.report != nil {
		s.rt.report.Report(s.info)
	}
}

// transcript keeps the last lines in memory and appends all lines to a file.
type transcript struct {
	lines []string
	file  *os.File
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

func (t *transcript) add(text string) {
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		t.lines = append(t.lines, l)
		if t.file != nil {
			_, _ = t.file.WriteString(l + "\n")
		}
	}
	if len(t.lines) > 2*keepLines {
		t.lines = append([]string(nil), t.lines[len(t.lines)-keepLines:]...)
	}
}

func (t *transcript) tail(n int) []string {
	lines := t.lines
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string{}, lines...)
}

func (t *transcript) close() {
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
}

// clip keeps the first n characters of text.
func clip(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	r := []rune(text)
	return string(r[:n-1]) + "…"
}

// tail keeps the last n characters of text.
func tail(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	r := []rune(text)
	return "…" + string(r[len(r)-n+1:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(errors.New("oversire: no random bytes"))
	}
	return hex.EncodeToString(b)
}
