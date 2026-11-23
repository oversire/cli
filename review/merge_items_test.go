package review

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func itemsOf(res wire.MergeResult) []string {
	var out []string
	for _, r := range res.Repos {
		out = append(out, r.Item+":"+string(r.Result))
	}
	return out
}

func TestMergeItemsOrder(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	res, err := r.Merge(context.Background(), "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-400", "OVERSIRE-402", "OVERSIRE-401"}})
	if err != nil || !res.Merged || res.Error != "" {
		t.Fatalf("%v %+v", err, res)
	}
	want := []string{"OVERSIRE-401:merged", "OVERSIRE-402:rebased", "OVERSIRE-400:rebased"}
	if got := itemsOf(res); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	log := strings.Split(w.git(filepath.Join(w.root, "app"), "log", "--format=%s", "-3", "main"), "\n")
	if !slices.Equal(log, []string{"Work on oversire-400", "Work on oversire-402", "Work on oversire-401"}) {
		t.Fatal(log)
	}
	for _, k := range []string{"OVERSIRE-400", "OVERSIRE-401", "OVERSIRE-402"} {
		if r.mergedAt(k) == nil {
			t.Fatal(k, "not merged")
		}
	}
}

func TestMergeItemsRebaseAfterSibling(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401")
	r := boundRig(t, family(w.root), w.root)
	res, err := r.Merge(context.Background(), "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-400", "OVERSIRE-401"}})
	if err != nil || !res.Merged || res.Repos[1].Result != wire.MergeRebased {
		t.Fatalf("%v %+v", err, res)
	}
	app := filepath.Join(w.root, "app")
	if w.git(app, "rev-parse", "main") != w.git(filepath.Join(w.root, ".wt", "oversire-400", "app"), "rev-parse", "HEAD") {
		t.Fatal("the parent is not on the new base")
	}
}

func TestMergeItemsAllOrNone(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	app := w.repo("app")
	for _, k := range []string{"oversire-400", "oversire-401", "oversire-402"} {
		g := w.group(k, "app")
		w.commit(filepath.Join(g, "app"), "Work on "+k, map[string]string{"lib.go": "package lib\n\n// " + k + "\n"})
	}
	old := w.git(app, "rev-parse", "main")
	r := boundRig(t, family(w.root), w.root)
	ctx := context.Background()
	res, err := r.Merge(ctx, "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-401", "OVERSIRE-402", "OVERSIRE-400"}, RemoveGroup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged || res.Removed || !strings.HasPrefix(res.Error, "OVERSIRE-402: app: ") {
		t.Fatalf("%+v", res)
	}
	if got := itemsOf(res); !slices.Equal(got, []string{"OVERSIRE-401:skipped", "OVERSIRE-402:conflict", "OVERSIRE-400:skipped"}) {
		t.Fatal(got)
	}
	if w.git(app, "rev-parse", "main") != old {
		t.Fatal("the base did not go back")
	}
	d, err := r.Get(ctx, "OVERSIRE-402", "", Fold{})
	if err != nil || d.Summary.State != wire.ReviewConflict {
		t.Fatal(err, d.Summary.State)
	}
	for _, k := range []string{"OVERSIRE-400", "OVERSIRE-401", "OVERSIRE-402"} {
		if r.mergedAt(k) != nil {
			t.Fatal(k, "merged")
		}
	}
}

func TestMergeItemsCheckFirst(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401")
	app := filepath.Join(w.root, "app")
	old := w.git(app, "rev-parse", "main")
	r := boundRig(t, family(w.root), w.root)
	r.book.put(session("s1", wire.PhaseWorking, r.clock.now(), filepath.Join(w.root, ".wt", "oversire-401", "app")))
	_, err := r.Merge(context.Background(), "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-400", "OVERSIRE-401"}})
	if !kindIs(err, server.Conflict) || !strings.HasPrefix(err.Error(), "OVERSIRE-401: ") {
		t.Fatal(err)
	}
	if w.git(app, "rev-parse", "main") != old {
		t.Fatal("merged after a failed check")
	}
}

func TestMergeItemsBadItem(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401")
	bs := family(w.root)
	bs["OVERSIRE-403"] = child("OVERSIRE-403", "OVERSIRE-400", w.root, ".wt/oversire-403")
	r := boundRig(t, bs, w.root)
	ctx := context.Background()
	if _, err := r.Merge(ctx, "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-400", "OVERSIRE-9"}}); !kindIs(err, server.Invalid) ||
		err.Error() != "OVERSIRE-9 is not a sub-item of OVERSIRE-400." {
		t.Fatal(err)
	}
	if _, err := r.Merge(ctx, "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-403"}}); !kindIs(err, server.NotFound) {
		t.Fatal(err)
	}
}

func TestMergeItemsEvents(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	if _, err := r.Merge(context.Background(), "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-400", "OVERSIRE-401", "OVERSIRE-402"}}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	shipped := map[string]string{}
	merges := map[string]bool{}
	for _, d := range r.rows {
		switch d.Kind {
		case wire.KindItemShipped:
			shipped[d.Item] = d.ParentItem
		case wire.KindGitMerge:
			merges[d.ID] = true
		}
	}
	if len(shipped) != 3 || shipped["OVERSIRE-401"] != "OVERSIRE-400" || shipped["OVERSIRE-402"] != "OVERSIRE-400" || shipped["OVERSIRE-400"] != "" {
		t.Fatalf("%+v", shipped)
	}
	if len(merges) != 3 {
		t.Fatalf("%+v", merges)
	}
}

func TestMergeItemsSharedRemove(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400")
	bs := family(w.root)
	shared := child("OVERSIRE-401", "OVERSIRE-400", w.root, ".wt/oversire-400")
	shared.Mode = wire.WorktreeParent
	bs["OVERSIRE-401"] = shared
	r := boundRig(t, bs, w.root)
	res, err := r.Merge(context.Background(), "OVERSIRE-400", wire.MergeRequest{Items: []string{"OVERSIRE-401", "OVERSIRE-400"}, RemoveGroup: true})
	if err != nil || !res.Merged || !res.Removed || res.Error != "" || len(res.Repos) != 1 || res.Repos[0].Item != "OVERSIRE-400" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(w.root, ".wt", "oversire-400")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestHeadsRestore(t *testing.T) {
	t.Parallel()
	w := newWS(t)
	app := w.repo("app")
	a := w.git(app, "rev-parse", "HEAD")
	b := w.commit(app, "B", map[string]string{"b.go": "package lib\n"})
	w.commit(app, "C", map[string]string{"c.go": "package lib\n"})
	var hs heads
	hs.keep(app, a)
	hs.keep(app, b)
	r := newRig(t, w.root)
	if failed := hs.restore(context.Background(), r.d.Git); len(failed) != 0 {
		t.Fatal(failed)
	}
	if w.git(app, "rev-parse", "HEAD") != a {
		t.Fatal("not back at the first head")
	}
}
