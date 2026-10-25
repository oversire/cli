package wire

import "time"

// The runtime API of the daemon. Each daemon talks only to the runtimes of its own machine.

// ViaAskReply is the via of an ask of an old Enhance worker. The daemon does not send it.
const ViaAskReply QuestionVia = "askReply"

// DaemonRuntime is one agent runtime on the machine.
type DaemonRuntime struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Installed    bool           `json:"installed"`
	Reachable    bool           `json:"reachable"`
	Version      string         `json:"version,omitempty"`
	Error        string         `json:"error,omitempty"`
	Capabilities []string       `json:"capabilities"` // Capability raw values of the app
	Stream       *RuntimeStream `json:"stream,omitempty"`
}

// StreamState: live, pairing or textOnly.
type StreamState string

const (
	StreamLive     StreamState = "live"
	StreamPairing  StreamState = "pairing"
	StreamTextOnly StreamState = "textOnly"
)

// RuntimeStream is the state of the live session stream of a runtime.
type RuntimeStream struct {
	State    StreamState `json:"state"`
	Reason   string      `json:"reason,omitempty"`
	Pairable bool        `json:"pairable,omitempty"` // POST /v1/runtimes/{id}/pair takes a link
	Paired   bool        `json:"paired,omitempty"`
}

// RuntimePair is the body of POST /v1/runtimes/{id}/pair. The link holds a secret: never log it.
type RuntimePair struct {
	Link string `json:"link"`
}

// DaemonRuntimesPage is the answer of GET /v1/runtimes.
type DaemonRuntimesPage struct {
	Runtimes []DaemonRuntime `json:"runtimes"`
}

// DaemonRepo is one repo that a runtime knows.
type DaemonRepo struct {
	ID      string `json:"id"`
	Runtime string `json:"runtime"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Folder  bool   `json:"folder"`
}

// DaemonReposPage is the answer of GET /v1/repos.
type DaemonReposPage struct {
	Repos []DaemonRepo `json:"repos"`
}

// DaemonTerminal is one terminal of a workspace.
type DaemonTerminal struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Session  string `json:"session,omitempty"` // the DaemonSession id
	Orphaned bool   `json:"orphaned"`
}

// WorkspaceKind: worktree, folder or main.
type WorkspaceKind string

const (
	KindWorktree WorkspaceKind = "worktree"
	KindFolder   WorkspaceKind = "folder"
	KindMain     WorkspaceKind = "main"
)

// DaemonWorkspace is one runtime workspace.
type DaemonWorkspace struct {
	ID            string           `json:"id"`
	Runtime       string           `json:"runtime"`
	Repo          string           `json:"repo,omitempty"`
	Path          string           `json:"path"`
	Name          string           `json:"name,omitempty"`
	Comment       string           `json:"comment,omitempty"`
	Kind          WorkspaceKind    `json:"kind"`
	Branch        string           `json:"branch,omitempty"`
	LiveTerminals int              `json:"liveTerminals"`
	LastActivity  time.Time        `json:"lastActivity"`
	Terminals     []DaemonTerminal `json:"terminals,omitempty"`
}

// DaemonWorkspacesPage is the answer of GET /v1/workspaces. A runtime that fails is in Errors.
type DaemonWorkspacesPage struct {
	Workspaces []DaemonWorkspace `json:"workspaces"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// WorkspaceRemove is the body of DELETE /v1/workspaces/{id}.
type WorkspaceRemove struct {
	Runtime  string `json:"runtime"`
	Force    bool   `json:"force"`
	RunHooks bool   `json:"runHooks"`
}

// WorkspaceRemoval is the answer of DELETE /v1/workspaces/{id}.
type WorkspaceRemoval struct {
	Warning string `json:"warning,omitempty"`
}

// WorkspaceFile is one file of POST /v1/files.
type WorkspaceFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"` // base64 on the wire
}

// WorkspaceFiles is the body of POST /v1/files. The daemon chooses the folder.
type WorkspaceFiles struct {
	Files []WorkspaceFile `json:"files"`
}

// WorkspaceFilesResult has the full paths, in the order of the files.
type WorkspaceFilesResult struct {
	Paths []string `json:"paths"`
}
