package oversire

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestChangedPaths(t *testing.T) {
	repo := gitRepo(t)
	write(t, filepath.Join(repo, "a.txt"), "a")
	write(t, filepath.Join(repo, "src", "b.txt"), "b")
	write(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	gitOut(t, repo, "add", ".")
	gitOut(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "files")
	w := NewWorktrees(t.TempDir(), proc.Local{})
	ctx := context.Background()
	dir, err := w.Ensure(ctx, repo, "APP-1", "app-1")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "src", "b.txt"), "changed")        // a change, not committed
	write(t, filepath.Join(dir, "new", "c.txt"), "c")              // a new file
	write(t, filepath.Join(dir, "x.log"), "ignored")               // ignored
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil { // a delete
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "d.txt"), "d")
	gitOut(t, dir, "add", "d.txt")
	gitOut(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "d") // a commit on the branch
	// A new commit on the base does not count.
	write(t, filepath.Join(repo, "main.txt"), "m")
	gitOut(t, repo, "add", ".")
	gitOut(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "main")

	got, err := w.Changed(ctx, repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"a.txt", "d.txt", "new/c.txt", "src/b.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("paths %v, want %v", got, want)
	}
}

type pathsRecorder struct {
	recorder
	pmu   sync.Mutex
	paths []wire.Paths
}

func (r *pathsRecorder) ReportPaths(order string, p wire.ChangedPaths) {
	r.pmu.Lock()
	r.paths = append(r.paths, wire.Paths{Order: order, Paths: p})
	r.pmu.Unlock()
}

func (r *pathsRecorder) all() []wire.Paths {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return slices.Clone(r.paths)
}

func TestTurnReportsChangedPathsOnce(t *testing.T) {
	f := setup(t, chat, Options{})
	rec := &pathsRecorder{}
	f.rt.report = rec
	item := wire.ItemRef{Project: wire.ProjectRef{Account: wire.AccountID{Provider: "plane", ID: "ocral"}, ID: "p1", Key: "APP"}, ID: "i9", Key: "APP-9", URL: "u"}
	ref, err := f.rt.Start(context.Background(), runtime.Order{ID: "o9"}, wire.LaunchRequest{Repo: f.repo, Item: &item, Agent: "fake", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, ref.ID, wire.SessionWaitingInput)
	dir := filepath.Join(f.data, "worktrees", "app", "APP-9")
	write(t, filepath.Join(dir, "x.go"), "x")
	turn := func(text string) {
		t.Helper()
		if err := f.rt.Send(context.Background(), wire.SendPayload{Session: ref, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	turn("two")
	waitFor(t, func() bool { return len(rec.all()) == 1 })
	turn("three")
	time.Sleep(300 * time.Millisecond)
	got := rec.all()
	if len(got) != 1 {
		t.Fatalf("reports %d, want 1: %+v", len(got), got)
	}
	p := got[0]
	if p.Order != "o9" || p.Paths.ID != "ocral/p1/i9" || p.Paths.Subject.Kind != "item" || !slices.Equal(p.Paths.Paths, []string{"x.go"}) {
		t.Errorf("paths %+v", p)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timeout")
}
