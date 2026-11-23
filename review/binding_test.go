package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func bound(key, root, group string) wire.WorktreeBinding {
	return wire.WorktreeBinding{Key: key, Machine: "mbp", Root: root, Group: group, Branch: strings.ToLower(key), Mode: wire.WorktreeOwn, Source: "create"}
}

// boundRig gives a rig whose engine has the bindings.
func boundRig(t *testing.T, bs fakeBindings, roots ...string) *rig {
	t.Helper()
	r := newRig(t, roots...)
	r.bindings = bs
	r.Engine = r.engine(roots...)
	return r
}

func TestGroupOfUsesBinding(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	app := w.repo("app")
	dir := filepath.Join(w.root, "other", "grp")
	os.MkdirAll(dir, 0o755)
	w.git(app, "worktree", "add", "-q", "-b", "x-1", filepath.Join(dir, "app"), "main")
	w.commit(filepath.Join(dir, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	r := boundRig(t, fakeBindings{"X-1": bound("X-1", w.root, "other/grp")}, w.root)

	d, err := r.Get(context.Background(), "X-1", "", Fold{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.Group != "other/grp" || d.Summary.Key != "X-1" || len(d.Files) != 1 {
		t.Fatalf("%+v %+v", d.Summary, d.Files)
	}
}

func TestGroupOfFallsBackToWt(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	w.repo("app")
	g := w.group("x-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n"})
	r := boundRig(t, fakeBindings{"X-1": {Key: "X-1", Machine: "mini", Root: w.root, Group: "elsewhere", Mode: wire.WorktreeOwn}}, w.root)

	d, err := r.Get(context.Background(), "X-1", "", Fold{})
	if err != nil || d.Summary.Group != ".wt/x-1" {
		t.Fatal(err, d.Summary.Group)
	}
}

func TestSingleWorktreeGroup(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	app := w.repo("app")
	dir := filepath.Join(t.TempDir(), "quick-fix")
	w.git(app, "worktree", "add", "-q", "-b", "quick-fix", dir, "main")
	w.commit(dir, "Fix\n\nItem: X-2", map[string]string{"n.go": "package lib\n"})
	r := boundRig(t, fakeBindings{"X-2": bound("X-2", w.root, dir)}, w.root)
	ctx := context.Background()

	g, ok := r.groupOf(ctx, "X-2")
	if !ok || !g.single || len(g.Repos) != 1 || g.Repos[0] != "app" || g.Root != w.root || g.worktree("app") != dir || g.Rel() != dir {
		t.Fatalf("%+v", g)
	}
	res, err := r.Merge(ctx, "X-2", wire.MergeRequest{RemoveGroup: true})
	if err != nil || !res.Merged || !res.Removed || res.Error != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the worktree stays", err)
	}
}

func TestParentModeUsesParentGroup(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	w.repo("app")
	g := w.group("p-1", "app")
	w.commit(filepath.Join(g, "app"), "Work\n\nItem: P-2", map[string]string{"n.go": "package lib\n"})
	sub := bound("P-2", w.root, ".wt/p-1")
	sub.Mode, sub.Parent = wire.WorktreeParent, "P-1"
	r := boundRig(t, fakeBindings{"P-1": bound("P-1", w.root, ".wt/p-1"), "P-2": sub}, w.root)
	ctx := context.Background()

	got, ok := r.groupOf(ctx, "P-2")
	if !ok || got.Dir() != g || got.Key() != "P-1" {
		t.Fatalf("%+v", got)
	}
	if page := list(t, r); len(page) != 1 || page[0].Key != "P-1" {
		t.Fatalf("%+v", page)
	}
}

func TestListAddsBoundGroups(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	app := w.repo("app")
	g := w.group("x-1", "app")
	w.commit(filepath.Join(g, "app"), "One", map[string]string{"a.go": "package lib\n"})
	dir := filepath.Join(w.root, "elsewhere", "x-2")
	os.MkdirAll(dir, 0o755)
	w.git(app, "worktree", "add", "-q", "-b", "x-2", filepath.Join(dir, "app"), "main")
	w.commit(filepath.Join(dir, "app"), "Two", map[string]string{"b.go": "package lib\n"})
	r := boundRig(t, fakeBindings{"X-1": bound("X-1", w.root, ".wt/x-1"), "X-2": bound("X-2", w.root, "elsewhere/x-2")}, w.root)

	page := list(t, r)
	got := map[string]string{}
	for _, s := range page {
		got[s.Key] = s.Group
	}
	if len(page) != 2 || got["X-1"] != ".wt/x-1" || got["X-2"] != "elsewhere/x-2" {
		t.Fatalf("%+v", got)
	}
}

func TestReadyRefusalNamesBind(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	w.repo("app")
	r := newRig(t, w.root)
	_, err := r.Ready(context.Background(), "APP-7", wire.ReviewReady{})
	if !kindIs(err, server.NotFound) || !strings.Contains(err.Error(), "oversire item bind APP-7 --worktree here, or make .wt/app-7") {
		t.Fatal(err)
	}
}
