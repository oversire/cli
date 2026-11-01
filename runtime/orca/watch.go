package orca

import (
	"context"
	"time"

	vorca "github.com/trebi-ai/agent-visor/backend/orca"
)

// fresh is the age limit of a visor snapshot. An older snapshot gives a new read.
const fresh = 10 * time.Second

// The Orca state types come from the orca backend of agent-visor. The app parses the same JSON in
// OrcaRuntime.cards(from:).
type (
	WorktreeCard = vorca.Worktree
	AgentCard    = vorca.Agent
	Terminal     = vorca.Terminal
)

// Worktrees gives the workspaces of the local Orca: the last poll of the visor backend, else
// `orca worktree ps`.
func (o *Orca) Worktrees(ctx context.Context) ([]WorktreeCard, error) {
	if s, ok := o.snapshot(); ok {
		return s.Worktrees, nil
	}
	return o.cli.Worktrees(ctx)
}

// Terminals gives the terminals of the local Orca: the last poll of the visor backend, else
// `orca terminal list`.
func (o *Orca) Terminals(ctx context.Context) ([]Terminal, error) {
	if s, ok := o.snapshot(); ok {
		return s.Terminals, nil
	}
	return o.cli.Terminals(ctx)
}

// Reachable tells whether `orca status` reaches the local Orca runtime.
func (o *Orca) Reachable(ctx context.Context) bool { return o.cli.Reachable(ctx) }

func (o *Orca) snapshot() (vorca.Snapshot, bool) {
	if o.backend == nil {
		return vorca.Snapshot{}, false
	}
	s := o.backend.Snapshot()
	return s, !s.At.IsZero() && o.now().Sub(s.At) < fresh
}
