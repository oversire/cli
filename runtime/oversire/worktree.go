//go:build !windows

package oversire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentvisor "github.com/trebi-ai/agent-visor"
)

// Runner runs one child process. proc.Local is the real one.
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// ensureWorkdir gives the visor workdir of key in repo: a git worktree on branch. The worktree of an item
// from before agent-visor, at <data>/worktrees/<repo name>/<key>, stays in use.
func (rt *Runtime) ensureWorkdir(ctx context.Context, repo, key, branch, base string) (agentvisor.Workdir, error) {
	spec := agentvisor.WorkdirSpec{Path: repo, Key: key, Kind: agentvisor.WorkdirWorktree, Branch: branch, Base: base}
	if rt.opts.DataDir != "" {
		old := filepath.Join(rt.opts.DataDir, "worktrees", filepath.Base(repo), key)
		if _, err := os.Stat(old); err == nil {
			spec = agentvisor.WorkdirSpec{Path: old, Key: key}
		}
	}
	w, err := rt.host.EnsureWorkdir(ctx, spec)
	if err != nil {
		return w, fmt.Errorf("make the worktree of %s: %w", key, err)
	}
	return w, nil
}

// gitCLI runs git.
type gitCLI struct{ git Runner }

func (g gitCLI) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, errOut, err := g.git.Run(ctx, "git", append([]string{"-C", dir}, args...), nil)
	if err != nil {
		text := strings.TrimSpace(string(errOut))
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], text)
	}
	return strings.TrimSpace(string(out)), nil
}

// changed gives the paths that the worktree dir changed since it left the HEAD of repo:
// the diff from the merge base to the work tree (deletes and both names of a rename included), plus untracked files.
func (g gitCLI) changed(ctx context.Context, repo, dir string) ([]string, error) {
	head, err := g.run(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	base, err := g.run(ctx, dir, "merge-base", head, "HEAD")
	if err != nil {
		return nil, err
	}
	diff, err := g.run(ctx, dir, "diff", "--name-only", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	untracked, err := g.run(ctx, dir, "ls-files", "--others", "--exclude-standard")
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
