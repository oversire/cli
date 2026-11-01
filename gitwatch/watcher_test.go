package gitwatch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/review"
	"github.com/oversire/cli/wire"
)

type rows struct {
	mu  sync.Mutex
	got []wire.EventDraft
	ses []string
}

func (r *rows) Add(session string, d wire.EventDraft) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got, r.ses = append(r.got, d), append(r.ses, session)
}

func (r *rows) take() []wire.EventDraft {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got, r.ses = nil, nil
	return out
}

type book []wire.DaemonSession

func (b book) All() []wire.DaemonSession { return b }

func script(t *testing.T, root, step string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture script needs sh")
	}
	if out, err := exec.Command("sh", "testdata/make-repo.sh", root, step).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", step, err, out)
	}
}

func setup(t *testing.T, sessions Sessions) (string, *rows, func() *Watcher) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script(t, root, "base")
	out := &rows{}
	cursors := filepath.Join(t.TempDir(), "gitwatch.json")
	mk := func() *Watcher {
		return NewWatcher(review.NewGit(proc.Local{}), sessions, out, events.NewResolver(nil, nil),
			func(context.Context) []string { return []string{root} }, cursors, nil)
	}
	return root, out, mk
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func byKind(got []wire.EventDraft) map[string][]wire.EventDraft {
	m := map[string][]wire.EventDraft{}
	for _, d := range got {
		m[d.Kind] = append(m[d.Kind], d)
	}
	return m
}

func TestReflogRows(t *testing.T) {
	root, out, mk := setup(t, nil)
	ctx := context.Background()
	w := mk()
	if n := w.Scan(ctx); n != 0 {
		t.Fatalf("the first read gives %d rows", n)
	}
	script(t, root, "work")
	w.Scan(ctx)
	got := out.take()
	var kinds []string
	for _, d := range got {
		kinds = append(kinds, d.Kind)
	}
	sort.Strings(kinds)
	want := []string{"git.commit", "git.commit", "git.commit", "git.merge", "git.push", "git.rebase", "git.reset", "worktree.added"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	cat, err := wire.LoadEventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		if p := cat.Problems(d); len(p) > 0 {
			t.Errorf("%s: %v", d.ID, p)
		}
		if ids[d.ID] {
			t.Errorf("same id %s", d.ID)
		}
		ids[d.ID] = true
	}
	k := byKind(got)
	wt := filepath.Join(root, ".wt", "app-7", "app")
	var add wire.EventDraft
	for _, d := range k["git.commit"] {
		if d.Summary == "Add b" {
			add = d
		}
	}
	var p map[string]any
	json.Unmarshal(add.Payload, &p)
	if add.Item != "APP-7" || add.ActorType != wire.ActorHuman || add.Actor != "dev@example.com" || p["worktree"] != wt ||
		p["files"] != 1.0 || p["add"] != 2.0 || p["del"] != 0.0 {
		t.Errorf("commit %+v %s", add, add.Payload)
	}
	m := k["git.merge"][0]
	json.Unmarshal(m.Payload, &p)
	if m.Summary != "Merged app-7 into main" || m.Item != "APP-7" || p["mode"] != "fastForward" || p["commits"] != 2.0 || m.ID != "git:"+p["sha"].(string)+":refs/heads/main" {
		t.Errorf("merge %+v %s", m, m.Payload)
	}
	if push := k["git.push"][0]; push.ID != "git:"+p["sha"].(string)+":refs/remotes/origin/main" || push.Summary != "Pushed main to origin" {
		t.Errorf("push %+v", push)
	}
	if r := k["git.rebase"][0]; r.Item != "APP-7" {
		t.Errorf("rebase %+v", r)
	}
	if r := k["git.reset"][0]; r.Summary != "Reset app-7 to HEAD~1" {
		t.Errorf("reset %+v", r)
	}
	if a := k["worktree.added"][0]; a.Item != "APP-7" || a.Summary != "Added worktree app on app-7" {
		t.Errorf("worktree %+v", a)
	}

	// A second read, and a new watcher on the same cursors, give no rows.
	if n := w.Scan(ctx); n != 0 {
		t.Fatalf("second read: %d rows", n)
	}
	if n := mk().Scan(ctx); n != 0 {
		t.Fatalf("restart: %d rows", n)
	}

	if out, err := exec.Command("git", "-C", filepath.Join(root, "app"), "worktree", "remove", "--force", wt).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	w.Scan(ctx)
	if got := out.take(); len(got) != 1 || got[0].Kind != wire.KindWorktreeRemoved || got[0].Item != "APP-7" {
		t.Fatalf("%+v", got)
	}
}

func TestAgentCommit(t *testing.T) {
	var sessions book
	root, out, mk := setup(t, &sessions)
	ctx := context.Background()
	w := mk()
	w.Scan(ctx)
	wt := filepath.Join(root, ".wt", "app-7", "app")
	sessions = book{{ID: "s1", Key: "work:APP-7", Harness: "claude", State: wire.PhaseWorking, Cwd: wt}}
	script(t, root, "work")
	w.Scan(ctx)
	out.mu.Lock()
	defer out.mu.Unlock()
	agent := 0
	for i, d := range out.got {
		if d.Kind == wire.KindGitCommit && d.Summary == "Add b" && (d.ActorType != wire.ActorAgent || out.ses[i] != "s1" || d.Actor != "") {
			t.Errorf("%+v %q", d, out.ses[i])
		}
		if d.ActorType == wire.ActorAgent {
			agent++
		}
	}
	// The worktree rows: commits, rebase and reset. The main rows stay human.
	if agent != 4 {
		t.Errorf("agent rows %d", agent)
	}
}

// A turn that ends before the scan still owns its commit. A commit after the turn is human.
func TestCommitBeforeTheTurnEnded(t *testing.T) {
	var sessions book
	root, out, mk := setup(t, &sessions)
	ctx := context.Background()
	w := mk()
	w.Scan(ctx)
	wt := filepath.Join(root, ".wt", "app-7", "app")
	git(t, filepath.Join(root, "app"), "worktree", "add", "-q", wt, "-b", "app-7")
	w.Scan(ctx)
	git(t, wt, "commit", "--allow-empty", "-qm", "In the turn")
	sessions = book{{ID: "s1", Harness: "claude", State: wire.PhaseWaiting, StateSince: time.Now().Add(time.Second), Cwd: wt}}
	w.Scan(ctx)
	sessions[0].StateSince = time.Now().Add(-time.Hour)
	git(t, wt, "commit", "--allow-empty", "-qm", "After the turn")
	w.Scan(ctx)
	out.mu.Lock()
	defer out.mu.Unlock()
	got := map[string]wire.ActorType{}
	for _, d := range out.got {
		if d.Kind == wire.KindGitCommit {
			got[d.Summary] = d.ActorType
		}
	}
	if got["In the turn"] != wire.ActorAgent || got["After the turn"] != wire.ActorHuman {
		t.Errorf("%v", got)
	}
}
