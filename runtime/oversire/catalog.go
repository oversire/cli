package oversire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/workdir"

	"github.com/oversire/cli/wire"
)

// Repos gives the projects of the visor.
func (rt *Runtime) Repos(ctx context.Context) ([]wire.DaemonRepo, error) {
	ps, err := rt.host.Projects(ctx, agentvisor.Filter{})
	if err != nil {
		return nil, err
	}
	out := []wire.DaemonRepo{}
	for _, p := range ps {
		out = append(out, wire.DaemonRepo{ID: p.ID, Runtime: ID, Name: p.Name, Path: p.Path, Folder: !p.Git})
	}
	return out, nil
}

// Workspaces gives the active workdirs of the visor, each with its OverSire sessions as terminals.
func (rt *Runtime) Workspaces(ctx context.Context) ([]wire.DaemonWorkspace, error) {
	wds, err := rt.host.Workdirs(ctx, agentvisor.Filter{})
	if err != nil {
		return nil, err
	}
	ps, err := rt.host.Projects(ctx, agentvisor.Filter{})
	if err != nil {
		return nil, err
	}
	projects := map[string]agentvisor.Project{}
	for _, p := range ps {
		projects[p.ID] = p
	}
	vss, err := rt.host.Sessions(ctx, agentvisor.Filter{Labels: map[string]string{LabelRuntime: ID}})
	if err != nil {
		return nil, err
	}
	out := []wire.DaemonWorkspace{}
	for _, w := range wds {
		if w.State != agentvisor.WorkdirActive {
			continue
		}
		ws := wire.DaemonWorkspace{
			ID: workspaceID(projects[w.Project], w), Runtime: ID, Repo: w.Project, Path: w.Path, Name: w.Key,
			Kind: wire.WorkspaceKind(w.Kind), Branch: w.Branch, LastActivity: w.UpdatedAt,
		}
		for _, vs := range vss {
			if vs.Workdir != w.ID {
				continue
			}
			if vs.State.Live() {
				ws.LiveTerminals++
			}
			if vs.UpdatedAt.After(ws.LastActivity) {
				ws.LastActivity = vs.UpdatedAt
			}
			id := vs.Labels[LabelSession]
			ws.Terminals = append(ws.Terminals, wire.DaemonTerminal{ID: id, Title: vs.Title, Session: id, Orphaned: vs.State == agentvisor.StateLost})
		}
		out = append(out, ws)
	}
	return out, nil
}

// workspaceID is "<repo name>/<key>", the workspace id of the sessions. A workdir with no key has its visor id.
func workspaceID(p agentvisor.Project, w agentvisor.Workdir) string {
	if w.Key == "" || p.Path == "" {
		return w.ID
	}
	return filepath.Base(p.Path) + "/" + w.Key
}

// RemoveWorkspace stops the sessions of the workdir and removes it. The visor runs the teardown script
// of the repository, so runHooks has no effect.
func (rt *Runtime) RemoveWorkspace(ctx context.Context, id string, force, _ bool) (wire.WorkspaceRemoval, error) {
	var none wire.WorkspaceRemoval
	wds, err := rt.host.Workdirs(ctx, agentvisor.Filter{})
	if err != nil {
		return none, err
	}
	ps, err := rt.host.Projects(ctx, agentvisor.Filter{})
	if err != nil {
		return none, err
	}
	projects := map[string]agentvisor.Project{}
	for _, p := range ps {
		projects[p.ID] = p
	}
	var found *agentvisor.Workdir
	for i, w := range wds {
		if w.ID == id || workspaceID(projects[w.Project], w) == id {
			found = &wds[i]
			break
		}
	}
	if found == nil {
		return none, fmt.Errorf("The oversire runtime has no workspace %s.", id)
	}
	if found.Kind != agentvisor.WorkdirWorktree {
		return none, fmt.Errorf("The workspace %s is not a worktree. Remove only a worktree.", id)
	}
	vss, err := rt.host.Sessions(ctx, agentvisor.Filter{Workdir: found.ID})
	if err != nil {
		return none, err
	}
	for _, vs := range vss {
		if vs.State == agentvisor.StateExited {
			continue
		}
		if s, err := rt.session(wire.SessionRef{ID: vs.Labels[LabelSession]}); err == nil {
			err = s.stop(ctx)
		} else {
			err = rt.host.Stop(ctx, vs.ID)
		}
		if err != nil {
			return none, err
		}
	}
	if err := rt.host.RemoveWorkdir(ctx, found.ID, force); err != nil {
		if errors.Is(err, workdir.ErrDirty) {
			return none, fmt.Errorf("The worktree %s has changes. Remove it with force to delete them.", found.Path)
		}
		return none, err
	}
	return none, nil
}
