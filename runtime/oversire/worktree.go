package oversire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runner runs one child process. proc.Local is the real one.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// Worktrees keeps one git worktree for each item at <root>/<repo name>/<key>.
type Worktrees struct {
	root string
	git  Runner
}

func NewWorktrees(root string, git Runner) *Worktrees {
	return &Worktrees{root: root, git: git}
}

// Ensure gives the worktree of key in repo on branch. It uses an existing worktree and branch again.
func (w *Worktrees) Ensure(ctx context.Context, repo, key, branch string) (string, error) {
	dir := filepath.Join(w.root, filepath.Base(repo), key)
	if _, err := os.Stat(dir); err == nil {
		if _, err := w.run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
			return "", fmt.Errorf("%s exists, but it is not a git worktree.", dir)
		}
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	// A worktree folder that someone removed stays registered until a prune.
	if _, err := w.run(ctx, repo, "worktree", "prune"); err != nil {
		return "", err
	}
	args := []string{"worktree", "add", dir, branch}
	if _, err := w.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		args = []string{"worktree", "add", "-b", branch, dir}
	}
	if _, err := w.run(ctx, repo, args...); err != nil {
		return "", err
	}
	return dir, nil
}

func (w *Worktrees) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, errOut, err := w.git.Run(ctx, "git", append([]string{"-C", dir}, args...), nil)
	if err != nil {
		text := strings.TrimSpace(string(errOut))
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], text)
	}
	return strings.TrimSpace(string(out)), nil
}

// Changed gives the paths that the worktree dir changed since it left the HEAD of repo:
// the diff from the merge base to the work tree (deletes and both names of a rename included), plus untracked files.
func (w *Worktrees) Changed(ctx context.Context, repo, dir string) ([]string, error) {
	head, err := w.run(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	base, err := w.run(ctx, dir, "merge-base", head, "HEAD")
	if err != nil {
		return nil, err
	}
	diff, err := w.run(ctx, dir, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := w.run(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(diff+"\n"+untracked, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}
