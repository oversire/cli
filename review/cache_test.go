package review

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/wire"
)

// countRunner counts the git calls and the most calls at the same time.
type countRunner struct {
	delay     time.Duration
	calls     atomic.Int64
	now, most atomic.Int64
}

func (c *countRunner) Run(ctx context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	c.calls.Add(1)
	n := c.now.Add(1)
	defer c.now.Add(-1)
	for {
		m := c.most.Load()
		if n <= m || c.most.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(c.delay)
	return proc.Local{}.Run(ctx, exe, args, stdin)
}

func countRig(t *testing.T, root string) (*rig, *countRunner) {
	t.Helper()
	run := &countRunner{}
	r := &rig{book: newFakeBook(), sender: &fakeSender{}, launch: &fakeLauncher{},
		clock: &clock{at: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, store: t.TempDir(), run: run}
	r.Engine = r.engine(root)
	return r, run
}

func TestCacheReadMakesNoGitCall(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"new.go": "package lib\n"})
	r, run := countRig(t, w.root)
	if len(list(t, r)) != 1 || run.calls.Load() == 0 {
		t.Fatal("first read")
	}
	n := run.calls.Load()
	if len(list(t, r)) != 1 || run.calls.Load() != n {
		t.Fatalf("second read made %d git calls", run.calls.Load()-n)
	}
}

// The bug of OVERSIRE-421: a conflict that the agent fixed must not stay in the list.
func TestCacheLiveAfterRefChange(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	wt := filepath.Join(g, "app")
	w.commit(wt, "Mine", map[string]string{"lib.go": "package lib\n\nfunc Mine() {}\n"})
	w.commit(app, "Theirs", map[string]string{"lib.go": "package lib\n\nfunc Theirs() {}\n"})
	r, _ := countRig(t, w.root)
	if s := list(t, r)[0]; s.Repos[0].Merge != wire.MergeConflict {
		t.Fatalf("%+v", s.Repos[0])
	}
	w.git(wt, "rebase", "-q", "-X", "theirs", "main")
	// No time passes: the read is in the settle time, and it is live.
	s := list(t, r)[0]
	if s.Repos[0].Merge != wire.MergeFastForward || s.Repos[0].Behind != 0 || s.Repos[0].Ahead != 1 {
		t.Fatalf("%+v", s.Repos[0])
	}
}

func TestCacheLoopSettles(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	wt := filepath.Join(g, "app")
	w.commit(wt, "Work", map[string]string{"new.go": "package lib\n"})
	r, run := countRig(t, w.root)
	list(t, r)
	var mu sync.Mutex
	var keys []string
	changed := func(k string) { mu.Lock(); keys = append(keys, k); mu.Unlock() }
	ctx := context.Background()

	w.commit(wt, "More", map[string]string{"more.go": "package lib\n"})
	n := run.calls.Load()
	r.cache.step(ctx, changed)
	if run.calls.Load() != n || len(keys) != 0 {
		t.Fatal("the change has not settled yet")
	}
	r.clock.add(settle)
	r.cache.step(ctx, changed)
	if run.calls.Load() == n || !slices.Equal(keys, []string{"APP-1"}) {
		t.Fatalf("calls %d, keys %v", run.calls.Load()-n, keys)
	}
	// The loop kept the new state: a read makes no git call.
	n = run.calls.Load()
	if s := list(t, r)[0]; s.Repos[0].Ahead != 2 || run.calls.Load() != n {
		t.Fatalf("%+v, %d calls", s.Repos[0], run.calls.Load()-n)
	}
	// The sweep computes an old state again, and tells nothing when it did not change.
	r.clock.add(sweepEvery)
	r.cache.step(ctx, changed)
	if run.calls.Load() == n || len(keys) != 1 {
		t.Fatalf("calls %d, keys %v", run.calls.Load()-n, keys)
	}
}

func TestCacheSettleMax(t *testing.T) {
	m := mark{}
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	m.done(start, nil)
	m.seen, m.stale, m.last = "x", start, start.Add(settleMax-time.Second)
	if !m.due(start.Add(settleMax)) {
		t.Fatal("a change that goes on is due after settleMax")
	}
	m.stale = start.Add(time.Minute)
	if m.due(start.Add(settleMax)) {
		t.Fatal("not due")
	}
}

func TestCacheGoneGroup(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"new.go": "package lib\n"})
	r, _ := countRig(t, w.root)
	list(t, r)
	w.git(app, "worktree", "remove", "--force", filepath.Join(g, "app"))
	if err := os.Remove(g); err != nil {
		t.Fatal(err)
	}
	if s := list(t, r); len(s) != 0 {
		t.Fatalf("%+v", s)
	}
	if len(r.cache.groups) != 0 {
		t.Fatal("the cache keeps a gone group")
	}
}

func TestCacheStoppedRequestKeepsResult(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"new.go": "package lib\n"})
	r, run := countRig(t, w.root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	page, err := r.List(ctx, wire.ReviewCode)
	if err != nil || len(page.Reviews) != 1 {
		t.Fatal(err, page.Reviews)
	}
	n := run.calls.Load()
	list(t, r)
	if run.calls.Load() != n {
		t.Fatal("the stopped read did not keep its result")
	}
}

func TestCacheParallel(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	w.repo("cli")
	for _, k := range []string{"app-1", "app-2", "app-3"} {
		g := w.group(k, "app", "cli")
		w.commit(filepath.Join(g, "app"), "Work", map[string]string{k + ".go": "package lib\n"})
	}
	r, run := countRig(t, w.root)
	run.delay = 20 * time.Millisecond
	if len(list(t, r)) != 3 {
		t.Fatal("three groups")
	}
	if run.most.Load() < 4 {
		t.Fatalf("at most %d git calls at the same time", run.most.Load())
	}
}

func TestCacheNotPushed(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	remote := filepath.Join(t.TempDir(), "app.git")
	w.git(w.root, "init", "-q", "--bare", remote)
	w.git(app, "remote", "add", "origin", remote)
	w.git(app, "push", "-q", "-u", "origin", "main")
	r, run := countRig(t, w.root)
	notPushed := func() []wire.NotPushed {
		page, err := r.List(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		return page.NotPushed
	}
	if np := notPushed(); len(np) != 0 {
		t.Fatalf("%+v", np)
	}
	w.commit(app, "Fix", map[string]string{"a.md": "a\n"})
	if np := notPushed(); len(np) != 1 || np[0].Ahead != 1 {
		t.Fatalf("%+v", np)
	}
	n := run.calls.Load()
	notPushed()
	if run.calls.Load() != n {
		t.Fatal("an unchanged base made git calls")
	}
	w.git(app, "push", "-q")
	if np := notPushed(); len(np) != 0 {
		t.Fatalf("pushed: %+v", np)
	}
}
