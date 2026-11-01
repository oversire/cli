package review

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oversire/cli/wire"
)

func TestOneRepo(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Add the store", map[string]string{"store.go": "package lib\n\nfunc Store() {}\n"})
	w.commit(filepath.Join(g, "app"), "Use the store\n\nItem: APP-2", map[string]string{"lib.go": "package lib\n\nfunc A() { Store() }\n"})
	r := newRig(t, w.root)
	ctx := context.Background()

	page, err := r.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Reviews) != 1 {
		t.Fatalf("%+v", page.Reviews)
	}
	s := page.Reviews[0]
	if s.Key != "APP-1" || s.Kind != wire.ReviewCode || s.State != wire.ReviewReadyState || s.Group != ".wt/app-1" || s.Root != w.root || s.Machine != "mbp" {
		t.Fatalf("%+v", s)
	}
	if s.Title != "Add the store" || !slices.Equal(s.Subitems, []string{"APP-2"}) || s.Session.State != wire.ReviewSessionNone {
		t.Fatalf("title %q, subitems %v, session %+v", s.Title, s.Subitems, s.Session)
	}
	repo := s.Repos[0]
	if repo.Name != "app" || repo.Branch != "app-1" || repo.Base != "main" || repo.Ahead != 2 || repo.Behind != 0 ||
		repo.Merge != wire.MergeFastForward || repo.Files != 2 || repo.Added != 4 || repo.Removed != 1 || repo.Dirty {
		t.Fatalf("%+v", repo)
	}
	if len(repo.Commits) != 2 || repo.Commits[0].Subject != "Use the store" || !slices.Equal(repo.Commits[0].Items, []string{"APP-2"}) {
		t.Fatalf("%+v", repo.Commits)
	}

	d, err := r.Get(ctx, "APP-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[0].Path != "lib.go" || d.Files[0].Status != wire.FileModified || d.Files[1].Status != wire.FileAdded {
		t.Fatalf("%+v", d.Files)
	}
	fd, err := r.Diff(ctx, "APP-1", "app", "lib.go")
	if err != nil {
		t.Fatal(err)
	}
	if fd.Status != wire.FileModified || len(fd.Hunks) != 1 || fd.Hunks[0].Lines[3].Text != "func A() { Store() }" {
		t.Fatalf("%+v", fd)
	}
	if _, err := r.Diff(ctx, "APP-1", "app", "none.go"); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "APP-9", ""); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
}

func TestThreeReposWithLink(t *testing.T) {
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

	gr, ok := findGroup([]string{w.root}, "OS-5")
	if !ok || !slices.Equal(gr.Links, []string{"CLAUDE.md"}) || len(gr.Repos) != 3 {
		t.Fatalf("%+v", gr)
	}
	page, err := r.List(context.Background(), wire.ReviewCode)
	if err != nil || len(page.Reviews) != 1 {
		t.Fatal(err, page)
	}
	var names []string
	for _, x := range page.Reviews[0].Repos {
		names = append(names, x.Name)
	}
	// The merge order of the rig is oversire, cli, then the others.
	if !slices.Equal(names, []string{"oversire", "cli", "cloud"}) || page.Reviews[0].Repos[2].Ahead != 0 {
		t.Fatalf("%v %+v", names, page.Reviews[0].Repos)
	}
	if page.Reviews[0].Title != "App part" {
		t.Fatal(page.Reviews[0].Title)
	}
	d, err := r.Get(context.Background(), "OS-5", wire.ReviewCode)
	if err != nil || len(d.Files) != 2 {
		t.Fatal(err, d.Files)
	}
}

func TestBaseMoved(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"new.go": "package lib\n"})
	w.commit(app, "Other work", map[string]string{"other.go": "package lib\n"})
	r := newRig(t, w.root)
	s := list(t, r)[0]
	if s.Repos[0].Behind != 1 || s.Repos[0].Ahead != 1 || s.Repos[0].Merge != wire.MergeRebase || s.State != wire.ReviewReadyState {
		t.Fatalf("%+v", s.Repos[0])
	}
	// The diff starts at the merge base: the commit of main is not in it.
	d, _ := r.Get(context.Background(), "APP-1", "")
	if len(d.Files) != 1 || d.Files[0].Path != "new.go" {
		t.Fatalf("%+v", d.Files)
	}
}

func TestConflictState(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Mine", map[string]string{"lib.go": "package lib\n\nfunc Mine() {}\n"})
	w.commit(app, "Theirs", map[string]string{"lib.go": "package lib\n\nfunc Theirs() {}\n"})
	r := newRig(t, w.root)
	s := list(t, r)[0]
	if s.Repos[0].Merge != wire.MergeConflict || !slices.Equal(s.Repos[0].ConflictFiles, []string{"lib.go"}) {
		t.Fatalf("%+v", s.Repos[0])
	}
}

func TestUntrackedAndDirty(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.write(filepath.Join(g, "app", "notes.txt"), "one\ntwo\n")
	w.write(filepath.Join(g, "app", "lib.go"), "package lib\n\nfunc A() { return }\n")
	r := newRig(t, w.root)
	s := list(t, r)[0]
	if s.Repos[0].Ahead != 0 || !s.Repos[0].Dirty {
		t.Fatalf("%+v", s.Repos[0])
	}
	d, err := r.Get(context.Background(), "APP-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[0].Path != "lib.go" || d.Files[0].Untracked || !d.Files[1].Untracked || d.Files[1].Added != 2 {
		t.Fatalf("%+v", d.Files)
	}
	fd, err := r.Diff(context.Background(), "APP-1", "app", "notes.txt")
	if err != nil || fd.Status != wire.FileAdded || len(fd.Hunks) != 1 || len(fd.Hunks[0].Lines) != 2 {
		t.Fatalf("%v %+v", err, fd)
	}
}

