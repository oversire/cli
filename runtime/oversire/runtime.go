//go:build !windows

// Package oversire runs agent sessions without Orca, on agent-visor. Each item session gets a
// git worktree (a visor workdir with the item key), and the visor holds the harness process.
// A session lives through a restart of the daemon. OverSire keeps the queue, the item, the
// permission expiry and the reports.
package oversire

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	ID                       = "oversire"
	DefaultSlots             = 2
	DefaultPermissionTimeout = 10 * time.Minute
	// DefaultOverseenTimeout is the time to answer a prompt of an overseen session. The overseer answers late.
	DefaultOverseenTimeout = 40 * time.Minute
	// Local is the workspace account id: the sessions run on this machine.
	Local = "local"

	keepLines = 1000
	maxText   = 500
	forget    = 24 * time.Hour
	// replayQuiet ends a replay that gets no event for this time.
	replayQuiet = 2 * time.Second
)

// The labels that OverSire puts on a visor session.
const (
	LabelRuntime = "oversire.runtime"
	LabelSession = "oversire.session"
	labelOrder   = "oversire.order"
	labelRepo    = "oversire.repo"
	labelBranch  = "oversire.branch"
	labelItem    = "oversire.item"
	labelRunner  = "oversire.runner"
	labelKey     = "oversire.key"
	// labelOverseer holds the session id of the overseer. A session that the daemon adopts after a restart reads it back.
	labelOverseer = "oversire.overseer"
)

// escPrefix starts the id of a question that the daemon sets to escalate to the user. It is not a request of the visor.
const escPrefix = "esc-"

// restartPrompt is the first turn of a session that the visor resumes after a lost process.
const restartPrompt = "The machine or the session host restarted during your turn. Go on with the task."

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

// Host is the part of agent-visor that the runtime uses. *agentvisor.Visor and *agentvisor.Client have it.
type Host interface {
	Info(ctx context.Context) (agentvisor.Info, error)
	Start(ctx context.Context, spec agentvisor.StartSpec) (agentvisor.Session, error)
	Session(ctx context.Context, id string) (agentvisor.Session, error)
	Sessions(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Session, error)
	Send(ctx context.Context, id string, in agentvisor.Input) (agentvisor.Delivery, error)
	Interrupt(ctx context.Context, id string) error
	Answer(ctx context.Context, id, requestID string, a agentvisor.Answer) error
	Stop(ctx context.Context, id string) error
	Events(ctx context.Context, f agentvisor.Filter, after uint64) (*agentvisor.EventStream, error)
	Projects(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Project, error)
	Workdirs(ctx context.Context, f agentvisor.Filter) ([]agentvisor.Workdir, error)
	EnsureWorkdir(ctx context.Context, w agentvisor.WorkdirSpec) (agentvisor.Workdir, error)
	RemoveWorkdir(ctx context.Context, id string, force bool) error
	Attach(ctx context.Context, id string, o agentvisor.AttachOptions) (*agentvisor.AttachStream, error)
	Handover(ctx context.Context, id string, to agentvisor.Kind) error
}

type Options struct {
	// Runner is the device id of this runner.
	Runner string
	// DataDir holds the worktrees and the transcripts of the sessions from before agent-visor.
	DataDir string
	// Slots is the maximum of sessions that run at the same time. Zero is DefaultSlots.
	Slots int
	// PermissionTimeout is the time to answer a permission prompt. Zero is DefaultPermissionTimeout.
	PermissionTimeout time.Duration
	// OverseenTimeout is the time to answer a prompt of an overseen session. Zero is DefaultOverseenTimeout.
	OverseenTimeout time.Duration
	// Fake permits the agent "fake", for tests.
	Fake bool
	Log  *slog.Logger
}

