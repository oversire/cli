package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/wire"
)

func TestMergeFastForward(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	remote := filepath.Join(t.TempDir(), "app.git")
	w.git(w.root, "init", "-q", "--bare", remote)
	w.git(app, "remote", "add", "origin", remote)
	w.git(app, "push", "-q", "-u", "origin", "main")
	g := w.group("app-1", "app")
	head := w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	r := newRig(t, w.root)

	res, err := r.Merge(context.Background(), "APP-1", wire.MergeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged || res.Removed || res.Error != "" || len(res.Repos) != 1 || res.Repos[0].Result != wire.MergeMerged || !strings.HasPrefix(head, res.Repos[0].Head) {
		t.Fatalf("%+v", res)
	}
	if got := w.git(app, "rev-parse", "main"); got != head {
		t.Fatal(got)
	}
	// The fast-forward leaves no merge commit: the reflog names the branch.
	if len(res.NotPushed) != 1 || !reflect.DeepEqual(res.NotPushed[0], wire.NotPushed{Root: w.root, Repo: "app", Base: "main", Ahead: 1, Items: []string{"APP-1"}}) {
		t.Fatalf("%+v", res.NotPushed)
	}
	// The group stays, with nothing to merge: the review is merged and leaves the list.
	if got := list(t, r); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	d, err := r.Get(context.Background(), "APP-1", "")
	if err != nil || d.Summary.State != wire.ReviewMerged {
		t.Fatal(err, d.Summary.State)
	}
	if !slices.Contains(r.eventNames(), wire.EventReviewMerged) {
		t.Fatal(r.eventNames())
	}
	// The id is the one that the git watcher gives to the same merge.
	if m, ok := r.row(wire.KindGitMerge); !ok || m.ID != "git:"+head+":refs/heads/main" || m.Item != "APP-1" || m.ActorType != wire.ActorHuman {
		t.Fatalf("%+v", m)
	}
	if s, ok := r.row(wire.KindItemShipped); !ok || s.Summary != "Shipped app" {
		t.Fatalf("%+v", s)
	}
	// The daemon never pushes.
	if got := w.git(remote, "rev-parse", "main"); got == head {
		t.Fatal("pushed")
	}
}

func TestMergeRebasesFirst(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	theirs := w.commit(app, "Other", map[string]string{"o.go": "package lib\n"})
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "APP-1", wire.MergeRequest{})
	if err != nil || !res.Merged || res.Repos[0].Result != wire.MergeRebased {
		t.Fatalf("%v %+v", err, res)
	}
	if w.git(app, "rev-parse", "main") != w.git(filepath.Join(g, "app"), "rev-parse", "HEAD") {
		t.Fatal("main is not the rebased branch")
	}
	w.git(app, "merge-base", "--is-ancestor", theirs, "main")
}

