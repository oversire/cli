package runtime

import (
	"context"
	"fmt"
	"sync"

	"github.com/oversire/cli/wire"
)

// The Capability raw values of the app that the parts of a runtime give.
const (
	CapSessionInput    = "sessionInput"
	CapSessionTail     = "sessionTail"
	CapSessionHistory  = "sessionHistory"
	CapHostFiles       = "hostFiles"
	CapReveal          = "reveal"
	CapRemoveWorkspace = "removeWorkspace"
	CapWorkspaces      = "workspaces"
	CapWorkers         = "workers"
	CapSessionStream   = "sessionStream"
)

// Capabilities are the parts that rt has. Each runtime takes files: the daemon writes them, not the runtime.
func Capabilities(rt Runtime) []string {
	caps := []string{CapSessionInput, CapSessionTail, CapSessionHistory, CapHostFiles}
	if _, ok := rt.(Catalog); ok {
		caps = append(caps, CapWorkspaces)
	}
	if _, ok := rt.(Revealer); ok {
		caps = append(caps, CapReveal)
	}
	if _, ok := rt.(WorkspaceRemover); ok {
		caps = append(caps, CapRemoveWorkspace)
	}
	if _, ok := rt.(Supervisor); ok {
		caps = append(caps, CapWorkers)
	}
	if _, ok := rt.(Streamer); ok {
		caps = append(caps, CapSessionStream)
	}
	return caps
}

// Status gives each runtime in the order of IDs. A runtime with no Prober is installed and reachable.
func (r *Registry) Status(ctx context.Context) []wire.DaemonRuntime {
	ids := r.IDs()
	out := make([]wire.DaemonRuntime, len(ids))
	r.each(ids, func(i int, rt Runtime) {
		d := wire.DaemonRuntime{ID: rt.ID(), Name: rt.ID(), Installed: true, Reachable: true}
		if p, ok := rt.(Prober); ok {
			d = p.Probe(ctx)
			d.ID = rt.ID()
		}
		d.Capabilities = []string{}
		if d.Reachable {
			d.Capabilities = Capabilities(rt)
			if ss, ok := rt.(StreamStater); ok {
				st := ss.StreamState()
				d.Stream = &st
			}
		}
		out[i] = d
	})
	return out
}

// Repos gives the repos of each Catalog runtime. A runtime that fails gives no repos.
func (r *Registry) Repos(ctx context.Context) wire.DaemonReposPage {
	page := wire.DaemonReposPage{Repos: []wire.DaemonRepo{}}
	var mu sync.Mutex
	r.each(r.IDs(), func(_ int, rt Runtime) {
		c, ok := rt.(Catalog)
		if !ok {
			return
		}
		repos, err := c.Repos(ctx)
		if err != nil {
			return
		}
		mu.Lock()
		page.Repos = append(page.Repos, repos...)
		mu.Unlock()
	})
	return page
}

// Workspaces gives the workspaces of each Catalog runtime. A runtime that fails is in Errors.
func (r *Registry) Workspaces(ctx context.Context) wire.DaemonWorkspacesPage {
	ids := r.IDs()
	lists := make([][]wire.DaemonWorkspace, len(ids))
	errs := make([]error, len(ids))
	r.each(ids, func(i int, rt Runtime) {
		if c, ok := rt.(Catalog); ok {
			lists[i], errs[i] = c.Workspaces(ctx)
		}
	})
	page := wire.DaemonWorkspacesPage{Workspaces: []wire.DaemonWorkspace{}}
	for i, id := range ids {
		if errs[i] != nil {
			if page.Errors == nil {
				page.Errors = map[string]string{}
			}
			page.Errors[id] = errs[i].Error()
			continue
		}
		page.Workspaces = append(page.Workspaces, lists[i]...)
	}
	return page
}

// RemoveWorkspace removes the workspace id of the runtime in rm.
func (r *Registry) RemoveWorkspace(ctx context.Context, id string, rm wire.WorkspaceRemove) (wire.WorkspaceRemoval, error) {
	rt, ok := r.byID[rm.Runtime]
	if !ok {
		return wire.WorkspaceRemoval{}, fmt.Errorf("%w: %s", ErrNoRuntime, rm.Runtime)
	}
	wr, ok := rt.(WorkspaceRemover)
	if !ok {
		return wire.WorkspaceRemoval{}, ErrUnsupported
	}
	return wr.RemoveWorkspace(ctx, id, rm.Force, rm.RunHooks)
}

// Reveal shows the session in the runtime of its account provider.
func (r *Registry) Reveal(ctx context.Context, s wire.SessionRef) error {
	rt, ok := r.byID[s.Workspace.Account.Provider]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRuntime, s.Workspace.Account.Provider)
	}
	rv, ok := rt.(Revealer)
	if !ok {
		return ErrUnsupported
	}
	return rv.Reveal(ctx, s)
}

// Get gives the runtime with the id.
func (r *Registry) Get(id string) (Runtime, bool) {
	rt, ok := r.byID[id]
	return rt, ok
}

// Supervisor gives the runtime with the id when it runs workers.
func (r *Registry) Supervisor(id string) (Supervisor, bool) {
	s, ok := r.byID[id].(Supervisor)
	return s, ok
}

// each calls f for each runtime of ids at the same time.
func (r *Registry) each(ids []string, f func(i int, rt Runtime)) {
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { f(i, r.byID[id]) })
	}
	wg.Wait()
}