type Runtime struct {
	opts   Options
	host   Host
	git    Runner
	slots  *Slots
	report Reporter
	now    func() time.Time
	log    *slog.Logger
	// digests gives the transcript reader of a harness.
	digests func(harness string) (digest.Reader, error)

	mu       sync.Mutex
	sessions map[string]*session // by OverSire id
	byVisor  map[string]*session // by visor session id
	others   map[string]bool     // visor session ids of other backends
	live     bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(opts Options, host Host, git Runner, report Reporter) *Runtime {
	if opts.Slots <= 0 {
		opts.Slots = DefaultSlots
	}
	if opts.PermissionTimeout <= 0 {
		opts.PermissionTimeout = DefaultPermissionTimeout
	}
	if opts.OverseenTimeout <= 0 {
		opts.OverseenTimeout = DefaultOverseenTimeout
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Runtime{
		opts: opts, host: host, git: git, report: report, now: time.Now, digests: digest.ForUser, log: opts.Log.With("source", "oversire"),
		slots:    NewSlots(opts.Slots),
		sessions: map[string]*session{},
		byVisor:  map[string]*session{},
		others:   map[string]bool{},
	}
}

func (rt *Runtime) ID() string { return ID }

var (
	workspaceKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sessionID    = regexp.MustCompile(`^s-[0-9a-f]+$`)
)

// Boot reads the sessions of the visor and follows its events. It returns when the state of each
// session is known, so the reports are right before the daemon serves.
func (rt *Runtime) Boot(ctx context.Context) error {
	info, err := rt.host.Info(ctx)
	if err != nil {
		return fmt.Errorf("read the session host: %w", err)
	}
	all, err := rt.host.Sessions(ctx, agentvisor.Filter{Labels: map[string]string{LabelRuntime: ID}})
	if err != nil {
		return fmt.Errorf("read the sessions of the session host: %w", err)
	}
	rt.mu.Lock()
	for _, vs := range all {
		rt.adoptLocked(vs)
	}
	rt.mu.Unlock()
	follow, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// No filter: the head can be an event of another backend, and a filtered replay never gets to it.
	stream, err := rt.host.Events(follow, agentvisor.Filter{}, 0)
	if err != nil {
		cancel()
		return fmt.Errorf("follow the session host: %w", err)
	}
	caught := make(chan struct{})
	rt.mu.Lock()
	rt.cancel, rt.done = cancel, make(chan struct{})
	rt.mu.Unlock()
	go rt.follow(follow, stream, info.Seq, caught)
	select {
	case <-caught:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// follow applies the events of the visor. The events up to head are the replay: they set the state
// with no reports. At head each session reports once and arms its permission expiry.
func (rt *Runtime) follow(ctx context.Context, stream *agentvisor.EventStream, head uint64, caught chan struct{}) {
	defer close(rt.done)
	defer stream.Close() //nolint:errcheck // Close of a stream gives nil
	live := false
	goLive := func() {
		live = true
		rt.mu.Lock()
		rt.live = true
		rt.prune()
		rt.mu.Unlock()
		for _, s := range rt.all() {
			s.wake()
		}
		close(caught)
	}
	if head == 0 {
		goLive()
	}
	for {
		next, stop := ctx, context.CancelFunc(func() {})
		if !live {
			next, stop = context.WithTimeout(ctx, replayQuiet)
		}
		e, err := stream.Next(next)
		stop()
		if !live && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			// The visor forgot the session of the head event during the replay.
			rt.log.Warn("the replay of the session host stopped before its head", "head", head)
			goLive()
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				rt.log.Warn("the event stream of the session host ended", "err", err)
			}
			if !live {
				goLive()
			}
			return
		}
		if s := rt.forVisor(ctx, e.Session); s != nil {
			// The events have no kind: a handover changes the kind, then the state.
			if e.Type == agentvisor.EventSessionState {
				if vs, err := rt.host.Session(ctx, e.Session); err == nil {
					s.mu.Lock()
					s.setKind(vs.Kind)
					s.mu.Unlock()
				}
			}
			s.apply(e)
		}
		if !live && e.Seq >= head {
			goLive()
		}
	}
}

// forVisor gives the session of a visor session id. A session that the follow sees before Start
// returns gets its labels from the visor.
func (rt *Runtime) forVisor(ctx context.Context, vid string) *session {
	if vid == "" {
		return nil
	}
	rt.mu.Lock()
	s, other := rt.byVisor[vid], rt.others[vid]
	rt.mu.Unlock()
	if s != nil || other {
		return s
	}
	vs, err := rt.host.Session(ctx, vid)
	if err != nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if vs.Labels[LabelRuntime] != ID {
		rt.others[vid] = true
		return nil
	}
	return rt.adoptLocked(vs)
}

// adoptLocked gives the session of a visor session, and makes it from the labels when it is new.
// Call it with rt.mu held.
func (rt *Runtime) adoptLocked(vs agentvisor.Session) *session {
	if s := rt.byVisor[vs.ID]; s != nil {
		return s
	}
	id := vs.Labels[LabelSession]
	s := rt.sessions[id]
	if s == nil {
		s = sessionOf(rt, vs)
		s.live = rt.live
		s.slot = vs.State != agentvisor.StateExited && rt.slots.Take()
		rt.sessions[id] = s
	}
	rt.bindLocked(s, vs)
	return s
}

// bind links a session to its visor session.
func (rt *Runtime) bind(s *session, vs agentvisor.Session) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.bindLocked(s, vs)
}

// bindLocked is bind with rt.mu held.
func (rt *Runtime) bindLocked(s *session, vs agentvisor.Session) {
	rt.byVisor[vs.ID] = s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vid = vs.ID
	s.info.View = viewOf(vs.Kind)
	if s.vstate == "" {
		s.vstate = vs.State
	}
	if s.vendorID == "" {
		s.vendorID = vs.VendorID
	}
}

func (rt *Runtime) all() []*session {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]*session, 0, len(rt.sessions))
	for _, s := range rt.sessions {
		out = append(out, s)
	}
	return out
}

