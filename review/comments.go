package review

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/oversire/cli/wire"
)

const (
	MaxText    = 8000
	MaxReply   = 4000
	MaxNote    = 300
	maxExcerpt = 20 // lines
)

var blockID = regexp.MustCompile(`^[a-z0-9-]+$`)

// AddComment adds a pending comment to the batch of key. The kind is d.Kind, else the kind of the anchor.
func (e *Engine) AddComment(ctx context.Context, key string, d wire.CommentDraft) (wire.ReviewComment, error) {
	kind := d.Kind
	if kind == "" {
		kind = wire.ReviewCode
		if d.Anchor.IsPlan() {
			kind = wire.ReviewPlan
		}
	}
	kind, err := e.kindOf(ctx, key, kind)
	if err != nil {
		return wire.ReviewComment{}, err
	}
	if err := checkText(d.Text, MaxText); err != nil {
		return wire.ReviewComment{}, err
	}
	a := d.Anchor
	var excerpt string
	switch kind {
	case wire.ReviewPlan:
		if a.Block == "" || a.Repo != "" || a.Path != "" || a.Line != 0 {
			return wire.ReviewComment{}, refuse(Invalid, "A plan comment needs an anchor with only a block.")
		}
		e.mu.Lock()
		r := e.recs[id(kind, key)]
		var plan *wire.PlanDoc
		if r != nil {
			plan = r.Plan
		}
		if plan == nil {
			e.mu.Unlock()
			return wire.ReviewComment{}, refuse(NotFound, "%s has no plan.", key)
		}
		i := slices.IndexFunc(plan.Blocks, func(b wire.PlanBlock) bool { return b.ID == a.Block })
		if i >= 0 {
			excerpt = plan.Blocks[i].Heading
		}
		e.mu.Unlock()
		if i < 0 {
			return wire.ReviewComment{}, refuse(Invalid, "The plan of %s has no block %s.", key, a.Block)
		}
	default:
		if a.Block != "" || a.Repo == "" || a.Path == "" || a.Line < 1 {
			return wire.ReviewComment{}, refuse(Invalid, "A code comment needs an anchor with a repo, a path and a line.")
		}
		if a.Side != "" && a.Side != "old" && a.Side != "new" {
			return wire.ReviewComment{}, refuse(Invalid, "The side %q is not old or new.", a.Side)
		}
		if a.From > a.Line {
			return wire.ReviewComment{}, refuse(Invalid, "The anchor starts at line %d after its end at line %d.", a.From, a.Line)
		}
		excerpt, err = e.excerpt(ctx, key, a)
		if err != nil {
			return wire.ReviewComment{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rec(kind, key)
	c := wire.ReviewComment{ID: e.commentID(), Key: key, Round: len(r.Rounds) + 1, State: wire.CommentPending, Anchor: a,
		Excerpt: excerpt, Text: d.Text, CreatedAt: e.now()}
	r.Comments = append(r.Comments, c)
	e.changed(r, wire.EventReviewChanged)
	return c, nil
}

// excerpt gives the lines of the anchor a from the diff, or from the file of the worktree when the diff does not have them.
func (e *Engine) excerpt(ctx context.Context, key string, a wire.Anchor) (string, error) {
	from := a.From
	if from == 0 {
		from = a.Line
	}
	from = max(from, a.Line-maxExcerpt+1)
	d, err := e.Diff(ctx, key, a.Repo, a.Path, false)
	if err != nil {
		return "", err
	}
	text := excerpt(d, a.Side, from, a.Line)
	if text == "" && a.Side != "old" {
		if g, ok := findGroup(e.allRoots(ctx), key); ok {
			text = readLines(filepath.Join(g.worktree(a.Repo), a.Path), from, a.Line)
		}
	}
	return text, nil
}

// EditComment changes the text of a pending comment.
func (e *Engine) EditComment(key, cid string, ed wire.CommentEdit) (wire.ReviewComment, error) {
	if err := checkText(ed.Text, MaxText); err != nil {
		return wire.ReviewComment{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, i, err := e.comment(key, cid)
	if err != nil {
		return wire.ReviewComment{}, err
	}
	c := &r.Comments[i]
	if c.State != wire.CommentPending {
		return wire.ReviewComment{}, refuse(Conflict, "The comment %s is %s. Only a pending comment can change.", cid, c.State)
	}
	now := e.now()
	c.Text, c.UpdatedAt = ed.Text, &now
	e.changed(r, wire.EventReviewChanged)
	return comments(r)[i], nil
}

// DeleteComment deletes a pending comment. kind can be "": the id finds the comment.
func (e *Engine) DeleteComment(key, cid string, kind wire.ReviewKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, i, err := e.comment(key, cid)
	if err != nil {
		return err
	}
	if kind != "" && r.Kind != kind {
		return refuse(NotFound, "The %s review of %s has no comment %s.", kind, key, cid)
	}
	if r.Comments[i].State != wire.CommentPending {
		return refuse(Conflict, "The comment %s is %s. Only a pending comment can go.", cid, r.Comments[i].State)
	}
	r.Comments = slices.Delete(r.Comments, i, i+1)
	e.changed(r, wire.EventReviewChanged)
	return nil
}

// Reply keeps the answer of the agent to a submitted comment.
func (e *Engine) Reply(key, cid string, rp wire.CommentReply) (wire.ReviewComment, error) {
	if err := checkText(rp.Text, MaxReply); err != nil {
		return wire.ReviewComment{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, i, err := e.comment(key, cid)
	if err != nil {
		return wire.ReviewComment{}, err
	}
	c := &r.Comments[i]
	if c.State == wire.CommentPending {
		return wire.ReviewComment{}, refuse(Conflict, "The comment %s is not submitted yet.", cid)
	}
	now := e.now()
	c.Reply, c.State, c.AnsweredAt = strings.TrimSpace(rp.Text), wire.CommentAnswered, &now
	e.changed(r, wire.EventReviewChanged)
	return comments(r)[i], nil
}

// comment finds the comment cid of key in the records of both kinds. Call it with e.mu held.
func (e *Engine) comment(key, cid string) (*record, int, error) {
	for _, kind := range []wire.ReviewKind{wire.ReviewCode, wire.ReviewPlan} {
		if r := e.recs[id(kind, key)]; r != nil {
			if i := slices.IndexFunc(r.Comments, func(c wire.ReviewComment) bool { return c.ID == cid }); i >= 0 {
				return r, i, nil
			}
		}
	}
	return nil, 0, refuse(NotFound, "%s has no comment %s.", key, cid)
}

// MarkViewed sets or clears the viewed mark of a file of the code review of key.
// The mark keeps the content hash of the file. Get clears the mark when the file changes.
func (e *Engine) MarkViewed(ctx context.Context, key string, m wire.ViewedMark) error {
	if !validKey(key) {
		return refuse(Invalid, "The key %q is not valid.", key)
	}
	if m.Repo == "" || m.Path == "" {
		return refuse(Invalid, "A viewed mark needs a repo and a path.")
	}
	var print string
	if m.Viewed {
		if g, ok := findGroup(e.allRoots(ctx), key); ok {
			print = g.print(m.Repo, m.Path)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rec(wire.ReviewCode, key)
	f := m.Repo + "/" + m.Path
	delete(r.Stale, f)
	kind, summary := wire.KindReviewUnviewed, "Not viewed: "+f+"."
	if m.Viewed {
		r.Viewed = setKey(r.Viewed, f, true)
		r.Prints = setKey(r.Prints, f, print)
		kind, summary = wire.KindReviewViewed, "Viewed "+f+"."
	} else {
		delete(r.Viewed, f)
		delete(r.Prints, f)
	}
	e.changed(r, wire.EventReviewChanged)
	e.viewedEvent(key, kind, wire.ActorHuman, f, summary)
	return nil
}

// unview clears the marks of the files that changed after the mark, and keeps a hash for an old mark with none.
// prints holds the hash of each viewed file now. Call it with e.mu held.
func (e *Engine) unview(r *record, prints map[string]string) {
	dirty := false
	for f, p := range prints {
		switch old, ok := r.Prints[f]; {
		case !ok || old == "":
			r.Prints = setKey(r.Prints, f, p)
		case old != p:
			delete(r.Viewed, f)
			delete(r.Prints, f)
			r.Stale = setKey(r.Stale, f, true)
			e.viewedEvent(r.Key, wire.KindReviewUnviewed, wire.ActorSystem, f, f+" changed after it was viewed.")
		default:
			continue
		}
		dirty = true
	}
	if dirty {
		e.changed(r, wire.EventReviewChanged)
	}
}

func (e *Engine) viewedEvent(key, kind string, actor wire.ActorType, f, summary string) {
	repo, path, _ := strings.Cut(f, "/")
	e.event(key, fmt.Sprintf("review:code:%s:%s:%s:%d", key, kind, f, e.d.Clock().UnixMilli()), kind, actor, summary,
		map[string]any{"kind": wire.ReviewCode, "repo": repo, "path": path})
}

func setKey[V any](m map[string]V, k string, v V) map[string]V {
	if m == nil {
		m = map[string]V{}
	}
	m[k] = v
	return m
}

// Ready is the hand-off of the agent: the review is the turn of the user again. It gives the summary.
func (e *Engine) Ready(ctx context.Context, key string, rd wire.ReviewReady) (wire.ReviewSummary, error) {
	if len([]rune(rd.Note)) > MaxNote {
		return wire.ReviewSummary{}, refuse(Invalid, "The note has more than %d characters.", MaxNote)
	}
	if rd.Root != "" && !filepath.IsAbs(rd.Root) {
		return wire.ReviewSummary{}, refuse(Invalid, "The root %q is not an absolute path.", rd.Root)
	}
	if rd.Root != "" && rd.Kind != wire.ReviewPlan {
		e.mu.Lock()
		e.addRoot(rd.Root)
		e.mu.Unlock()
	}
	kind, err := e.kindOf(ctx, key, rd.Kind)
	if err != nil {
		return wire.ReviewSummary{}, err
	}
	var g group
	if kind == wire.ReviewCode {
		var ok bool
		if g, ok = findGroup(e.allRoots(ctx), key); !ok {
			return wire.ReviewSummary{}, refuse(NotFound, "There is no group .wt/%s in the roots of this daemon. Give the root.", strings.ToLower(key))
		}
	} else {
		e.mu.Lock()
		r := e.recs[id(kind, key)]
		none := r == nil || r.Plan == nil
		e.mu.Unlock()
		if none {
			return wire.ReviewSummary{}, refuse(NotFound, "%s has no plan. Store it first: oversire review ready --kind plan --plan <file.md>.", key)
		}
	}
	e.mu.Lock()
	r := e.rec(kind, key)
	now := e.now()
	r.Note, r.ReadyAt, r.Conflict, r.Approved = strings.TrimSpace(rd.Note), &now, false, false
	if kind == wire.ReviewCode {
		r.Root, r.MergedAt = g.Root, nil
	}
	if n := len(r.Rounds); n > 0 && r.Rounds[n-1].AnsweredAt == nil {
		r.Rounds[n-1].AnsweredAt = &now
	}
	// The hand-off ends the turn: a queued round goes when the book shows it.
	delete(e.sent, id(kind, key))
	e.changed(r, wire.EventReviewReady)
	round := len(r.Rounds)
	e.mu.Unlock()
	summary := "Ready for review"
	if kind == wire.ReviewPlan {
		summary = "Plan ready for review"
	}
	if note := strings.TrimSpace(rd.Note); note != "" {
		summary += ": " + note
	}
	e.event(key, fmt.Sprintf("review:%s:%s:ready:%d", kind, key, now.UnixMilli()), wire.KindReviewReady, wire.ActorAgent, summary,
		map[string]any{"kind": kind, "round": round})
	if kind == wire.ReviewPlan {
		return e.planSummary(key), nil
	}
	s, _, err := e.codeSummary(ctx, g)
	return s, err
}

// PutPlan keeps the plan of a Plan session. Each new plan of the key has the next version.
func (e *Engine) PutPlan(key string, p wire.PlanDoc) (wire.PlanDoc, error) {
	if !validKey(key) {
		return p, refuse(Invalid, "The key %q is not valid.", key)
	}
	if p.Key != "" && p.Key != key {
		return p, refuse(Invalid, "The plan is for %s, not %s.", p.Key, key)
	}
	if !filepath.IsAbs(p.Root) {
		return p, refuse(Invalid, "The plan needs the folder of the Plan session as an absolute root.")
	}
	if len(p.Blocks) == 0 {
		return p, refuse(Invalid, "The plan has no blocks.")
	}
	seen := map[string]bool{}
	for _, b := range p.Blocks {
		if !blockID.MatchString(b.ID) || seen[b.ID] {
			return p, refuse(Invalid, "The block id %q is not valid or not unique. Use a-z, 0-9 and -.", b.ID)
		}
		seen[b.ID] = true
	}
	for _, s := range p.Subitems {
		if s.Title == "" {
			return p, refuse(Invalid, "Each sub-item needs a title.")
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rec(wire.ReviewPlan, key)
	p.Key, p.Version, p.Root = key, 1, filepath.Clean(p.Root)
	if r.Plan != nil {
		p.Version = r.Plan.Version + 1
	}
	plan := clone(p)
	r.Plan, r.Root, r.Approved = &plan, p.Root, false
	e.changed(r, wire.EventReviewChanged)
	return p, nil
}

func checkText(text string, n int) error {
	switch {
	case strings.TrimSpace(text) == "":
		return refuse(Invalid, "The text is empty.")
	case len([]rune(text)) > n:
		return refuse(Invalid, "The text has more than %d characters.", n)
	}
	return nil
}

// commentID is c_ and 8 hex digits, unique in the records. Call it with e.mu held.
func (e *Engine) commentID() string {
	for {
		cid := "c_" + randomHex(4)
		taken := false
		for _, r := range e.recs {
			taken = taken || slices.ContainsFunc(r.Comments, func(c wire.ReviewComment) bool { return c.ID == cid })
		}
		if !taken {
			return cid
		}
	}
}
