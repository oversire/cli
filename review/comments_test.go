package review

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// codeRig is a group app-1 with one commit that adds n.go, and a rig on its root.
func codeRig(t *testing.T) (*ws, *rig, string) {
	t.Helper()
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n\nfunc N() {}\n\nfunc M() {}\n", "lib.go": "package lib\n\nfunc A() { N() }\n"})
	return w, newRig(t, w.root), g
}

func TestCommentRules(t *testing.T) {
	_, r, _ := codeRig(t)
	ctx := context.Background()
	for name, d := range map[string]wire.CommentDraft{
		"no repo":     {Anchor: wire.Anchor{Path: "n.go", Line: 1}, Text: "x"},
		"no line":     {Anchor: wire.Anchor{Repo: "app", Path: "n.go"}, Text: "x"},
		"bad side":    {Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1, Side: "left"}, Text: "x"},
		"from > line": {Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1, From: 3}, Text: "x"},
		"no text":     {Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1}, Text: "  "},
		"long text":   {Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1}, Text: strings.Repeat("x", MaxText+1)},
	} {
		if _, err := r.AddComment(ctx, "APP-1", d); !kindIs(err, Invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "x.go", Line: 1}, Text: "x"}); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
	if _, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Block: "goal"}, Text: "x"}); !kindIs(err, NotFound) {
		t.Fatal(err)
	}

	c, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 5, From: 3}, Text: "Join them."})
	if err != nil || c.Excerpt != "func N() {}\n\nfunc M() {}" || c.State != wire.CommentPending {
		t.Fatalf("%v %+v", err, c)
	}
	if c, err = r.EditComment("APP-1", c.ID, wire.CommentEdit{Text: "Join N and M."}); err != nil || c.Text != "Join N and M." || c.UpdatedAt == nil {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := r.Reply("APP-1", c.ID, wire.CommentReply{Text: "Done."}); !kindIs(err, Conflict) {
		t.Fatal(err)
	}
	gone, _ := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1}, Text: "Drop."})
	if err := r.DeleteComment("APP-1", gone.ID, wire.ReviewPlan); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
	if err := r.DeleteComment("APP-1", gone.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteComment("APP-1", gone.ID, ""); !kindIs(err, NotFound) {
		t.Fatal(err)
	}

	if _, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictApprove}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EditComment("APP-1", c.ID, wire.CommentEdit{Text: "Later."}); !kindIs(err, Conflict) {
		t.Fatal(err)
	}
	if err := r.DeleteComment("APP-1", c.ID, ""); !kindIs(err, Conflict) {
		t.Fatal(err)
	}
	if s := list(t, r)[0]; s.Unanswered != 1 || s.Pending != 0 || s.Round != 1 {
		t.Fatalf("%+v", s)
	}
	a, err := r.Reply("APP-1", c.ID, wire.CommentReply{Text: "Joined."})
	if err != nil || a.State != wire.CommentAnswered || a.Reply != "Joined." || a.AnsweredAt == nil || a.Round != 1 {
		t.Fatalf("%v %+v", err, a)
	}
	if s := list(t, r)[0]; s.Unanswered != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestReadyConflict(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Mine", map[string]string{"lib.go": "package lib\n\nfunc Mine() {}\n"})
	w.commit(app, "Theirs", map[string]string{"lib.go": "package lib\n\nfunc Theirs() {}\n"})
	r := newRig(t, w.root)
	if s := list(t, r)[0]; s.State != wire.ReviewReadyState {
		t.Fatal(s.State)
	}
	s, err := r.Ready(context.Background(), "app-1", wire.ReviewReady{})
	if !kindIs(err, Invalid) {
		t.Fatal(err, s)
	}
	if s, err = r.Ready(context.Background(), "APP-1", wire.ReviewReady{Note: "Done."}); err != nil || s.State != wire.ReviewConflict {
		t.Fatal(err, s.State)
	}
	if s := list(t, r)[0]; s.State != wire.ReviewConflict || s.Note != "Done." || s.ReadyAt == nil {
		t.Fatalf("%+v", s)
	}
	if !slices.Equal(r.eventNames(), []string{wire.EventReviewReady}) {
		t.Fatal(r.eventNames())
	}
	if _, err := r.Ready(context.Background(), "APP-2", wire.ReviewReady{}); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
}