// Start makes the worktree and gives the session at once. With r.InPlace, the session works in the folder r.Repo itself. The agent starts when a slot is free.
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
	if !rt.knows(agentwire.Harness(r.Agent)) {
		return none, fmt.Errorf("The oversire runtime does not know the agent %q.", r.Agent)
	}
	repo := filepath.Clean(r.Repo)
	branch := r.Branch
	if branch == "" {
		branch = strings.ToLower(key)
	}
	var w agentvisor.Workdir
	var err error
	dir := repo
	if r.InPlace {
		w, err = rt.ensureFolder(ctx, repo)
	} else {
		w, err = rt.ensureWorkdir(ctx, repo, key, branch, r.BaseBranch)
		dir = w.Path
	}
	if err != nil {
		return none, err
	}
	id := "s-" + randomHex(8)
	ref := sessionRef(repo, key, id)
	now := rt.now().UTC()
	s := newSession(rt, wire.RunnerSession{ID: id, Runner: rt.opts.Runner, Session: ref, Order: o.ID, Agent: r.Agent, Repo: r.Repo, Branch: w.Branch, Item: r.Item,
		State: wire.SessionQueued, StartedAt: now, UpdatedAt: now}, dir)
	s.req, s.workdir, s.live, s.overseer = r, w.ID, true, r.Overseer
	rt.mu.Lock()
	rt.prune()
	rt.sessions[id] = s
	rt.mu.Unlock()

	s.mu.Lock()
	s.publish()
	s.mu.Unlock()
	go s.run(context.WithoutCancel(ctx), o.ExpiresAt)
	return ref, nil
}

// Launch is Start with the DaemonSession id, which is the session id.
func (rt *Runtime) Launch(ctx context.Context, o runtime.Order, r wire.LaunchRequest) (runtime.Started, error) {
	ref, err := rt.Start(ctx, o, r)
	if err != nil {
		return runtime.Started{}, err
	}
	return runtime.Started{Session: ref, PaneKey: ref.ID}, nil
}

func (rt *Runtime) knows(h agentwire.Harness) bool {
	if h == agentwire.Fake {
		return rt.opts.Fake
	}
	return h != agentwire.Native && slices.Contains(agentwire.Harnesses(), h)
}

func (rt *Runtime) Stop(ctx context.Context, ref wire.SessionRef) error {
	s, err := rt.session(ref)
	if err != nil {
		return err
	}
	return s.stop(ctx)
}

// Read gives the last lines of the transcript. A session from before agent-visor reads its transcript file.
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
	return s.say(ctx, p.Text, p.Origin)
}

