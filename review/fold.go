package review

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Fold asks a code read to put the groups of the sub-items into the row of their parent (OVERSIRE-590).
// A sub-item folds when its binding on this machine names a parent and has a group of its own.
type Fold struct {
	On bool
	// Except are the key prefixes (projects) that do not fold, in upper case.
	Except []string
}

// Folds tells if the group of key can fold into its parent.
func (f Fold) Folds(key string) bool {
	i := strings.LastIndex(key, "-")
	if !f.On || i <= 0 {
		return false
	}
	return !slices.ContainsFunc(f.Except, func(p string) bool { return strings.EqualFold(p, key[:i]) })
}

// foldOf reads ?fold=1&nofold=A,B. Any other fold value is no fold.
func foldOf(q url.Values) Fold {
	f := Fold{On: q.Get("fold") == "1"}
	for _, p := range strings.Split(q.Get("nofold"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			f.Except = append(f.Except, strings.ToUpper(p))
		}
	}
	return f
}

// keyCompare orders keys by project, then by number: X-9 before X-10.
func keyCompare(a, b string) int {
	ap, an, _ := strings.Cut(a, "-")
	bp, bn, _ := strings.Cut(b, "-")
	if c := strings.Compare(ap, bp); c != 0 {
		return c
	}
	ai, aerr := strconv.Atoi(an)
	bi, berr := strconv.Atoi(bn)
	if aerr == nil && berr == nil && ai != bi {
		return cmp.Compare(ai, bi)
	}
	return strings.Compare(a, b)
}

// folded is a parent row and the summaries of its groups: the sub-items in key order, then the parent.
type folded struct {
	parent string
	parts  []wire.ReviewSummary
	merged map[string]*time.Time
}

func (fd *folded) sort() {
	slices.SortStableFunc(fd.parts, func(a, b wire.ReviewSummary) int {
		switch {
		case a.Key == fd.parent:
			return 1
		case b.Key == fd.parent:
			return -1
		}
		return keyCompare(a.Key, b.Key)
	})
}

// listed is true when one part has work to review.
func (fd *folded) listed() bool { return slices.ContainsFunc(fd.parts, listed) }

// summary is the parent row. It starts from the row of the parent group, or an empty row when the parent has none.
// The repos carry the key of their part. The counts are sums, the round and the times are the newest.
func (fd *folded) summary(machine string) wire.ReviewSummary {
	s := wire.ReviewSummary{Key: fd.parent, Kind: wire.ReviewCode, Machine: machine, Root: fd.parts[0].Root}
	if own := fd.parts[len(fd.parts)-1]; own.Key == fd.parent {
		s = own
	}
	s.Repos, s.Subitems, s.Parts = []wire.ReviewRepo{}, []string{}, []wire.ReviewPart{}
	s.Pending, s.Unanswered = 0, 0
	for _, p := range fd.parts {
		for _, r := range p.Repos {
			r.Item = p.Key
			s.Repos = append(s.Repos, r)
		}
		for _, k := range append([]string{p.Key}, p.Subitems...) {
			if k != fd.parent && !slices.Contains(s.Subitems, k) {
				s.Subitems = append(s.Subitems, k)
			}
		}
		s.Pending += p.Pending
		s.Unanswered += p.Unanswered
		s.Round = max(s.Round, p.Round)
		if p.UpdatedAt.After(s.UpdatedAt) {
			s.UpdatedAt = p.UpdatedAt
		}
		if p.ReadyAt != nil && (s.ReadyAt == nil || p.ReadyAt.After(*s.ReadyAt)) {
			s.ReadyAt = p.ReadyAt
		}
		s.Parts = append(s.Parts, wire.ReviewPart{Key: p.Key, Root: p.Root, Group: p.Group, State: p.State, Round: p.Round,
			Pending: p.Pending, Unanswered: p.Unanswered, Session: p.Session, Note: p.Note, ReadyAt: p.ReadyAt, MergedAt: fd.merged[p.Key]})
	}
	slices.SortFunc(s.Subitems, keyCompare)
	s.State = fd.state()
	return s
}

// state: a conflict first, then a part that is ready with work, then a part that waits. Merged when all parts are.
func (fd *folded) state() wire.ReviewState {
	has := func(st wire.ReviewState, work bool) bool {
		return slices.ContainsFunc(fd.parts, func(p wire.ReviewSummary) bool { return p.State == st && (!work || listed(p)) })
	}
	switch {
	case has(wire.ReviewConflict, false):
		return wire.ReviewConflict
	case has(wire.ReviewReadyState, true):
		return wire.ReviewReadyState
	case has(wire.ReviewWaiting, false):
		return wire.ReviewWaiting
	case !slices.ContainsFunc(fd.parts, func(p wire.ReviewSummary) bool { return p.State != wire.ReviewMerged }):
		return wire.ReviewMerged
	}
	return wire.ReviewReadyState
}

