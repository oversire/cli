package ticket

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

// ErrNoReview: the daemon has no code review of the key.
var ErrNoReview = errors.New("no review")

// SweepDaemon is what Sweep reads from the local daemon.
type SweepDaemon interface {
	SessionLister
	Reviews(ctx context.Context) (wire.ReviewsPage, error)
	Review(ctx context.Context, key string) (wire.ReviewDetail, error)
}

// Git runs git. review.Git satisfies it.
type Git interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
	GroupRepos(ctx context.Context, root, key string) ([]wire.ReviewRepo, bool, error)
}

// Signal is what the facts tell about a started ticket. The agent adds the Plane state.
type Signal string

const (
	SignalMerged   Signal = "merged"
	SignalConflict Signal = "conflict"
	SignalApproved Signal = "approved"
	SignalReady    Signal = "ready"
	SignalWorking  Signal = "working"
	SignalStalled  Signal = "stalled"
	SignalOpen     Signal = "open"
	SignalNone     Signal = "none"
)

type SweepSession struct {
	ID        string            `json:"id"`
	Machine   string            `json:"machine,omitempty"`
	Runtime   string            `json:"runtime,omitempty"`
	Harness   string            `json:"harness,omitempty"`
	Role      string            `json:"role,omitempty"` // the ledger role, or the start key prefix
	State     wire.SessionPhase `json:"state,omitempty"`
	Live      bool              `json:"live"`
	Stoppable bool              `json:"stoppable"` // runtime orca or oversire on this daemon
	Since     time.Time         `json:"since"`
	Via       string            `json:"via"` // key, worktree or ledger
}

type BaseCommit struct {
	Repo    string    `json:"repo"`
	Base    string    `json:"base"`
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type SweepRow struct {
	Key       string            `json:"key"`
	Signal    Signal            `json:"signal"`
	Sessions  []SweepSession    `json:"sessions"`
	Review    wire.ReviewState  `json:"review,omitempty"`
	Approved  bool              `json:"approved"`
	Group     string            `json:"group,omitempty"`
	Repos     []wire.ReviewRepo `json:"repos,omitempty"`
	OnBase    []BaseCommit      `json:"onBase,omitempty"`
	NotPushed []wire.NotPushed  `json:"notPushed,omitempty"`
	LastWork  *time.Time        `json:"lastWork,omitempty"`
}

type SweepReport struct {
	Root     string     `json:"root"`
	Daemon   bool       `json:"daemon"`
	Rows     []SweepRow `json:"rows"`
	Warnings []string   `json:"warnings,omitempty"`
}

// Sweeper collects the facts of tickets from the daemon, the ledger and git.
type Sweeper struct {
	Daemon SweepDaemon // nil: no daemon facts
	Git    Git
	Ledger *Ledger
	Root   string
	Stale  time.Duration
	Now    func() time.Time
}

const (
	maxOnBase    = 20 // the most base commits of one key
	DefaultStale = 7 * 24 * time.Hour
)

// sweepFacts are the facts that all keys share.
type sweepFacts struct {
	daemon    bool
	sessions  []wire.DaemonSession
	notPushed []wire.NotPushed
	reviews   []string
	bases     []baseCheckout
}

type baseCheckout struct{ repo, dir, branch string }

// Sweep gives one row for each key. With no keys it finds the keys of the local work, of the project when set.
func (s Sweeper) Sweep(ctx context.Context, keys []string, project string) (SweepReport, error) {
	rep := SweepReport{Root: s.Root}
	norm := make([]string, 0, len(keys))
	for _, k := range keys {
		n, err := NormalKey(k)
		if err != nil {
			return rep, err
		}
		if !slices.Contains(norm, n) {
			norm = append(norm, n)
		}
	}
	f := s.facts(ctx, &rep)
	if len(norm) == 0 {
		norm = s.discover(f, project)
	}
	for _, k := range norm {
		rep.Rows = append(rep.Rows, s.row(ctx, f, k, &rep))
	}
	return rep, nil
}

func (s Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s Sweeper) facts(ctx context.Context, rep *SweepReport) sweepFacts {
	var f sweepFacts
	if s.Daemon != nil {
		list, err := s.Daemon.Sessions(ctx)
		switch {
		case errors.Is(err, ErrNoDaemon):
			rep.Warnings = append(rep.Warnings, "No daemon answers: the sessions and the reviews are not in the report.")
		case err != nil:
			rep.Warnings = append(rep.Warnings, "sessions: "+err.Error())
		default:
			f.daemon, f.sessions = true, list
		}
		if f.daemon {
			if page, err := s.Daemon.Reviews(ctx); err != nil {
				rep.Warnings = append(rep.Warnings, "reviews: "+err.Error())
			} else {
				f.notPushed = page.NotPushed
				for _, r := range page.Reviews {
					f.reviews = append(f.reviews, r.Key)
				}
			}
		}
	}
	rep.Daemon = f.daemon
	for _, dir := range s.checkouts() {
		b, err := s.Git.Run(ctx, dir, "branch", "--show-current")
		if err != nil || b == "" {
			continue
		}
		f.bases = append(f.bases, baseCheckout{repo: filepath.Base(dir), dir: dir, branch: b})
	}
	return f
}

// checkouts gives the primary checkouts of the root: each child folder whose .git is a folder, or the root itself.
func (s Sweeper) checkouts() []string {
	if isGitDir(s.Root) {
		return []string{s.Root}
	}
	entries, _ := os.ReadDir(s.Root)
	var out []string
	for _, e := range entries {
		if d := filepath.Join(s.Root, e.Name()); e.IsDir() && isGitDir(d) {
			out = append(out, d)
		}
	}
	return out
}

func isGitDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && st.IsDir()
}

