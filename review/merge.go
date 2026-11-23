package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// part is one group of a merge and the key of its review.
type part struct {
	key   string
	g     group
	repos []wire.ReviewRepo
}

// heads keeps the first old head of each primary checkout, for the rollback of a merge.
// One primary can take the merges of many parts, so only its first head is the old one.
type heads struct {
	order []string
	old   map[string]string
}

func (h *heads) keep(primary, old string) {
	if h.old == nil {
		h.old = map[string]string{}
	}
	if _, ok := h.old[primary]; !ok {
		h.old[primary] = old
		h.order = append(h.order, primary)
	}
}

// restore resets each primary to its old head, the last first. It gives the failures by primary.
func (h *heads) restore(ctx context.Context, git Git) map[string]error {
	failed := map[string]error{}
	for i := len(h.order) - 1; i >= 0; i-- {
		p := h.order[i]
		if _, err := git.Run(ctx, p, "reset", "--keep", h.old[p]); err != nil {
			failed[p] = err
		}
	}
	return failed
}

// Merge merges the branch of each repo of the group of key into its base, in merge order, all or none.
// With m.Items it merges the groups of these items (key and its sub-items, OVERSIRE-592), the sub-items in key order,
// then key. A repo that is behind its base is rebased first. A conflict stops the merge: the rebase goes back,
// each base that merged goes back to its old head, and the review of the part is in the conflict state.
// A failed check before the merge is an Error of kind Conflict. The daemon never pushes.
func (e *Engine) Merge(ctx context.Context, key string, m wire.MergeRequest) (wire.MergeResult, error) {
	res := wire.MergeResult{Repos: []wire.RepoMerge{}, NotPushed: []wire.NotPushed{}}
	if !validKey(key) {
		return res, server.Refuse(server.Invalid, "The key %q is not valid.", key)
	}
	e.send.Lock()
	defer e.send.Unlock()
	many := len(m.Items) > 0
	var parts []part
	if many {
		ps, err := e.mergeOrder(ctx, key, m.Items)
		if err != nil {
			return res, err
		}
		parts = ps
	} else {
		g, ok := e.groupOf(ctx, key)
		if !ok {
			return res, noGroup(key)
		}
		parts = []part{{key: key, g: g}}
	}
	for i := range parts {
		repos, err := e.checkMerge(ctx, parts[i].g)
		var rf *server.Refusal
		if many && errors.As(err, &rf) {
			err = server.Refuse(rf.Kind, "%s: %s", parts[i].key, rf.Text)
		}
		if err != nil {
			return res, err
		}
		parts[i].repos = repos
	}
	item, prefix := func(string) string { return "" }, func(string) string { return "" }
	if many {
		item, prefix = func(k string) string { return k }, func(k string) string { return k + ": " }
	}

	git := e.d.Git
	var hs heads
	type done struct {
		primary string
		at      int
	}
	var merged []done
	type mergeRow struct {
		key  string
		g    group
		repo wire.ReviewRepo
		rm   wire.RepoMerge
	}
	var rows []mergeRow
	fail := func(pi, ri int, rm wire.RepoMerge, text string) {
		res.Repos = append(res.Repos, rm)
		for _, r := range parts[pi].repos[ri+1:] {
			res.Repos = append(res.Repos, wire.RepoMerge{Name: r.Name, Result: wire.MergeSkipped, Item: item(parts[pi].key)})
		}
		for _, p := range parts[pi+1:] {
			for _, r := range p.repos {
				res.Repos = append(res.Repos, wire.RepoMerge{Name: r.Name, Result: wire.MergeSkipped, Item: item(p.key)})
			}
		}
		failed := hs.restore(ctx, git)
		for _, p := range hs.order {
			if err := failed[p]; err != nil {
				text += " The base of " + filepath.Base(p) + " did not go back: " + err.Error()
			}
		}
		for _, d := range merged {
			if failed[d.primary] == nil {
				res.Repos[d.at] = wire.RepoMerge{Name: res.Repos[d.at].Name, Result: wire.MergeSkipped, Item: res.Repos[d.at].Item}
			}
		}
		res.Error = prefix(parts[pi].key) + parts[pi].repos[ri].Name + ": " + text
	}
parts:
	for pi := range parts {
		p := &parts[pi]
		if pi > 0 {
			// An earlier part moved the base.
			for ri, r := range p.repos {
				fresh, _, err := git.repo(ctx, p.g, r.Name)
				if err != nil {
					fail(pi, ri, wire.RepoMerge{Name: r.Name, Result: wire.MergeStopped, Item: item(p.key)}, err.Error()+" Nothing merged.")
					break parts
				}
				p.repos[ri] = fresh
			}
		}
		for ri, r := range p.repos {
			if r.Ahead == 0 {
				res.Repos = append(res.Repos, wire.RepoMerge{Name: r.Name, Result: wire.MergeSkipped, Item: item(p.key)})
				continue
			}
			primary, wt := p.g.primary(r.Name), p.g.worktree(r.Name)
			old, err := git.Run(ctx, primary, "rev-parse", r.Base)
			if err != nil {
				fail(pi, ri, wire.RepoMerge{Name: r.Name, Result: wire.MergeStopped, Item: item(p.key)}, err.Error()+" Nothing merged.")
				break parts
			}
			rm := wire.RepoMerge{Name: r.Name, Result: wire.MergeMerged, Item: item(p.key)}
			if r.Behind > 0 {
				if _, err := git.Run(ctx, wt, "rebase", r.Base); err != nil {
					files, _ := git.Run(ctx, wt, "diff", "--name-only", "--diff-filter=U")
					_, _ = git.Run(ctx, wt, "rebase", "--abort")
					rm.Result, rm.ConflictFiles = wire.MergeStopped, lines(files)
					fail(pi, ri, rm, fmt.Sprintf("the rebase on %s has a conflict. Nothing merged.", r.Base))
					e.conflict(p.key, true)
					break parts
				}
				rm.Result = wire.MergeRebased
			}
			if _, err := git.Run(ctx, primary, "merge", "--ff-only", r.Branch); err != nil {
				rm.Result = wire.MergeStopped
				fail(pi, ri, rm, "the fast-forward of "+r.Base+" failed. Nothing merged. "+err.Error())
				e.conflict(p.key, false)
				break parts
			}
			hs.keep(primary, old)
			merged = append(merged, done{primary, len(res.Repos)})
			rm.Head, _ = git.Run(ctx, primary, "rev-parse", "--short", r.Base)
			res.Repos = append(res.Repos, rm)
			rows = append(rows, mergeRow{p.key, p.g, r, rm})
		}
	}
	if res.Error == "" {
		res.Merged = true
		now := e.now()
		e.mu.Lock()
		for _, p := range parts {
			at := now
			rec := e.rec(wire.ReviewCode, p.key)
			rec.MergedAt, rec.Conflict, rec.Root = &at, false, p.g.Root
			e.changed(rec, wire.EventReviewMerged)
		}
		e.mu.Unlock()
		for _, p := range parts {
			var names []string
			for _, m := range rows {
				if m.key != p.key {
					continue
				}
				names = append(names, m.repo.Name)
				// The id is the one of the git watcher, so that its read of the reflog gives no second row.
				sha, _ := git.Run(ctx, m.g.primary(m.repo.Name), "rev-parse", m.rm.Head)
				mode := "fastForward"
				if m.rm.Result == wire.MergeRebased {
					mode = "rebase"
				}
				e.event(p.key, "git:"+strings.TrimSpace(sha)+":refs/heads/"+m.repo.Base, wire.KindGitMerge, wire.ActorHuman,
					fmt.Sprintf("Merged %s into %s (%s)", m.repo.Branch, m.repo.Base, m.repo.Name),
					map[string]any{"sha": strings.TrimSpace(sha), "ref": "refs/heads/" + m.repo.Base, "branch": m.repo.Branch, "mode": mode,
						"commits": m.repo.Ahead, "worktree": m.g.primary(m.repo.Name)})
			}
			if len(names) > 0 {
				e.event(p.key, fmt.Sprintf("review:code:%s:shipped:%d", p.key, now.UnixMilli()), wire.KindItemShipped, wire.ActorHuman,
					"Shipped "+strings.Join(names, ", "), map[string]any{"repos": names, "group": p.g.Rel()})
			}
		}
		if m.RemoveGroup {
			var errs []string
			for _, p := range parts {
				if err := e.removeGroup(ctx, p.g); err != nil {
					errs = append(errs, prefix(p.key)+err.Error())
				}
			}
			if len(errs) > 0 {
				res.Error = "The merge is done, but the group stays: " + strings.Join(errs, "; ")
			} else {
				res.Removed = true
			}
		}
	}
	var roots []string
	for _, p := range parts {
		if !slices.Contains(roots, p.g.Root) {
			roots = append(roots, p.g.Root)
		}
	}
	res.NotPushed = e.notPushed(ctx, roots)
	return res, nil
}

