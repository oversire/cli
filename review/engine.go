// Package review is the review gate of the daemon (OVERSIRE-365). It finds the worktree groups with work to review
// and the plans of Plan sessions, computes the diffs, keeps the comments and the rounds, sends a submitted round
// to the session of the key, and merges a group locally. It never pushes.
package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

const (
	tick = 30 * time.Second
	// sendWait is how long a session that got a prompt counts as working when the book shows no new state.
	sendWait  = 2 * time.Minute
	launchTTL = 30 * time.Minute
)

// Repos gives the repos of the runtimes of this machine (runtime.Registry).
type Repos interface {
	Repos(ctx context.Context) wire.DaemonReposPage
}

// Sessions is the session book (sessions.Book).
type Sessions interface {
	All() []wire.DaemonSession
	Get(id string) (wire.DaemonSession, bool)
	Watch() (<-chan string, func())
}

// Sender types a prompt into a live session (ask.Asker).
type Sender interface {
	CanSend(s wire.DaemonSession) bool
	Deliver(ctx context.Context, s wire.DaemonSession, text string) error
}

// Launcher starts a session on a runtime of this machine (orders.Runner).
type Launcher interface {
	Launch(ctx context.Context, rt string, o runtime.Order, r wire.LaunchRequest) (runtime.Started, error)
}

// Picker gives the runtime of a new session (runtime.Registry).
type Picker interface {
	Pick(ctx context.Context, id string) (string, error)
}

// Deps are the parts of an Engine. Notify and Clock can be nil.
type Deps struct {
	Store    *Store
	Git      Git
	Repos    Repos
	Sessions Sessions
	Sender   Sender
	Launcher Launcher
	Picker   Picker
	Machine  string
	// Order is the merge order: repo names. Names that it does not list come after, in alphabetic order.
	Order  Order
	Notify func(wire.ReviewEvent)
	Clock  func() time.Time
	// Events takes the rows review.ready, review.submitted, review.approved, git.merge and item.shipped. It can be nil.
	Events EventAdder
}

// EventAdder takes the event rows (capture.Writer).
type EventAdder interface {
	Add(session string, d wire.EventDraft)
}

// Kind is the kind of an Error. The routes give it as the HTTP status.
type Kind int

const (
	NotFound Kind = iota + 1
	Conflict
	Invalid
)

// Error is a refused call.
type Error struct {
	Kind Kind
	Text string
}

func (e *Error) Error() string { return e.Text }

func refuse(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Text: fmt.Sprintf(format, a...)}
}

// Engine keeps the reviews of this machine. mu guards the records and the roots.
// send puts the deliveries and the merges in a line, so that two prompts never go to one session at the same time.
type Engine struct {
	Log *slog.Logger
	d   Deps

	send sync.Mutex

	mu    sync.Mutex
	recs  map[string]*record // by kind + ":" + key
	roots []string           // the roots that a hand-off named
	sent  map[string]time.Time
}

// NewEngine loads the records and the roots of the store.
func NewEngine(d Deps) *Engine {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Notify == nil {
		d.Notify = func(wire.ReviewEvent) {}
	}
	e := &Engine{Log: slog.Default(), d: d, recs: map[string]*record{}, sent: map[string]time.Time{}}
	recs, err := d.Store.Load()
	if err != nil {
		e.Log.Warn("review load", "err", err)
	}
	for _, r := range recs {
		e.recs[id(r.Kind, r.Key)] = r
	}
	if e.roots, err = d.Store.Roots(); err != nil {
		e.Log.Warn("review roots", "err", err)
	}
	return e
}

func id(kind wire.ReviewKind, key string) string { return string(kind) + ":" + key }

