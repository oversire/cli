//go:build !windows

package oversire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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

// ensureFolder gives the visor workdir that holds dir, with no new worktree. dir can be a folder that is not a git repo.
// The visor can give a workdir of a parent folder, so the session sets its working dir to dir.
func (rt *Runtime) ensureFolder(ctx context.Context, dir string) (agentvisor.Workdir, error) {
	w, err := rt.host.EnsureWorkdir(ctx, agentvisor.WorkdirSpec{Path: dir, Kind: agentvisor.WorkdirFolder})
	if err != nil {
		return w, fmt.Errorf("open the folder %s: %w", dir, err)
	}
	return w, nil
}
