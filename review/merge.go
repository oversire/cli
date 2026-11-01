package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oversire/cli/wire"
)

// Merge merges the branch of each repo of the group of key into its base, in merge order, all or none.
// A repo that is behind its base is rebased first. A conflict stops the merge: the rebase goes back,
// each base that merged goes back to its old head, and the review is in the conflict state.
// A failed check before the merge is an Error of kind Conflict. The daemon never pushes.
func (e *Engine) Merge(ctx context.Context, key string, m wire.MergeRequest) (wire.MergeResult, error) {
	res := wire.MergeResult{Repos: []wire.RepoMerge{}, NotPushed: []wire.NotPushed{}}
	if !validKey(key) {
		return res, refuse(Invalid, "The key %q is not valid.", key)
	}
	e.send.Lock()
	defer e.send.Unlock()
	g, ok := findGroup(e.allRoots(ctx), key)
	if !ok {
		return res, refuse(NotFound, "There is no group .wt/%s in the roots of this daemon.", strings.ToLower(key))
	}
	repos, err := e.checkMerge(ctx, g)
	if err != nil {
		return res, err
	}

	git := e.d.Git
	type done struct {
		primary, old string
		at           int
	}
	var merged []done
	type mergeRow struct {
		repo wire.ReviewRepo
		rm   wire.RepoMerge
	}
	var rows []mergeRow
	fail := func(name string, i int, rm wire.RepoMerge, text string) {
		res.Repos = append(res.Repos, rm)
		for _, r := range repos[i+1:] {
			res.Repos = append(res.Repos, wire.RepoMerge{Name: r.Name, Result: wire.MergeSkipped})
		}
		for _, d := range merged {
			if _, err := git.Run(ctx, d.primary, "reset", "--keep", d.old); err != nil {
				text += " The base of " + filepath.Base(d.primary) + " did not go back: " + err.Error()
				continue
			}
			res.Repos[d.at] = wire.RepoMerge{Name: res.Repos[d.at].Name, Result: wire.MergeSkipped}
		}
		res.Error = name + ": " + text
	}
	for i, r := range repos {
		if r.Ahead == 0 {
			res.Repos = append(res.Repos, wire.RepoMerge{Name: r.Name, Result: wire.MergeSkipped})
			continue
		}
		primary, wt := g.primary(r.Name), g.worktree(r.Name)
		old, err := git.Run(ctx, primary, "rev-parse", r.Base)
		if err != nil {
			fail(r.Name, i, wire.RepoMerge{Name: r.Name, Result: wire.MergeStopped}, err.Error()+" Nothing merged.")
			break
		}
		rm := wire.RepoMerge{Name: r.Name, Result: wire.MergeMerged}
		if r.Behind > 0 {
			if _, err := git.Run(ctx, wt, "rebase", r.Base); err != nil {
				files, _ := git.Run(ctx, wt, "diff", "--name-only", "--diff-filter=U")
				_, _ = git.Run(ctx, wt, "rebase", "--abort")
				rm.Result, rm.ConflictFiles = wire.MergeStopped, lines(files)
				fail(r.Name, i, rm, fmt.Sprintf("the rebase on %s has a conflict. Nothing merged.", r.Base))
				e.conflict(key, true)
				break
			}
			rm.Result = wire.MergeRebased
		}
		if _, err := git.Run(ctx, primary, "merge", "--ff-only", r.Branch); err != nil {
			rm.Result = wire.MergeStopped
			fail(r.Name, i, rm, "the fast-forward of "+r.Base+" failed. Nothing merged. "+err.Error())
			e.conflict(key, false)
			break
		}
		merged = append(merged, done{primary, old, len(res.Repos)})
		rm.Head, _ = git.Run(ctx, primary, "rev-parse", "--short", r.Base)
		res.Repos = append(res.Repos, rm)
		rows = append(rows, mergeRow{r, rm})
	}
	if res.Error == "" {
		res.Merged = true
		e.mu.Lock()
		rec := e.rec(wire.ReviewCode, key)
		now := e.now()
		rec.MergedAt, rec.Conflict, rec.Root = &now, false, g.Root
		e.changed(rec, wire.EventReviewMerged)
		e.mu.Unlock()
		var names []string
		for _, m := range rows {
			names = append(names, m.repo.Name)
			// The id is the one of the git watcher, so that its read of the reflog gives no second row.
			sha, _ := git.Run(ctx, g.primary(m.repo.Name), "rev-parse", m.repo.Base)
			mode := "fastForward"
			if m.rm.Result == wire.MergeRebased {
				mode = "rebase"
			}
			e.event(key, "git:"+strings.TrimSpace(sha)+":refs/heads/"+m.repo.Base, wire.KindGitMerge, wire.ActorHuman,
				fmt.Sprintf("Merged %s into %s (%s)", m.repo.Branch, m.repo.Base, m.repo.Name),
				map[string]any{"sha": strings.TrimSpace(sha), "ref": "refs/heads/" + m.repo.Base, "branch": m.repo.Branch, "mode": mode,
					"commits": m.repo.Ahead, "worktree": g.primary(m.repo.Name)})
		}
		if len(names) > 0 {
			e.event(key, fmt.Sprintf("review:code:%s:shipped:%d", key, now.UnixMilli()), wire.KindItemShipped, wire.ActorHuman,
				"Shipped "+strings.Join(names, ", "), map[string]any{"repos": names, "group": g.Rel()})
		}
		if m.RemoveGroup {
			if err := e.removeGroup(ctx, g); err != nil {
				res.Error = "The merge is done, but the group stays: " + err.Error()
			} else {
				res.Removed = true
			}
		}
	}
	res.NotPushed = e.notPushed(ctx, []string{g.Root})
	return res, nil
}

// checkMerge gives the repos of g in merge order when the merge can start.
func (e *Engine) checkMerge(ctx context.Context, g group) ([]wire.ReviewRepo, error) {
	key := g.Key()
	if _, rs := e.sessionOf(wire.ReviewCode, key, g.Dir()); rs.State == wire.ReviewSessionWorking {
		return nil, refuse(Conflict, "The session of %s works. Wait for its hand-off.", key)
	}
	var repos []wire.ReviewRepo
	ahead := false
	for _, name := range e.d.Order.Sort(g.Repos) {
		r, _, err := e.d.Git.repo(ctx, g, name)
		if err != nil {
			return nil, refuse(Conflict, "%s: %v", name, err)
		}
		if r.Dirty {
			return nil, refuse(Conflict, "%s: the worktree has changes that are not committed. Commit or discard them first.", name)
		}
		if r.Ahead > 0 {
			ahead = true
			if r.Base == "HEAD" {
				return nil, refuse(Conflict, "%s: the primary checkout %s is not on a branch.", name, g.primary(name))
			}
			st, err := e.d.Git.Run(ctx, g.primary(name), "status", "--porcelain", "--untracked-files=no")
			if err != nil {
				return nil, refuse(Conflict, "%s: %v", name, err)
			}
			if st != "" {
				return nil, refuse(Conflict, "%s: the primary checkout %s has changes that are not committed.", name, g.primary(name))
			}
		}
		repos = append(repos, r)
	}
	if !ahead {
		return nil, refuse(Conflict, "%s has no commits to merge.", g.Rel())
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
	if err := os.Remove(g.Dir()); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