// List gives the reviews of kind, or of both kinds for "", and the bases that are not pushed.
func (e *Engine) List(ctx context.Context, kind wire.ReviewKind) (wire.ReviewsPage, error) {
	page := wire.ReviewsPage{Reviews: []wire.ReviewSummary{}, NotPushed: []wire.NotPushed{}}
	if kind != "" && kind != wire.ReviewCode && kind != wire.ReviewPlan {
		return page, refuse(Invalid, "The kind %q is not code or plan.", kind)
	}
	roots := e.allRoots(ctx)
	if kind != wire.ReviewPlan {
		for _, g := range scanGroups(roots) {
			s, _, err := e.codeSummary(ctx, g)
			if err != nil {
				e.Log.Debug("review group", "group", g.Dir(), "err", err)
				continue
			}
			if listed(s) {
				page.Reviews = append(page.Reviews, s)
			}
		}
	}
	if kind != wire.ReviewCode {
		e.mu.Lock()
		var plans []*record
		for _, r := range e.recs {
			if r.Kind == wire.ReviewPlan && r.Plan != nil && !r.Approved {
				plans = append(plans, r)
			}
		}
		e.mu.Unlock()
		for _, r := range plans {
			page.Reviews = append(page.Reviews, e.planSummary(r.Key))
		}
	}
	sort.Slice(page.Reviews, func(i, j int) bool {
		a, b := page.Reviews[i], page.Reviews[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.Key+string(a.Kind) < b.Key+string(b.Kind)
	})
	page.NotPushed = e.notPushed(ctx, roots)
	return page, nil
}

// listed is true for a code review with a repo that has commits or changes, which is not merged.
func listed(s wire.ReviewSummary) bool {
	return s.State != wire.ReviewMerged && slices.ContainsFunc(s.Repos, func(r wire.ReviewRepo) bool { return r.Ahead > 0 || r.Dirty })
}

// Get gives the review of key and kind. An empty kind is code, or plan for a key that only has a plan.
func (e *Engine) Get(ctx context.Context, key string, kind wire.ReviewKind) (wire.ReviewDetail, error) {
	kind, err := e.kindOf(ctx, key, kind)
	if err != nil {
		return wire.ReviewDetail{}, err
	}
	d := wire.ReviewDetail{Files: []wire.ReviewFile{}, Comments: []wire.ReviewComment{}, Rounds: []wire.ReviewRound{}}
	if kind == wire.ReviewPlan {
		e.mu.Lock()
		r := e.recs[id(kind, key)]
		var plan *wire.PlanDoc
		if r != nil && r.Plan != nil {
			p := clone(*r.Plan)
			plan = &p
		}
		e.mu.Unlock()
		if plan == nil {
			return d, refuse(NotFound, "%s has no plan.", key)
		}
		d.Summary, d.Plan = e.planSummary(key), plan
	} else {
		g, ok := findGroup(e.allRoots(ctx), key)
		var repos []wire.ReviewRepo
		if ok {
			if d.Summary, repos, err = e.codeSummary(ctx, g); err != nil {
				return d, err
			}
		} else if d.Summary, ok = e.goneSummary(key); !ok {
			return d, refuse(NotFound, "There is no group .wt/%s in the roots of this daemon.", strings.ToLower(key))
		}
		viewed := e.viewed(kind, key)
		for _, r := range repos {
			files, err := e.d.Git.files(ctx, g, r)
			if err != nil {
				return d, err
			}
			for _, f := range files {
				f.Viewed = viewed[f.Repo+"/"+f.Path]
				d.Files = append(d.Files, f)
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.recs[id(kind, key)]; r != nil {
		d.Comments = comments(r)
		d.Rounds = clone(r.Rounds)
	}
	if d.Rounds == nil {
		d.Rounds = []wire.ReviewRound{}
	}
	return d, nil
}

// Diff gives the diff of one file of the code review of key.
func (e *Engine) Diff(ctx context.Context, key, repo, path string) (wire.FileDiff, error) {
	g, r, err := e.repoOf(ctx, key, repo)
	if err != nil {
		return wire.FileDiff{}, err
	}
	files, err := e.d.Git.files(ctx, g, r)
	if err != nil {
		return wire.FileDiff{}, err
	}
	for _, f := range files {
		if f.Path == path {
			return e.d.Git.diff(ctx, g, r, f)
		}
	}
	return wire.FileDiff{}, refuse(NotFound, "The review %s has no changed file %s/%s.", key, repo, path)
}

// repoOf gives the group of key and the state of its repo.
func (e *Engine) repoOf(ctx context.Context, key, repo string) (group, wire.ReviewRepo, error) {
	g, ok := findGroup(e.allRoots(ctx), key)
	if !ok {
		return g, wire.ReviewRepo{}, refuse(NotFound, "There is no group .wt/%s in the roots of this daemon.", strings.ToLower(key))
	}
	if !slices.Contains(g.Repos, repo) {
		return g, wire.ReviewRepo{}, refuse(NotFound, "The group %s has no repo %s.", g.Rel(), repo)
	}
	r, _, err := e.d.Git.repo(ctx, g, repo)
	return g, r, err
}

// codeSummary gives the summary of the group g and its repos in merge order.
func (e *Engine) codeSummary(ctx context.Context, g group) (wire.ReviewSummary, []wire.ReviewRepo, error) {
	key := g.Key()
	s := wire.ReviewSummary{Key: key, Kind: wire.ReviewCode, Machine: e.d.Machine, Root: g.Root, Group: g.Rel(), Repos: []wire.ReviewRepo{}, Subitems: []string{}}
	var newest time.Time
	for _, name := range e.d.Order.Sort(g.Repos) {
		r, at, err := e.d.Git.repo(ctx, g, name)
		if err != nil {
			return s, nil, err
		}
		s.Repos = append(s.Repos, r)
		if at.After(newest) {
			newest = at
		}
	}
	for _, r := range s.Repos {
		if len(r.Commits) > 0 && s.Title == "" {
			s.Title = r.Commits[len(r.Commits)-1].Subject
		}
		for _, c := range r.Commits {
			for _, k := range c.Items {
				if k != key && !slices.Contains(s.Subitems, k) {
					s.Subitems = append(s.Subitems, k)
				}
			}
		}
	}
	slices.Sort(s.Subitems)
	ses, rs := e.sessionOf(wire.ReviewCode, key, g.Dir())
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.recs[id(wire.ReviewCode, key)]
	if r == nil && ses.Harness != "" {
		r = e.rec(wire.ReviewCode, key) // keep the agent for a new session after this one ends
		r.Root = g.Root
	}
	e.fill(&s, r, ses, rs, newest)
	switch {
	case rs.State == wire.ReviewSessionWorking:
		s.State = wire.ReviewWaiting
	case r != nil && r.Conflict,
		r != nil && r.ReadyAt != nil && slices.ContainsFunc(s.Repos, func(x wire.ReviewRepo) bool { return x.Merge == wire.MergeConflict }):
		s.State = wire.ReviewConflict
	case r != nil && r.MergedAt != nil && !slices.ContainsFunc(s.Repos, func(x wire.ReviewRepo) bool { return x.Ahead > 0 }):
		s.State = wire.ReviewMerged
	}
	return s, s.Repos, nil
}

// goneSummary is the summary of a merged code review whose group is gone.
func (e *Engine) goneSummary(key string) (wire.ReviewSummary, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.recs[id(wire.ReviewCode, key)]
	if r == nil || r.MergedAt == nil || r.Root == "" {
		return wire.ReviewSummary{}, false
	}
	s := wire.ReviewSummary{Key: key, Kind: wire.ReviewCode, Machine: e.d.Machine, Root: r.Root, Group: ".wt/" + strings.ToLower(key),
		Repos: []wire.ReviewRepo{}, Subitems: []string{}}
	e.fill(&s, r, wire.DaemonSession{}, wire.ReviewSession{State: wire.ReviewSessionNone}, time.Time{})
	s.State = wire.ReviewMerged
	return s, true
}

// planSummary is the summary of the plan of key.
func (e *Engine) planSummary(key string) wire.ReviewSummary {
	e.mu.Lock()
	root := ""
	if r := e.recs[id(wire.ReviewPlan, key)]; r != nil {
		root = r.Root
	}
	e.mu.Unlock()
	ses, rs := e.sessionOf(wire.ReviewPlan, key, root)
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.recs[id(wire.ReviewPlan, key)]
	s := wire.ReviewSummary{Key: key, Kind: wire.ReviewPlan, Machine: e.d.Machine, Root: root, Repos: []wire.ReviewRepo{}, Subitems: []string{}}
	if r != nil && r.Plan != nil {
		s.Title = r.Plan.Title
		for _, it := range r.Plan.Subitems {
			if it.Key != "" {
				s.Subitems = append(s.Subitems, it.Key)
			}
		}
	}
	e.fill(&s, r, ses, rs, time.Time{})
	if rs.State == wire.ReviewSessionWorking {
		s.State = wire.ReviewWaiting
	}
	return s
}

// fill sets the parts of s that come from the record and the session. The state is ready. Call it with e.mu held.
func (e *Engine) fill(s *wire.ReviewSummary, r *record, ses wire.DaemonSession, rs wire.ReviewSession, newest time.Time) {
	s.State = wire.ReviewReadyState
	s.Session = &rs
	if r == nil {
		s.UpdatedAt = newest
		if s.UpdatedAt.IsZero() {
			s.UpdatedAt = e.now()
		}
		return
	}
	if rs.ID != "" && e.remember(r, ses) {
		e.save(r)
	}
	s.Round, s.Note, s.ReadyAt = len(r.Rounds), r.Note, r.ReadyAt
	for _, c := range r.Comments {
		switch c.State {
		case wire.CommentPending:
			s.Pending++
		case wire.CommentSubmitted:
			s.Unanswered++
		}
	}
	s.UpdatedAt = r.UpdatedAt
	if newest.After(s.UpdatedAt) {
		s.UpdatedAt = newest
	}
}

// remember keeps the session and its agent in r, for a new session after it ends. It tells if r changed.
func (e *Engine) remember(r *record, s wire.DaemonSession) bool {
	a := &agentMemo{Runtime: s.Runtime, Launch: wire.AgentLaunch{CLI: s.Harness, Model: s.Model, Effort: s.Effort, Profile: s.Profile}}
	if s.Harness == "" || r.Session == s.ID && r.Agent != nil && reflect.DeepEqual(*r.Agent, *a) {
		return false
	}
	if r.Agent != nil && r.Agent.Launch.CLI == a.Launch.CLI && r.Agent.Runtime == a.Runtime {
		a.Launch.Command = r.Agent.Launch.Command
	}
	r.Session, r.Agent = s.ID, a
	return true
}

// sessionOf gives the newest session of key in the book: for code, a session in the group folder dir or with the
// start key work:<KEY>; for a plan, a session with the start key plan:<KEY> or refine:<KEY>. The last known session
// counts too. ok in the ReviewSession is false when there is none.
func (e *Engine) sessionOf(kind wire.ReviewKind, key, dir string) (wire.DaemonSession, wire.ReviewSession) {
	keys := []string{"work:" + key}
	if kind == wire.ReviewPlan {
		keys = []string{"plan:" + key, "refine:" + key}
		dir = ""
	}
	e.mu.Lock()
	last, sentAt := "", e.sent[id(kind, key)]
	if r := e.recs[id(kind, key)]; r != nil {
		last = r.Session
	}
	e.mu.Unlock()
	for _, s := range e.d.Sessions.All() {
		if slices.Contains(keys, s.Key) || s.ID == last || dir != "" && (inside(s.Cwd, dir) || inside(s.Repo, dir)) {
			return s, e.reviewSession(s, sentAt)
		}
	}
	if last != "" {
		return wire.DaemonSession{}, wire.ReviewSession{ID: last, State: wire.ReviewSessionEnded}
	}
	return wire.DaemonSession{}, wire.ReviewSession{State: wire.ReviewSessionNone}
}

// reviewSession maps the phase of s. A session that got a prompt at sentAt counts as working until the book shows a newer state.
func (e *Engine) reviewSession(s wire.DaemonSession, sentAt time.Time) wire.ReviewSession {
	rs := wire.ReviewSession{ID: s.ID, Harness: s.Harness, Profile: s.Profile, CanSend: e.d.Sender != nil && e.d.Sender.CanSend(s)}
	switch s.State {
	case wire.PhaseWorking, wire.PhaseStarting:
		rs.State = wire.ReviewSessionWorking
	case wire.PhaseWaiting:
		rs.State = wire.ReviewSessionIdle
		if s.Reason == wire.ReasonQuestion || s.Reason == wire.ReasonPermission {
			rs.State = wire.ReviewSessionWorking // the agent waits for an answer, not for a prompt
		}
	case wire.PhaseDone:
		// A session of the oversire runtime is done when it ends. A Claude session is done at the end of each turn.
		rs.State = wire.ReviewSessionIdle
		if s.Runtime == wire.RuntimeOversire {
			rs.State = wire.ReviewSessionEnded
		}
	default:
		rs.State = wire.ReviewSessionEnded
	}
	if rs.State == wire.ReviewSessionIdle && !sentAt.IsZero() && !s.StateSince.After(sentAt) && e.d.Clock().Sub(sentAt) < sendWait {
		rs.State = wire.ReviewSessionWorking
	}
	if rs.State == wire.ReviewSessionEnded {
		rs.CanSend = false
	}
	return rs
}

// inside tells if path is dir or a path in dir.
func inside(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// kindOf checks the key and gives the kind: kind when it is set, else code, or plan for a key that only has a plan.
func (e *Engine) kindOf(ctx context.Context, key string, kind wire.ReviewKind) (wire.ReviewKind, error) {
	if !validKey(key) {
		return "", refuse(Invalid, "The key %q is not valid. Use the item key in upper case, for example SLING-170.", key)
	}
	switch kind {
	case wire.ReviewCode, wire.ReviewPlan:
		return kind, nil
	case "":
	default:
		return "", refuse(Invalid, "The kind %q is not code or plan.", kind)
	}
	e.mu.Lock()
	code, plan := e.recs[id(wire.ReviewCode, key)], e.recs[id(wire.ReviewPlan, key)]
	e.mu.Unlock()
	if plan != nil && code == nil {
		if _, ok := findGroup(e.allRoots(ctx), key); !ok {
			return wire.ReviewPlan, nil
		}
	}
	return wire.ReviewCode, nil
}

// allRoots is the folder repos of the runtimes and the roots of the hand-offs, sorted, with no copy.
func (e *Engine) allRoots(ctx context.Context) []string {
	e.mu.Lock()
	roots := slices.Clone(e.roots)
	e.mu.Unlock()
	if e.d.Repos != nil {
		for _, r := range e.d.Repos.Repos(ctx).Repos {
			if r.Folder && r.Path != "" {
				roots = append(roots, filepath.Clean(r.Path))
			}
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// addRoot keeps root as a known root. Call it with e.mu held.
func (e *Engine) addRoot(root string) {
	root = filepath.Clean(root)
	if slices.Contains(e.roots, root) {
		return
	}
	e.roots = append(e.roots, root)
	slices.Sort(e.roots)
	if err := e.d.Store.SaveRoots(e.roots); err != nil {
		e.Log.Error("review roots save", "err", err)
	}
}

// notPushed gives each base of the primary checkouts of the roots that is ahead of its upstream.
func (e *Engine) notPushed(ctx context.Context, roots []string) []wire.NotPushed {
	out := []wire.NotPushed{}
	for _, root := range roots {
		for _, name := range primaries(root) {
			if np, ok := e.d.Git.notPushed(ctx, root, name); ok {
				out = append(out, np)
			}
		}
	}
	return out
}

func (e *Engine) viewed(kind wire.ReviewKind, key string) map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.recs[id(kind, key)]; r != nil {
		return maps.Clone(r.Viewed)
	}
	return nil
}

// rec gives the record of kind and key, and makes it when it is missing. Call it with e.mu held.
func (e *Engine) rec(kind wire.ReviewKind, key string) *record {
	r := e.recs[id(kind, key)]
	if r == nil {
		r = &record{Key: key, Kind: kind, Comments: []wire.ReviewComment{}, Rounds: []wire.ReviewRound{}, UpdatedAt: e.now()}
		e.recs[id(kind, key)] = r
	}
	return r
}

// changed saves r and tells the event. Call it with e.mu held.
func (e *Engine) changed(r *record, event string) {
	r.UpdatedAt = e.now()
	e.save(r)
	e.d.Notify(wire.ReviewEvent{Type: "review", Event: event, Key: r.Key, Kind: r.Kind, Machine: e.d.Machine})
}

func (e *Engine) save(r *record) {
	if err := e.d.Store.Save(r); err != nil {
		e.Log.Error("review save", "key", r.Key, "kind", r.Kind, "err", err)
	}
}

// event writes one row about the item key.
func (e *Engine) event(key, id, kind string, actor wire.ActorType, summary string, payload map[string]any) {
	if e.d.Events == nil {
		return
	}
	d := wire.EventDraft{ID: id, At: e.d.Clock().UTC(), Kind: kind, Item: key, ActorType: actor, Via: "oversire", Summary: summary}
	if actor == wire.ActorAgent {
		d.Via = "cli"
	}
	d.Payload, _ = json.Marshal(payload)
	e.d.Events.Add("", d)
}

func (e *Engine) now() time.Time { return e.d.Clock().UTC().Truncate(time.Second) }

// comments gives the comments of r, oldest first. A pending comment has the next round.
func comments(r *record) []wire.ReviewComment {
	out := clone(r.Comments)
	if out == nil {
		out = []wire.ReviewComment{}
	}
	for i := range out {
		if out[i].State == wire.CommentPending {
			out[i].Round = len(r.Rounds) + 1
		}
	}
	return out
}

// clone is a deep copy, so that a caller can read it outside the lock.
func clone[T any](v T) T {
	var out T
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// readLines gives the lines from..to of a file, for an excerpt outside the diff.
func readLines(path string, from, to int) string {
	data, err := os.ReadFile(path)
	if err != nil || isBinary(data) {
		return ""
	}
	all := strings.Split(string(data), "\n")
	if from < 1 || from > len(all) {
		return ""
	}
	return strings.Join(all[from-1:min(to, len(all))], "\n")
}