// mergeOrder gives the parts of a merge of items from the review of parent: each item is parent or one of its
// sub-items. A folder shows once. The order is the sub-items in key order, then the part of parent.
func (e *Engine) mergeOrder(ctx context.Context, parent string, items []string) ([]part, error) {
	parent = strings.ToUpper(parent)
	var out []part
	seen := map[string]bool{}
	for _, it := range items {
		k := strings.ToUpper(strings.TrimSpace(it))
		if !validKey(k) {
			return nil, server.Refuse(server.Invalid, "The key %q is not valid.", it)
		}
		if k != parent {
			var b wire.WorktreeBinding
			ok := false
			if e.d.Bindings != nil {
				b, ok = e.d.Bindings.Get(k)
			}
			if !ok || !strings.EqualFold(b.Parent, parent) {
				return nil, server.Refuse(server.Invalid, "%s is not a sub-item of %s.", k, parent)
			}
		}
		g, ok := e.groupOf(ctx, k)
		if !ok {
			return nil, server.Refuse(server.NotFound, "%s has no group.", k)
		}
		if seen[g.Dir()] {
			continue
		}
		seen[g.Dir()] = true
		// A sub-item in parent mode gives the group of the parent, with the key of the parent.
		out = append(out, part{key: g.Key(), g: g})
	}
	slices.SortStableFunc(out, func(a, b part) int {
		switch {
		case a.key == b.key:
			return 0
		case a.key == parent:
			return 1
		case b.key == parent:
			return -1
		}
		return keyCompare(a.key, b.key)
	})
	return out, nil
}