func TestRenameAndBinary(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	wt := filepath.Join(g, "app")
	w.git(wt, "mv", "lib.go", "core.go")
	w.commit(wt, "Rename and add an image", map[string]string{"i.png": "PNG\x00\x01\x02"})
	r := newRig(t, w.root)
	d, err := r.Get(context.Background(), "APP-1", "")
	if err != nil {
		t.Fatal(err)
	}
	var ren, bin wire.ReviewFile
	for _, f := range d.Files {
		switch f.Path {
		case "core.go":
			ren = f
		case "i.png":
			bin = f
		}
	}
	if ren.Status != wire.FileRenamed || ren.OldPath != "lib.go" || len(d.Files) != 2 {
		t.Fatalf("%+v", d.Files)
	}
	if !bin.Binary || bin.Status != wire.FileAdded {
		t.Fatalf("%+v", bin)
	}
	fd, err := r.Diff(context.Background(), "APP-1", "app", "core.go")
	if err != nil || fd.Status != wire.FileRenamed || fd.OldPath != "lib.go" {
		t.Fatalf("%v %+v", err, fd)
	}
	fd, err = r.Diff(context.Background(), "APP-1", "app", "i.png")
	if err != nil || !fd.Binary || len(fd.Hunks) != 0 {
		t.Fatalf("%v %+v", err, fd)
	}
}

func TestEmptyGroupNotListed(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	w.group("app-1", "app")
	if err := os.MkdirAll(filepath.Join(w.root, ".wt", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, w.root)
	if got := list(t, r); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestSessionLookup(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	r := newRig(t, w.root)
	now := r.clock.now()
	r.book.put(wire.DaemonSession{ID: "other", Runtime: wire.RuntimeOrca, Harness: "claude", Cwd: w.root, State: wire.PhaseWorking, StateSince: now})
	r.book.put(wire.DaemonSession{ID: "s1", Runtime: wire.RuntimeOrca, Harness: "claude", Terminal: "t1", Cwd: filepath.Join(g, "app"), Model: "opus",
		State: wire.PhaseWorking, StateSince: now})
	s := list(t, r)[0]
	if s.State != wire.ReviewWaiting || s.Session.ID != "s1" || s.Session.State != wire.ReviewSessionWorking {
		t.Fatalf("%+v %+v", s, s.Session)
	}
	r.book.put(wire.DaemonSession{ID: "s1", Runtime: wire.RuntimeOrca, Harness: "claude", Cwd: filepath.Join(g, "app"),
		State: wire.PhaseWaiting, Reason: wire.ReasonTurnEnd, StateSince: now})
	if s := list(t, r)[0]; s.State != wire.ReviewReadyState || s.Session.State != wire.ReviewSessionIdle || !s.Session.CanSend {
		t.Fatalf("%+v", s.Session)
	}
	r.book.put(wire.DaemonSession{ID: "s1", Runtime: wire.RuntimeOrca, Harness: "claude", Cwd: filepath.Join(g, "app"),
		State: wire.PhaseWaiting, Reason: wire.ReasonQuestion, StateSince: now})
	if s := list(t, r)[0]; s.Session.State != wire.ReviewSessionWorking {
		t.Fatalf("%+v", s.Session)
	}
	// The session goes from the book: the review knows it ended and keeps its agent.
	r.book.mu.Lock()
	r.book.sessions = nil
	r.book.mu.Unlock()
	if s := list(t, r)[0]; s.Session.State != wire.ReviewSessionEnded || s.Session.ID != "s1" {
		t.Fatalf("%+v", s.Session)
	}
	r.mu.Lock()
	rec := r.recs[id(wire.ReviewCode, "APP-1")]
	r.mu.Unlock()
	if rec.Agent == nil || rec.Agent.Launch.CLI != "claude" || rec.Agent.Runtime != wire.RuntimeOrca {
		t.Fatalf("%+v", rec.Agent)
	}

	// A plan session by its start key.
	r.book.put(wire.DaemonSession{ID: "p1", Runtime: wire.RuntimeOversire, Harness: "codex", Key: "plan:APP-1", State: wire.PhaseWorking, StateSince: now})
	if _, rs := r.sessionOf(wire.ReviewPlan, "APP-1", ""); rs.ID != "p1" || rs.State != wire.ReviewSessionWorking {
		t.Fatalf("%+v", rs)
	}
	r.book.put(wire.DaemonSession{ID: "p1", Runtime: wire.RuntimeOversire, Harness: "codex", Key: "plan:APP-1", State: wire.PhaseDone, StateSince: now})
	if _, rs := r.sessionOf(wire.ReviewPlan, "APP-1", ""); rs.State != wire.ReviewSessionEnded || rs.CanSend {
		t.Fatalf("%+v", rs)
	}
}

func list(t *testing.T, r *rig) []wire.ReviewSummary {
	t.Helper()
	page, err := r.List(context.Background(), wire.ReviewCode)
	if err != nil {
		t.Fatal(err)
	}
	return page.Reviews
}
