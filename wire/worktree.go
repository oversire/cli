package wire

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CollectionWorktreeBindings is the store collection of the worktree bindings. The id is the item key.
const CollectionWorktreeBindings = "worktree-bindings"

// WorktreeMode tells if an item has its own group or works in the group of its parent.
type WorktreeMode string

const (
	WorktreeOwn    WorktreeMode = "own"
	WorktreeParent WorktreeMode = "parent"
)

// WorktreeBinding is the fixed worktree folder of one item (store.json WorktreeBinding). The app writes it.
type WorktreeBinding struct {
	Key       string       `json:"key"`
	Machine   string       `json:"machine"`
	Root      string       `json:"root"`
	Group     string       `json:"group"`
	Branch    string       `json:"branch"`
	Parent    string       `json:"parent,omitempty"`
	Mode      WorktreeMode `json:"mode"`
	Source    string       `json:"source"`
	UpdatedAt time.Time    `json:"updatedAt"`
	UpdatedBy string       `json:"updatedBy,omitempty"`
}

// Dir is the absolute folder of the group.
func (b WorktreeBinding) Dir() string {
	if filepath.IsAbs(b.Group) {
		return filepath.Clean(b.Group)
	}
	return filepath.Join(b.Root, b.Group)
}

// Valid is false for a binding with no key, machine or group, or with a .. part in the group.
func (b WorktreeBinding) Valid() bool {
	return b.Key != "" && b.Machine != "" && b.Group != "" && !slices.Contains(strings.Split(filepath.ToSlash(b.Group), "/"), "..")
}

// WorktreeBindingsPage is the answer of GET /v1/worktree-bindings.
type WorktreeBindingsPage struct {
	Bindings []WorktreeBinding `json:"bindings"`
}

// WorktreeGuess is the item that a worktree with no binding is likely for.
type WorktreeGuess struct {
	Key  string `json:"key"`
	From string `json:"from"` // cwd, branch or prompt
}

// WorktreeInfo is one git worktree on the machine of the daemon.
type WorktreeInfo struct {
	Path        string         `json:"path"`
	Repo        string         `json:"repo"`
	Branch      string         `json:"branch"`
	Base        string         `json:"base"`
	Uncommitted int            `json:"uncommitted"`
	Ahead       int            `json:"ahead"`
	Session     string         `json:"session,omitempty"`
	Guess       *WorktreeGuess `json:"guess,omitempty"`
}

// WorktreesPage is the answer of GET /v1/worktrees.
type WorktreesPage struct {
	Worktrees []WorktreeInfo `json:"worktrees"`
	At        time.Time      `json:"at"`
}