// checkMerge gives the repos of g in merge order when the merge can start.
func (e *Engine) checkMerge(ctx context.Context, g group) ([]wire.ReviewRepo, error) {
	key := g.Key()
	if _, rs := e.sessionOf(wire.ReviewCode, key, g.Dir()); rs.State == wire.ReviewSessionWorking {
		return nil, server.Refuse(server.Conflict, "The session of %s works. Wait for its hand-off.", key)
	}
	var repos []wire.ReviewRepo
	ahead := false
	for _, name := range e.d.Order.Sort(g.Repos) {
		r, _, err := e.d.Git.repo(ctx, g, name)
		if err != nil {
			return nil, server.Refuse(server.Conflict, "%s: %v", name, err)
		}
		if r.Dirty {
			return nil, server.Refuse(server.Conflict, "%s: the worktree has changes that are not committed. Commit or discard them first.", name)
		}
		if r.Ahead > 0 {
			ahead = true
			if r.Base == "HEAD" {
				return nil, server.Refuse(server.Conflict, "%s: the primary checkout %s is not on a branch.", name, g.primary(name))
			}
			// Changes in the primary checkout stop the merge only in files of the branch.
			// The fast-forward and the rollback (reset --keep) keep the other changes.
			dirty, err := e.d.Git.Run(ctx, g.primary(name), "diff", "--name-only", "--no-renames", "HEAD")
			if err != nil {
				return nil, server.Refuse(server.Conflict, "%s: %v", name, err)
			}
			if dirty != "" {
				incoming, err := e.d.Git.Run(ctx, g.primary(name), "diff", "--name-only", "--no-renames", r.Base+"..."+r.Branch)
				if err != nil {
					return nil, server.Refuse(server.Conflict, "%s: %v", name, err)
				}
				var shared []string
				branch := lines(incoming)
				for _, f := range lines(dirty) {
					if slices.Contains(branch, f) {
						shared = append(shared, f)
					}
				}
				if len(shared) > 0 {
					return nil, server.Refuse(server.Conflict, "%s: the primary checkout %s has changes that are not committed in files of %s: %s. Commit or stash them first.",
						name, g.primary(name), r.Branch, strings.Join(shared, ", "))
				}
			}
		}
		repos = append(repos, r)
	}
	if !ahead {
		return nil, server.Refuse(server.Conflict, "%s has no commits to merge.", g.Rel())
	}
	return repos, nil
}

// conflict marks a stopped merge. A rebase conflict puts the review in the conflict state.
func (e *Engine) conflict(key string, state bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.rec(wire.ReviewCode, key)
	if state {
		r.Conflict = true
		e.changed(r, wire.EventReviewConflict)
		return
	}
	e.changed(r, wire.EventReviewChanged)
}

// removeGroup removes each worktree and its merged branch, then the links, then the empty group folder.
// It never removes a folder with its contents: git removes the worktrees, and a folder that is not empty stays.
func (e *Engine) removeGroup(ctx context.Context, g group) error {
	git := e.d.Git
	var errs []string
	for _, name := range g.Repos {
		branch, err := git.Run(ctx, g.worktree(name), "branch", "--show-current")
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if _, err := git.Run(ctx, g.primary(name), "worktree", "remove", g.worktree(name)); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if branch != "" {
			if _, err := git.Run(ctx, g.primary(name), "branch", "-d", branch); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	for _, l := range g.Links {
		p := filepath.Join(g.Dir(), l)
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(p); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	// git removes the folder of a single group with its worktree.
	if err := os.Remove(g.Dir()); err != nil && !(g.single && errors.Is(err, os.ErrNotExist)) {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
