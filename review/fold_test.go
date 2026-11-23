package review

import (
	"context"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func child(key, parent, root, group string) wire.WorktreeBinding {
	b := bound(key, root, group)
	b.Parent = parent
	return b
}

// foldWS gives a root with the repo app and the groups of the keys, each with one commit on its own file.
func foldWS(t *testing.T, keys ...string) *ws {
	t.Helper()
	w := newWS(t)
	w.repo("app")
	for _, k := range keys {
		g := w.group(k, "app")
		w.commit(filepath.Join(g, "app"), "Work on "+k, map[string]string{k + ".go": "package lib\n"})
	}
	return w
}

// family binds 401 and 402 to the parent 400.
func family(root string) fakeBindings {
	return fakeBindings{
		"OVERSIRE-400": bound("OVERSIRE-400", root, ".wt/oversire-400"),
		"OVERSIRE-401": child("OVERSIRE-401", "OVERSIRE-400", root, ".wt/oversire-401"),
		"OVERSIRE-402": child("OVERSIRE-402", "OVERSIRE-400", root, ".wt/oversire-402"),
	}
}

func foldList(t *testing.T, r *rig, f Fold) []wire.ReviewSummary {
	t.Helper()
	page, err := r.List(context.Background(), wire.ReviewCode, f)
	if err != nil {
		t.Fatal(err)
	}
	return page.Reviews
}

func partKeys(s wire.ReviewSummary) []string {
	var out []string
	for _, p := range s.Parts {
		out = append(out, p.Key)
	}
	return out
}

func TestFoldFolds(t *testing.T) {
	f := Fold{On: true, Except: []string{"SLING"}}
	if !f.Folds("OVERSIRE-401") || f.Folds("sling-3") || f.Folds("NOKEY") || (Fold{}).Folds("OVERSIRE-401") {
		t.Fatal("wrong fold")
	}
}

func TestFoldQuery(t *testing.T) {
	q, _ := url.ParseQuery("fold=1&nofold=sling,os")
	if got := foldOf(q); !reflect.DeepEqual(got, Fold{On: true, Except: []string{"SLING", "OS"}}) {
		t.Fatalf("%+v", got)
	}
	if got := foldOf(url.Values{}); got.On {
		t.Fatalf("%+v", got)
	}
}

func TestKeyCompareIsNatural(t *testing.T) {
	keys := []string{"X-10", "X-9", "A-2"}
	slices.SortFunc(keys, keyCompare)
	if !slices.Equal(keys, []string{"A-2", "X-9", "X-10"}) {
		t.Fatal(keys)
	}
}

func TestListFoldsChildren(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)

	rows := foldList(t, r, Fold{On: true})
	if len(rows) != 1 || rows[0].Key != "OVERSIRE-400" {
		t.Fatalf("%+v", rows)
	}
	if got := partKeys(rows[0]); !slices.Equal(got, []string{"OVERSIRE-401", "OVERSIRE-402", "OVERSIRE-400"}) {
		t.Fatal(got)
	}
	var items []string
	for _, repo := range rows[0].Repos {
		items = append(items, repo.Item)
	}
	if !slices.Equal(items, []string{"OVERSIRE-401", "OVERSIRE-402", "OVERSIRE-400"}) || !slices.Equal(rows[0].Subitems, []string{"OVERSIRE-401", "OVERSIRE-402"}) {
		t.Fatalf("%v %v", items, rows[0].Subitems)
	}
	if rows := foldList(t, r, Fold{}); len(rows) != 3 || rows[0].Parts != nil {
		t.Fatalf("%+v", rows)
	}
}

func TestListFoldNoParentGroup(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	rows := foldList(t, r, Fold{On: true})
	if len(rows) != 1 || rows[0].Key != "OVERSIRE-400" || rows[0].Group != ".wt/oversire-400" && rows[0].Group != "" {
		t.Fatalf("%+v", rows)
	}
	if got := partKeys(rows[0]); !slices.Equal(got, []string{"OVERSIRE-401", "OVERSIRE-402"}) {
		t.Fatal(got)
	}
}

func TestListFoldSharedGroup(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400")
	bs := family(w.root)
	shared := child("OVERSIRE-401", "OVERSIRE-400", w.root, ".wt/oversire-400")
	shared.Mode = wire.WorktreeParent
	bs["OVERSIRE-401"] = shared
	delete(bs, "OVERSIRE-402")
	r := boundRig(t, bs, w.root)
	rows := foldList(t, r, Fold{On: true})
	if len(rows) != 1 || rows[0].Key != "OVERSIRE-400" || rows[0].Parts != nil {
		t.Fatalf("%+v", rows)
	}
}

func TestListFoldExcept(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	if rows := foldList(t, r, Fold{On: true, Except: []string{"OVERSIRE"}}); len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
}

func TestListFoldNilBindings(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := newRig(t, w.root)
	if rows := foldList(t, r, Fold{On: true}); len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
}

