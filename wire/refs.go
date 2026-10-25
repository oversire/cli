// Package wire holds the Go types of the OverSire wire contract (contract/schema).
package wire

type AccountID struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

type ProjectRef struct {
	Account AccountID `json:"account"`
	ID      string    `json:"id"`
	Key     string    `json:"key,omitempty"`
}

type ItemRef struct {
	Project ProjectRef `json:"project"`
	ID      string     `json:"id"`
	Key     string     `json:"key"`
	URL     string     `json:"url"`
}

type WorkspaceRef struct {
	Account AccountID `json:"account"`
	ID      string    `json:"id"`
}

type SessionRef struct {
	Workspace WorkspaceRef `json:"workspace"`
	ID        string       `json:"id"`
}

// LaunchRequest starts an agent in a runtime workspace.
type LaunchRequest struct {
	Repo        string   `json:"repo"`
	Item        *ItemRef `json:"item,omitempty"`
	Name        string   `json:"name,omitempty"`
	Note        string   `json:"note,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Agent       string   `json:"agent"`
	Prompt      string   `json:"prompt"`
	Environment string   `json:"environment,omitempty"` // deprecated: the daemon ignores it
	Command     string   `json:"command,omitempty"`     // the resolved start command; empty: the runtime default
	Title       string   `json:"title,omitempty"`       // the terminal title
	BaseBranch  string   `json:"baseBranch,omitempty"`
	InPlace     bool     `json:"inPlace,omitempty"` // a new terminal in the folder Repo, no workspace
	Key         string   `json:"key,omitempty"`     // one live session for each key
}

// WorkspaceName is the name, else the item key.
func (r LaunchRequest) WorkspaceName() string {
	if r.Name == "" && r.Item != nil {
		return r.Item.Key
	}
	return r.Name
}

// WorkspaceNote is the note, else the item URL.
func (r LaunchRequest) WorkspaceNote() string {
	if r.Note == "" && r.Item != nil {
		return r.Item.URL
	}
	return r.Note
}