func (s Sweeper) discover(f sweepFacts, project string) []string {
	var found []string
	add := func(k string) {
		n, err := NormalKey(k)
		if err != nil || slices.Contains(found, n) || project != "" && !strings.HasPrefix(n, strings.ToUpper(project)+"-") {
			return
		}
		found = append(found, n)
	}
	if entries, err := os.ReadDir(filepath.Join(s.Root, ".wt")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(e.Name())
			}
		}
	}
	if s.Ledger != nil {
		ks, _ := s.Ledger.Keys()
		for _, k := range ks {
			add(k)
		}
	}
	for _, ds := range f.sessions {
		if k, ok := ItemOf(ds); ok {
			add(k)
		}
	}
	for _, k := range f.reviews {
		add(k)
	}
	sort.Strings(found)
	return found
}

func (s Sweeper) row(ctx context.Context, f sweepFacts, key string, rep *SweepReport) SweepRow {
	r := SweepRow{Key: key}
	seen := map[string]bool{}
	for _, ds := range f.sessions {
		if k, ok := ItemOf(ds); !ok || k != key {
			continue
		}
		via, role := "worktree", ""
		if ds.Key != "" {
			role, _, _ = strings.Cut(ds.Key, ":")
			via = "key"
		}
		seen[ds.ID] = true
		if ds.HarnessSession != "" {
			seen[ds.HarnessSession] = true
		}
		r.Sessions = append(r.Sessions, sweepSession(ds, role, via))
	}
	if s.Ledger != nil {
		recs, err := s.Ledger.Sessions(key)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: ledger: %v", key, err))
		}
		for _, rec := range recs {
			if seen[rec.Session] {
				continue
			}
			seen[rec.Session] = true
			ss := SweepSession{ID: rec.Session, Machine: rec.Machine, Harness: rec.Harness, Role: rec.Role, Since: rec.At, Via: "ledger"}
			for _, ds := range f.sessions {
				if ds.HarnessSession == rec.Session {
					d := sweepSession(ds, rec.Role, "ledger")
					d.Since = maxTime(d.Since, rec.At)
					ss = d
					break
				}
			}
			r.Sessions = append(r.Sessions, ss)
		}
	}

	var reviewAt time.Time
	if f.daemon {
		d, err := s.Daemon.Review(ctx, key)
		switch {
		case errors.Is(err, ErrNoReview):
		case err != nil:
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: review: %v", key, err))
		default:
			r.Review, r.Repos, reviewAt = d.Summary.State, d.Summary.Repos, d.Summary.UpdatedAt
			if n := len(d.Rounds); n > 0 && d.Rounds[n-1].Verdict == wire.VerdictApprove {
				r.Approved = true
			}
		}
		for _, np := range f.notPushed {
			if slices.Contains(np.Items, key) {
				r.NotPushed = append(r.NotPushed, np)
			}
		}
	}
	if dir := filepath.Join(s.Root, ".wt", strings.ToLower(key)); isDirPath(dir) {
		r.Group = dir
		if r.Repos == nil {
			repos, _, err := s.Git.GroupRepos(ctx, s.Root, key)
			if err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: group: %v", key, err))
			}
			r.Repos = repos
		}
	}
	r.OnBase = s.onBase(ctx, f, key, rep)

	last := reviewAt
	for _, ss := range r.Sessions {
		last = maxTime(last, ss.Since)
	}
	for _, c := range r.OnBase {
		last = maxTime(last, c.At)
	}
	if !last.IsZero() {
		r.LastWork = &last
	}
	stale := s.Stale
	if stale <= 0 {
		stale = DefaultStale
	}
	r.Signal = signal(r, stale, s.now())
	return r
}