// directParent gives the parent of key when the group of key folds, else "". A key in parent mode has no group of
// its own, so it does not fold.
func (e *Engine) directParent(key string, f Fold) string {
	if !f.Folds(key) {
		return ""
	}
	b, ok := e.bound(key)
	if !ok || b.Parent == "" || strings.EqualFold(b.Parent, key) || b.Mode == wire.WorktreeParent {
		return ""
	}
	return strings.ToUpper(b.Parent)
}

// parentOf gives the row that the group of key folds into. Only one level folds: when the parent folds too, key
// keeps its own row.
func (e *Engine) parentOf(key string, f Fold) string {
	p := e.directParent(key, f)
	if p == "" || e.directParent(p, f) != "" {
		return ""
	}
	return p
}

// foldRows puts the rows of the sub-items into the row of their parent. The other rows show when they are listed.
func (e *Engine) foldRows(rows []wire.ReviewSummary, f Fold) []wire.ReviewSummary {
	byParent := map[string]*folded{}
	var parents []string
	for _, s := range rows {
		if p := e.parentOf(s.Key, f); p != "" {
			if byParent[p] == nil {
				byParent[p] = &folded{parent: p, merged: map[string]*time.Time{}}
				parents = append(parents, p)
			}
			byParent[p].parts = append(byParent[p].parts, s)
		}
	}
	out := []wire.ReviewSummary{}
	for _, s := range rows {
		switch {
		case byParent[s.Key] != nil:
			byParent[s.Key].parts = append(byParent[s.Key].parts, s)
		case e.parentOf(s.Key, f) != "":
		case listed(s):
			out = append(out, s)
		}
	}
	for _, p := range parents {
		fd := byParent[p]
		if !fd.listed() {
			continue
		}
		fd.sort()
		for _, s := range fd.parts {
			fd.merged[s.Key] = e.mergedAt(s.Key)
		}
		out = append(out, fd.summary(e.d.Machine))
	}
	return out
}

// mergedAt gives the merge time of the code review of key.
func (e *Engine) mergedAt(key string) *time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.recs[id(wire.ReviewCode, key)]; r != nil && r.MergedAt != nil {
		t := *r.MergedAt
		return &t
	}
	return nil
}

// foldedKids gives the sub-items of parent whose groups fold into it, in key order.
func (e *Engine) foldedKids(parent string, f Fold) []string {
	if e.d.Bindings == nil || e.directParent(parent, f) != "" {
		return nil
	}
	var out []string
	for _, b := range e.d.Bindings.Children(parent) {
		if k := strings.ToUpper(b.Key); e.parentOf(k, f) == parent {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, keyCompare)
	return slices.Compact(out)
}

// foldedDetail is the detail of parent with the groups of its sub-items. Each file and round carries the key of its
// part. A sub-item with no group is not a part. ok is false when no sub-item has a group.
func (e *Engine) foldedDetail(ctx context.Context, parent string, kids []string) (wire.ReviewDetail, bool, error) {
	fd := &folded{parent: parent, merged: map[string]*time.Time{}}
	d := wire.ReviewDetail{Files: []wire.ReviewFile{}, Comments: []wire.ReviewComment{}, Rounds: []wire.ReviewRound{}}
	for _, k := range append(kids, parent) {
		one, err := e.codeDetail(ctx, k)
		if kindIsNotFound(err) {
			continue
		}
		if err != nil {
			return d, true, err
		}
		if k != parent && one.Summary.Key != k {
			continue // the group of the parent, through a binding in parent mode
		}
		for _, f := range one.Files {
			f.Item = k
			d.Files = append(d.Files, f)
		}
		for _, r := range one.Rounds {
			r.Item = k
			d.Rounds = append(d.Rounds, r)
		}
		d.Comments = append(d.Comments, one.Comments...)
		fd.parts = append(fd.parts, one.Summary)
		fd.merged[k] = e.mergedAt(k)
	}
	if len(fd.parts) == 0 || fd.parts[0].Key == parent {
		return d, false, nil
	}
	d.Summary = fd.summary(e.d.Machine)
	return d, true, nil
}

func kindIsNotFound(err error) bool {
	var r *server.Refusal
	return errors.As(err, &r) && r.Kind == server.NotFound
}