func TestListFoldOtherMachine(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401")
	bs := family(w.root)
	other := bs["OVERSIRE-401"]
	other.Machine = "mini"
	bs["OVERSIRE-401"] = other
	r := boundRig(t, bs, w.root)
	if rows := foldList(t, r, Fold{On: true}); len(rows) != 2 {
		t.Fatalf("%+v", rows)
	}
}

func TestFoldedSummaryState(t *testing.T) {
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	work := []wire.ReviewRepo{{Name: "app", Ahead: 1}}
	row := func(key string, st wire.ReviewState, round, pending int) wire.ReviewSummary {
		return wire.ReviewSummary{Key: key, State: st, Round: round, Pending: pending, Root: "/w", Repos: work, UpdatedAt: at}
	}
	fd := &folded{parent: "P-1", parts: []wire.ReviewSummary{row("P-2", wire.ReviewConflict, 1, 2), row("P-3", wire.ReviewReadyState, 3, 1)}}
	s := fd.summary("mbp")
	if s.State != wire.ReviewConflict || s.Pending != 3 || s.Round != 3 || s.Key != "P-1" {
		t.Fatalf("%+v", s)
	}
	fd.parts[0].State = wire.ReviewWaiting
	if s := fd.summary("mbp"); s.State != wire.ReviewReadyState {
		t.Fatal(s.State)
	}
	fd.parts[0].State, fd.parts[1].State = wire.ReviewMerged, wire.ReviewMerged
	if s := fd.summary("mbp"); s.State != wire.ReviewMerged {
		t.Fatal(s.State)
	}
}

func TestGetFolded(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	ctx := context.Background()
	if _, err := r.AddComment(ctx, "OVERSIRE-401", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "oversire-401.go", Line: 1}, Text: "Why?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, "OVERSIRE-402", wire.SubmitReview{Verdict: wire.VerdictApprove}); err != nil {
		t.Fatal(err)
	}

	d, err := r.Get(ctx, "OVERSIRE-400", wire.ReviewCode, Fold{On: true})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range d.Files {
		files = append(files, f.Item+":"+f.Path)
	}
	if !slices.Equal(files, []string{"OVERSIRE-401:oversire-401.go", "OVERSIRE-402:oversire-402.go", "OVERSIRE-400:oversire-400.go"}) {
		t.Fatal(files)
	}
	if len(d.Comments) != 1 || d.Comments[0].Key != "OVERSIRE-401" {
		t.Fatalf("%+v", d.Comments)
	}
	if len(d.Rounds) != 1 || d.Rounds[0].Item != "OVERSIRE-402" || len(d.Summary.Parts) != 3 {
		t.Fatalf("%+v", d.Rounds)
	}
	own, err := r.Get(ctx, "OVERSIRE-401", wire.ReviewCode, Fold{On: true})
	if err != nil || own.Summary.Key != "OVERSIRE-401" || own.Summary.Parts != nil || len(own.Files) != 1 {
		t.Fatalf("%+v %v", own.Summary, err)
	}
	plain, err := r.Get(ctx, "OVERSIRE-400", wire.ReviewCode, Fold{})
	if err != nil || len(plain.Files) != 1 || plain.Files[0].Item != "" {
		t.Fatalf("%+v %v", plain.Files, err)
	}
}

func TestEventParentItem(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401")
	r := boundRig(t, family(w.root), w.root)
	ctx := context.Background()
	if _, err := r.Ready(ctx, "OVERSIRE-401", wire.ReviewReady{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Ready(ctx, "OVERSIRE-400", wire.ReviewReady{}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	got := map[string]string{}
	for _, d := range r.rows {
		got[d.Item] = d.ParentItem
	}
	if got["OVERSIRE-401"] != "OVERSIRE-400" || got["OVERSIRE-400"] != "" {
		t.Fatalf("%+v", got)
	}
}

func TestRoutesFold(t *testing.T) {
	t.Parallel()
	w := foldWS(t, "oversire-400", "oversire-401", "oversire-402")
	r := boundRig(t, family(w.root), w.root)
	loop, _ := serve(t, r.Engine)
	code, body := send(t, "GET", loop+"/v1/reviews?kind=code&fold=1", "")
	if page := decodeAs[wire.ReviewsPage](t, body); code != 200 || len(page.Reviews) != 1 || len(page.Reviews[0].Parts) != 3 {
		t.Fatal(code, body)
	}
	_, body = send(t, "GET", loop+"/v1/reviews?kind=code", "")
	if page := decodeAs[wire.ReviewsPage](t, body); len(page.Reviews) != 3 {
		t.Fatal(body)
	}
	code, body = send(t, "GET", loop+"/v1/reviews/OVERSIRE-400?fold=1", "")
	if d := decodeAs[wire.ReviewDetail](t, body); code != 200 || len(d.Files) != 3 {
		t.Fatal(code, body)
	}
}