func TestPlan(t *testing.T) {
	r := newRig(t)
	root := t.TempDir()
	ctx := context.Background()
	p := wire.PlanDoc{Key: "APP-1", Version: 1, Title: "Plan", Root: root, Blocks: []wire.PlanBlock{
		{ID: "goal", Heading: "Goal", Markdown: "Ship it."},
		{ID: "steps", Heading: "Steps", Markdown: "1. Do."},
	}, Subitems: []wire.PlanSubitem{{Key: "APP-2", Title: "Store"}, {Title: "Routes"}}}
	for name, bad := range map[string]wire.PlanDoc{
		"relative root": {Root: "plans", Blocks: p.Blocks},
		"no blocks":     {Root: root},
		"bad id":        {Root: root, Blocks: []wire.PlanBlock{{ID: "Goal"}}},
		"same id":       {Root: root, Blocks: []wire.PlanBlock{{ID: "a"}, {ID: "a"}}},
		"other key":     {Key: "APP-9", Root: root, Blocks: p.Blocks},
	} {
		if _, err := r.PutPlan("APP-1", bad); !kindIs(err, Invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.Ready(ctx, "APP-1", wire.ReviewReady{Kind: wire.ReviewPlan}); !kindIs(err, NotFound) {
		t.Fatal(err)
	}
	got, err := r.PutPlan("APP-1", p)
	if err != nil || got.Version != 1 {
		t.Fatal(err, got)
	}
	if got, _ = r.PutPlan("APP-1", p); got.Version != 2 {
		t.Fatal(got.Version)
	}
	s, err := r.Ready(ctx, "APP-1", wire.ReviewReady{Kind: wire.ReviewPlan, Note: "Two blocks."})
	if err != nil || s.Kind != wire.ReviewPlan || s.Title != "Plan" || s.Root != root || !slices.Equal(s.Subitems, []string{"APP-2"}) {
		t.Fatalf("%v %+v", err, s)
	}
	if row, ok := r.row(wire.KindReviewReady); !ok || row.Summary != "Plan ready for review: Two blocks." || row.ActorType != wire.ActorAgent || row.Item != "APP-1" {
		t.Fatalf("%+v", row)
	}
	// A key with only a plan is a plan review without the kind.
	c, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Block: "steps"}, Text: "Add tests."})
	if err != nil || c.Excerpt != "Steps" {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Block: "none"}, Text: "x"}); !kindIs(err, Invalid) {
		t.Fatal(err)
	}
	d, err := r.Get(ctx, "APP-1", "")
	if err != nil || d.Plan == nil || d.Plan.Version != 2 || len(d.Comments) != 1 || d.Summary.Pending != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	page, _ := r.List(ctx, wire.ReviewPlan)
	if len(page.Reviews) != 1 {
		t.Fatal(page.Reviews)
	}
	// An approved plan leaves the list. A new plan brings it back.
	if _, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Kind: wire.ReviewPlan, Verdict: wire.VerdictApprove}); err != nil {
		t.Fatal(err)
	}
	if page, _ = r.List(ctx, ""); len(page.Reviews) != 0 {
		t.Fatal(page.Reviews)
	}
	r.clock.add(time.Minute)
	r.PutPlan("APP-1", p)
	if page, _ = r.List(ctx, ""); len(page.Reviews) != 1 {
		t.Fatal(page.Reviews)
	}
}

func TestStoreKeepsReviews(t *testing.T) {
	w := newWS(t)
	w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Work", map[string]string{"n.go": "package lib\n\nfunc N() {}\n"})
	other := t.TempDir()
	r := newRig(t)
	ctx := context.Background()
	// The root comes only from the hand-off.
	if _, err := r.Ready(ctx, "APP-1", wire.ReviewReady{Root: w.root, Note: "One."}); err != nil {
		t.Fatal(err)
	}
	c, err := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "Name it."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ID, "c_") || len(c.ID) != 10 || c.Excerpt != "func N() {}" || c.Round != 1 {
		t.Fatalf("%+v", c)
	}
	if err := r.MarkViewed("APP-1", wire.ViewedMark{Repo: "app", Path: "n.go", Viewed: true}); err != nil {
		t.Fatal(err)
	}

	r2 := r.engine(other)
	d, err := r2.Get(ctx, "APP-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != 1 || d.Comments[0].ID != c.ID || !d.Files[0].Viewed || d.Summary.Note != "One." {
		t.Fatalf("%+v", d)
	}
	if roots := r2.allRoots(ctx); !slices.Equal(roots, slices.Sorted(slices.Values([]string{other, w.root}))) {
		t.Fatal(roots)
	}
}