func TestMergeConflict(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	wt := filepath.Join(g, "app")
	mine := w.commit(wt, "Mine", map[string]string{"lib.go": "package lib\n\nfunc Mine() {}\n"})
	theirs := w.commit(app, "Theirs", map[string]string{"lib.go": "package lib\n\nfunc Theirs() {}\n"})
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "APP-1", wire.MergeRequest{RemoveGroup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged || res.Removed || res.Repos[0].Result != wire.MergeStopped || !slices.Equal(res.Repos[0].ConflictFiles, []string{"lib.go"}) ||
		!strings.Contains(res.Error, "conflict") {
		t.Fatalf("%+v", res)
	}
	// The rebase went back: the branch and the base are as before, and no rebase is open.
	if w.git(wt, "rev-parse", "HEAD") != mine || w.git(app, "rev-parse", "main") != theirs || w.git(wt, "status", "--porcelain") != "" {
		t.Fatal("not back")
	}
	if s := list(t, r)[0]; s.State != wire.ReviewConflict {
		t.Fatal(s.State)
	}
	if !slices.Contains(r.eventNames(), wire.EventReviewConflict) {
		t.Fatal(r.eventNames())
	}
}

func TestMergeAllOrNone(t *testing.T) {
	w := newWS(t)
	ov, cli := w.repo("oversire"), w.repo("cli")
	g := w.group("os-5", "cli", "oversire")
	w.commit(filepath.Join(g, "oversire"), "App part", map[string]string{"a.swift": "let a = 1\n"})
	w.commit(filepath.Join(g, "cli"), "CLI part", map[string]string{"x.txt": "mine\n"})
	// An untracked file in the primary checkout of cli stops its fast-forward.
	w.write(filepath.Join(cli, "x.txt"), "local\n")
	old := w.git(ov, "rev-parse", "main")
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "OS-5", wire.MergeRequest{RemoveGroup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged || res.Removed || len(res.Repos) != 2 || res.Repos[0].Name != "oversire" || res.Repos[0].Result != wire.MergeSkipped || res.Repos[0].Head != "" ||
		res.Repos[1].Name != "cli" || res.Repos[1].Result != wire.MergeStopped || !strings.HasPrefix(res.Error, "cli: ") {
		t.Fatalf("%+v", res)
	}
	if got := w.git(ov, "rev-parse", "main"); got != old {
		t.Fatal("the base of oversire did not go back")
	}
	if _, err := os.Stat(g); err != nil {
		t.Fatal(err)
	}
}

func TestMergeChecks(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	wt := filepath.Join(g, "app")
	r := newRig(t, w.root)
	ctx := context.Background()
	if _, err := r.Merge(ctx, "APP-1", wire.MergeRequest{}); !kindIs(err, Conflict) {
		t.Fatal("no commits:", err)
	}
	w.commit(wt, "Work", map[string]string{"n.go": "package lib\n", "README.md": "branch\n"})
	w.write(filepath.Join(wt, "wip.go"), "package lib\n")
	if _, err := r.Merge(ctx, "APP-1", wire.MergeRequest{}); !kindIs(err, Conflict) {
		t.Fatal("dirty worktree:", err)
	}
	os.Remove(filepath.Join(wt, "wip.go"))
	w.write(filepath.Join(app, "README.md"), "changed\n")
	_, err := r.Merge(ctx, "APP-1", wire.MergeRequest{})
	if !kindIs(err, Conflict) || !strings.Contains(err.Error(), "primary checkout") || !strings.Contains(err.Error(), "README.md") {
		t.Fatal("dirty primary:", err)
	}
	w.git(app, "checkout", "-q", "README.md")
	r.book.put(session("s1", wire.PhaseWorking, r.clock.now(), wt))
	if _, err := r.Merge(ctx, "APP-1", wire.MergeRequest{}); !kindIs(err, Conflict) {
		t.Fatal("working session:", err)
	}
	if _, err := r.Merge(ctx, "APP-2", wire.MergeRequest{}); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
	if w.git(app, "rev-parse", "main") == w.git(wt, "rev-parse", "HEAD") {
		t.Fatal("merged after a failed check")
	}
}

func TestMergeRemovesGroup(t *testing.T) {
	w := newWS(t)
	for _, n := range []string{"cli", "cloud", "oversire"} {
		w.repo(n)
	}
	w.write(filepath.Join(w.root, "CLAUDE.md"), "rules\n")
	g := w.group("os-5", "cli", "cloud", "oversire")
	if err := os.Symlink("../../CLAUDE.md", filepath.Join(g, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	w.commit(filepath.Join(g, "cli"), "CLI part", map[string]string{"c.go": "package c\n"})
	w.commit(filepath.Join(g, "oversire"), "App part", map[string]string{"a.swift": "let a = 1\n"})
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "OS-5", wire.MergeRequest{RemoveGroup: true})
	if err != nil || !res.Merged || !res.Removed || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	results := map[string]wire.MergeOutcome{}
	for _, x := range res.Repos {
		results[x.Name] = x.Result
	}
	if results["cloud"] != wire.MergeSkipped || results["cli"] != wire.MergeMerged || results["oversire"] != wire.MergeMerged {
		t.Fatalf("%+v", res.Repos)
	}
	if _, err := os.Lstat(g); !os.IsNotExist(err) {
		t.Fatal("the group stays", err)
	}
	for _, n := range []string{"cli", "cloud", "oversire"} {
		dir := filepath.Join(w.root, n)
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
			t.Fatal(err)
		}
		if w.git(dir, "status", "--porcelain") != "" || w.git(dir, "branch", "--list", "os-5") != "" || strings.Count(w.git(dir, "worktree", "list"), "\n") != 0 {
			t.Fatalf("%s is not clean", n)
		}
	}
	if _, err := os.Stat(filepath.Join(w.root, "CLAUDE.md")); err != nil {
		t.Fatal("the target of the link is gone")
	}
	// The review of the removed group is merged.
	d, err := r.Get(context.Background(), "OS-5", "")
	if err != nil || d.Summary.State != wire.ReviewMerged {
		t.Fatal(err, d.Summary)
	}
}

// A change in the primary checkout in a file that the branch does not change stays through the merge.
func TestMergeKeepsPrimaryChanges(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	head := w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	w.write(filepath.Join(app, "README.md"), "changed\n")
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "APP-1", wire.MergeRequest{})
	if err != nil || !res.Merged || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	if got := w.git(app, "rev-parse", "main"); got != head {
		t.Fatal(got)
	}
	if data, err := os.ReadFile(filepath.Join(app, "README.md")); err != nil || string(data) != "changed\n" {
		t.Fatal("the change is gone", err)
	}
}

func TestMergeKeepsUnknownFiles(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	w.write(filepath.Join(g, "notes.txt"), "keep me\n")
	r := newRig(t, w.root)
	res, err := r.Merge(context.Background(), "APP-1", wire.MergeRequest{RemoveGroup: true})
	if err != nil || !res.Merged || res.Removed || !strings.Contains(res.Error, "the group stays") {
		t.Fatalf("%v %+v", err, res)
	}
	if data, err := os.ReadFile(filepath.Join(g, "notes.txt")); err != nil || string(data) != "keep me\n" {
		t.Fatal("the file is gone", err)
	}
}

func TestNotPushedItems(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	remote := filepath.Join(t.TempDir(), "app.git")
	w.git(w.root, "init", "-q", "--bare", remote)
	w.git(app, "remote", "add", "origin", remote)
	w.git(app, "push", "-q", "-u", "origin", "main")
	gt := NewGit(proc.Local{})
	ctx := context.Background()

	w.commit(app, "Fix the docs", map[string]string{"a.md": "a\n"})
	if np, ok := gt.notPushed(ctx, w.root, "app"); !ok || np.Items != nil {
		t.Fatalf("%+v", np)
	}
	w.commit(app, "Status card (OVERSIRE-383, mcpq-2)\n\nItem: SLING-9", map[string]string{"b.md": "b\n"})
	w.git(app, "checkout", "-q", "-b", "web-12")
	w.commit(app, "Page", map[string]string{"c.md": "c\n"})
	w.git(app, "checkout", "-q", "main")
	w.git(app, "merge", "-q", "--ff-only", "web-12")
	np, ok := gt.notPushed(ctx, w.root, "app")
	if !ok || np.Ahead != 3 || !reflect.DeepEqual(np.Items, []string{"OVERSIRE-383", "SLING-9", "WEB-12"}) {
		t.Fatalf("%+v", np)
	}
	w.git(app, "push", "-q")
	if _, ok := gt.notPushed(ctx, w.root, "app"); ok {
		t.Fatal("pushed")
	}
}
