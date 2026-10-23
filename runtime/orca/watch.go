package orca

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// listTimeout is the limit of each list call of the watcher.
const listTimeout = 15 * time.Second

// WorktreeCard is one workspace of `orca worktree ps`. The app parses the same JSON in OrcaRuntime.cards(from:).
type WorktreeCard struct {
	ID          string      `json:"worktreeId"`
	Repo        string      `json:"repo"`
	Path        string      `json:"path"`
	Branch      string      `json:"branch"`
	DisplayName string      `json:"displayName"`
	IsArchived  bool        `json:"isArchived"`
	Agents      []AgentCard `json:"agents"`
}

// BranchName is Branch without refs/heads/.
func (w WorktreeCard) BranchName() string { return strings.TrimPrefix(w.Branch, "refs/heads/") }

// AgentCard is one agent pane of a workspace. The times are Unix milliseconds.
type AgentCard struct {
	PaneKey              string `json:"paneKey"`
	State                string `json:"state"`       // working, waiting, blocked, done
	WorkingMode          string `json:"workingMode"` // "monitoring" when only a background shell runs
	AgentType            string `json:"agentType"`
	Prompt               string `json:"prompt"`
	LastAssistantMessage string `json:"lastAssistantMessage"`
	ToolName             string `json:"toolName"`
	ToolInput            string `json:"toolInput"`
	Interrupted          bool   `json:"interrupted"`
	StateStartedAt       int64  `json:"stateStartedAt"`
	UpdatedAt            int64  `json:"updatedAt"`
}

// Terminal is one terminal of `orca terminal list`.
type Terminal struct {
	Handle          string `json:"handle"`
	WorktreeID      string `json:"worktreeId"`
	WorktreePath    string `json:"worktreePath"`
	TabID           string `json:"tabId"`
	LeafID          string `json:"leafId"`
	RecordedPaneKey string `json:"recordedPaneKey"`
	Title           string `json:"title"`
	AgentIdentity   string `json:"agentIdentity"`
	Orphaned        bool   `json:"orphaned"`
	Connected       bool   `json:"connected"`
}

// PaneKey is the pane of the terminal (tabId:leafId). An orphaned terminal keeps its old pane in recordedPaneKey.
func (t Terminal) PaneKey() string {
	if t.RecordedPaneKey != "" {
		return t.RecordedPaneKey
	}
	return t.TabID + ":" + t.LeafID
}

// Worktrees runs `orca worktree ps` on the local Orca.
func (o *Orca) Worktrees(ctx context.Context) ([]WorktreeCard, error) {
	var r struct {
		Worktrees []WorktreeCard `json:"worktrees"`
	}
	err := o.callInto(ctx, listTimeout, &r, "worktree", "ps", "--limit", "500")
	return r.Worktrees, err
}

// Terminals runs `orca terminal list` on the local Orca.
func (o *Orca) Terminals(ctx context.Context) ([]Terminal, error) {
	var r struct {
		Terminals []Terminal `json:"terminals"`
	}
	err := o.callInto(ctx, listTimeout, &r, "terminal", "list", "--limit", "500")
	return r.Terminals, err
}

// Reachable tells whether `orca status` reaches the local Orca runtime.
func (o *Orca) Reachable(ctx context.Context) bool {
	var r struct {
		Runtime struct {
			Reachable bool `json:"reachable"`
		} `json:"runtime"`
	}
	raw, err := o.call(ctx, listTimeout, "status")
	return err == nil && json.Unmarshal(raw, &r) == nil && r.Runtime.Reachable
}