func sweepSession(ds wire.DaemonSession, role, via string) SweepSession {
	return SweepSession{ID: ds.ID, Machine: ds.Machine, Runtime: ds.Runtime, Harness: ds.Harness, Role: role, State: ds.State,
		Live: Live(ds), Stoppable: ds.Runtime == "orca" || ds.Runtime == "oversire", Since: maxTime(ds.StateSince, ds.UpdatedAt), Via: via}
}

// onBase gives the commits of the base branches that name key, the newest first.
func (s Sweeper) onBase(ctx context.Context, f sweepFacts, key string, rep *SweepReport) []BaseCommit {
	grep := `(^|[^A-Z0-9_-])` + regexp.QuoteMeta(key) + `([^0-9]|$)`
	var out []BaseCommit
	for _, b := range f.bases {
		text, err := s.Git.Run(ctx, b.dir, "log", b.branch, "-n", fmt.Sprint(maxOnBase), "-E", "--grep", grep, "--format=%h%x1f%cI%x1f%s")
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: git log in %s: %v", key, b.repo, err))
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			p := strings.Split(line, "\x1f")
			if len(p) != 3 {
				continue
			}
			at, _ := time.Parse(time.RFC3339, p[1])
			out = append(out, BaseCommit{Repo: b.repo, Base: b.branch, SHA: p[0], At: at, Subject: p[2]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > maxOnBase {
		out = out[:maxOnBase]
	}
	return out
}

// signal sorts a row. The first rule that matches wins.
func signal(r SweepRow, stale time.Duration, now time.Time) Signal {
	live, working := false, false
	for _, s := range r.Sessions {
		if s.Live {
			live = true
			if s.State == wire.PhaseWorking || s.State == wire.PhaseWaiting || s.State == wire.PhaseStarting {
				working = true
			}
		}
	}
	switch {
	case r.Review == wire.ReviewMerged, r.Group == "" && !live && len(r.OnBase) > 0:
		return SignalMerged
	case r.Review == wire.ReviewConflict:
		return SignalConflict
	case r.Approved:
		return SignalApproved
	case r.Review == wire.ReviewReadyState:
		return SignalReady
	case r.Review == wire.ReviewWaiting, working:
		return SignalWorking
	case !live && r.LastWork != nil && now.Sub(*r.LastWork) > stale:
		return SignalStalled
	case r.Group != "" || len(r.Sessions) > 0 || len(r.OnBase) > 0:
		return SignalOpen
	}
	return SignalNone
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