// History reads the transcript of the harness session. A session from before agent-visor reads its session file.
func (rt *Runtime) History(_ context.Context, ref wire.SessionRef, b wire.HistoryBudget) (wire.SessionDigest, error) {
	var m sessionMeta
	s, err := rt.session(ref)
	if err == nil {
		s.mu.Lock()
		m = sessionMeta{Agent: s.info.Agent, HarnessSession: s.vendorID}
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

// Known tells whether the runtime has the session id. The daemon ends the sessions in its book that the runtime does not know.
func (rt *Runtime) Known(id string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.sessions[id]
	return ok
}

// Close stops the follow and the queued sessions. The visor keeps the sessions that run, and
// the next Boot finds them again.
func (rt *Runtime) Close(ctx context.Context) {
	for _, s := range rt.all() {
		s.dropQueued()
	}
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// prune forgets the sessions that ended a day ago. Call it with rt.mu held.
func (rt *Runtime) prune() {
	old := rt.now().Add(-forget)
	for id, s := range rt.sessions {
		s.mu.Lock()
		gone := s.info.State.Ended() && s.info.UpdatedAt.Before(old)
		vid := s.vid
		s.mu.Unlock()
		if gone {
			delete(rt.sessions, id)
			delete(rt.byVisor, vid)
		}
	}
}

func (rt *Runtime) session(ref wire.SessionRef) (*session, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	s, ok := rt.sessions[ref.ID]
	if !ok {
		return nil, fmt.Errorf("The oversire runtime has no session %s: %w.", ref.ID, runtime.ErrNotFound)
	}
	return s, nil
}

// Inspect gives the asks of the session, also the hidden ones of an overseen session, with its tokens and times.
func (rt *Runtime) Inspect(_ context.Context, ref wire.SessionRef) (runtime.Inspection, error) {
	s, err := rt.session(ref)
	if err != nil {
		return runtime.Inspection{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in := runtime.Inspection{Session: s.info, Tokens: s.tokens, LastTurn: s.turns.lastAt()}
	oldest := func(at time.Time) {
		if !at.IsZero() && (in.AskedAt.IsZero() || at.Before(in.AskedAt)) {
			in.AskedAt = at
		}
	}
	for _, p := range s.permissions {
		oldest(p.at)
	}
	// A question of the daemon (esc-) is not an ask of the worker.
	if q := s.question; q != nil && !strings.HasPrefix(q.question.ID, escPrefix) {
		oldest(q.question.AskedAt)
	}
	return in, nil
}

// Escalate shows the open ask of an overseen session to the user (q nil), or sets q as its open question.
// The id of q starts with escPrefix. Only the daemon keeps q: a restart drops it.
func (rt *Runtime) Escalate(_ context.Context, ref wire.SessionRef, q *wire.RunnerQuestion) error {
	s, err := rt.session(ref)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info.State.Ended() {
		return fmt.Errorf("The session %s has ended: %w.", s.id, runtime.ErrConflict)
	}
	if q != nil {
		switch {
		case !strings.HasPrefix(q.ID, escPrefix):
			return fmt.Errorf("The id %q of an escalation must start with %s.", q.ID, escPrefix)
		case s.question != nil:
			return fmt.Errorf("The session %s has an open question: %w.", s.id, runtime.ErrConflict)
		}
		s.question = &openQuestion{question: *q}
		s.info.Question = &s.question.question
		s.log.add("? " + firstText(q))
	}
	s.escalated = true
	s.show()
	s.publish()
	return nil
}

func firstText(q *wire.RunnerQuestion) string {
	if len(q.Items) == 0 {
		return ""
	}
	return q.Items[0].Text
}

// The files of a session from before agent-visor.

func (rt *Runtime) logPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".log")
}

func (rt *Runtime) metaPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".json")
}

func (rt *Runtime) turnsPath(id string) string {
	return filepath.Join(rt.opts.DataDir, "sessions", id+".turns.jsonl")
}

// sessionMeta lets History find the harness session of a session from before agent-visor.
type sessionMeta struct {
	Agent          string `json:"agent"`
	HarnessSession string `json:"harnessSession"`
}

// transcript keeps the last lines.
type transcript struct {
	lines []string
}

func (t *transcript) add(text string) {
	t.lines = append(t.lines, strings.Split(strings.TrimRight(text, "\n"), "\n")...)
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
