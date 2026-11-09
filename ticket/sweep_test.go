package ticket

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func TestSignal(t *testing.T) {
	old := t0.Add(-8 * 24 * time.Hour)
	live := []SweepSession{{Live: true, State: wire.PhaseWorking}}
	for _, c := range []struct {
		name string
		r    SweepRow
		want Signal
	}{
		{"review merged", SweepRow{Review: wire.ReviewMerged, Group: "/g"}, SignalMerged},
		{"base commit, no group", SweepRow{OnBase: []BaseCommit{{SHA: "a"}}}, SignalMerged},
		{"conflict", SweepRow{Review: wire.ReviewConflict, Group: "/g"}, SignalConflict},
		{"approved", SweepRow{Review: wire.ReviewReadyState, Approved: true, Group: "/g"}, SignalApproved},
		{"ready", SweepRow{Review: wire.ReviewReadyState, Group: "/g"}, SignalReady},
		{"live working", SweepRow{Sessions: live, Group: "/g"}, SignalWorking},
		{"stalled", SweepRow{Group: "/g", LastWork: &old}, SignalStalled},
		{"group only", SweepRow{Group: "/g"}, SignalOpen},
		{"nothing", SweepRow{}, SignalNone},
	} {
		if got := signal(c.r, 7*24*time.Hour, t0); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

type fakeSweepDaemon struct {
	sessions []wire.DaemonSession
	page     wire.ReviewsPage
	reviews  map[string]wire.ReviewDetail
	err      error
}

func (f fakeSweepDaemon) Sessions(context.Context) ([]wire.DaemonSession, error) {
	return f.sessions, f.err
}
func (f fakeSweepDaemon) Reviews(context.Context) (wire.ReviewsPage, error) { return f.page, f.err }
func (f fakeSweepDaemon) Review(_ context.Context, key string) (wire.ReviewDetail, error) {
	if d, ok := f.reviews[key]; ok {
		return d, nil
	}
	return wire.ReviewDetail{}, ErrNoReview
}

// fakeGit has no checkouts to log, and gives one repo for each group.
type fakeGit struct{ groups []string }

func (fakeGit) Run(context.Context, string, ...string) (string, error) { return "", nil }
func (g fakeGit) GroupRepos(_ context.Context, _, key string) ([]wire.ReviewRepo, bool, error) {
	if slices.Contains(g.groups, key) {
		return []wire.ReviewRepo{{Name: "app", Ahead: 1}}, true, nil
	}
	return nil, false, nil
}

func sweepRig(t *testing.T, d SweepDaemon) (Sweeper, string) {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".wt", "a-1", "app"), 0o755)
	return Sweeper{Daemon: d, Git: fakeGit{groups: []string{"A-1"}}, Ledger: NewLedger(filepath.Join(t.TempDir(), "tickets")),
		Root: root, Now: func() time.Time { return t0 }}, root
}

func TestSweepRows(t *testing.T) {
	d := fakeSweepDaemon{
		sessions: []wire.DaemonSession{{ID: "o1", Runtime: "orca", Key: "work:A-1", State: wire.PhaseWorking, StateSince: t0.Add(-time.Hour)}},
		page:     wire.ReviewsPage{NotPushed: []wire.NotPushed{{Repo: "app", Base: "main", Ahead: 1, Items: []string{"A-1"}}}},
	}
	s, root := sweepRig(t, d)
	s.Ledger.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "c9", Role: "work", At: t0.Add(-2 * time.Hour)})
	rep, err := s.Sweep(context.Background(), []string{"a-1"}, "")
	if err != nil || !rep.Daemon || len(rep.Rows) != 1 {
		t.Fatal(rep, err)
	}
	r := rep.Rows[0]
	if len(r.Sessions) != 2 {
		t.Fatalf("%+v", r.Sessions)
	}
	if k := r.Sessions[0]; k.Via != "key" || !k.Live || !k.Stoppable || k.Role != "work" {
		t.Fatalf("%+v", k)
	}
	if l := r.Sessions[1]; l.Via != "ledger" || l.Live || l.ID != "c9" {
		t.Fatalf("%+v", l)
	}
	if r.Group != filepath.Join(root, ".wt", "a-1") || len(r.Repos) != 1 || len(r.NotPushed) != 1 || r.Signal != SignalWorking {
		t.Fatalf("%+v", r)
	}
}

func TestSweepDiscoversKeys(t *testing.T) {
	d := fakeSweepDaemon{sessions: []wire.DaemonSession{
		{ID: "p", Runtime: "orca", Key: "plan:A-4", State: wire.PhaseWorking},
		{ID: "b", Runtime: "orca", Key: "work:B-1", State: wire.PhaseWorking},
	}}
	s, root := sweepRig(t, d)
	os.MkdirAll(filepath.Join(root, ".wt", "a-2"), 0o755)
	s.Ledger.Add(SessionRecord{Key: "A-3", Session: "x", Role: "work", At: t0})
	rep, err := s.Sweep(context.Background(), nil, "a")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range rep.Rows {
		keys = append(keys, r.Key)
	}
	if !slices.Equal(keys, []string{"A-1", "A-2", "A-3", "A-4"}) {
		t.Fatal(keys)
	}
}

func TestSweepNoDaemon(t *testing.T) {
	s, _ := sweepRig(t, fakeSweepDaemon{err: ErrNoDaemon})
	s.Ledger.Add(SessionRecord{Key: "A-1", Session: "x", Role: "work", At: t0})
	rep, err := s.Sweep(context.Background(), []string{"A-1"}, "")
	if err != nil || rep.Daemon || len(rep.Warnings) != 1 {
		t.Fatal(rep, err)
	}
	if r := rep.Rows[0]; r.Group == "" || len(r.Repos) != 1 || len(r.Sessions) != 1 {
		t.Fatalf("%+v", r)
	}
}

// gitGit runs the real git, as review.Git does.
type gitGit struct{}

func (gitGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimRight(string(out), "\n"), err
}
func (gitGit) GroupRepos(context.Context, string, string) ([]wire.ReviewRepo, bool, error) {
	return nil, false, nil
}

func TestSweepBaseCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@t")
	}
	root := t.TempDir()
	app := filepath.Join(root, "app")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", app},
		{"-C", app, "commit", "-q", "--allow-empty", "-m", "Fix x\n\nItem: A-1"},
		{"-C", app, "commit", "-q", "--allow-empty", "-m", "A-12: other work"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", args, out)
		}
	}
	s := Sweeper{Git: gitGit{}, Ledger: NewLedger(t.TempDir()), Root: root, Now: time.Now}
	rep, err := s.Sweep(context.Background(), []string{"A-1", "A-12"}, "")
	if err != nil {
		t.Fatal(err)
	}
	a1, a12 := rep.Rows[0], rep.Rows[1]
	if len(a1.OnBase) != 1 || a1.OnBase[0].Subject != "Fix x" || a1.OnBase[0].Repo != "app" || a1.OnBase[0].Base != "main" {
		t.Fatalf("%+v", a1.OnBase)
	}
	if len(a12.OnBase) != 1 || a12.OnBase[0].Subject != "A-12: other work" || a1.Signal != SignalMerged {
		t.Fatalf("%+v %s", a12.OnBase, a1.Signal)
	}
}
