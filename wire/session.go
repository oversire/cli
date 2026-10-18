package wire

import "time"

// RunnerSessionState is the state of a session of the oversire runtime.
type RunnerSessionState string

const (
	SessionQueued            RunnerSessionState = "queued"
	SessionStarting          RunnerSessionState = "starting"
	SessionWorking           RunnerSessionState = "working"
	SessionWaitingInput      RunnerSessionState = "waitingInput"
	SessionWaitingPermission RunnerSessionState = "waitingPermission"
	SessionDone              RunnerSessionState = "done"
	SessionFailed            RunnerSessionState = "failed"
)

// Ended is true for done and failed.
func (s RunnerSessionState) Ended() bool { return s == SessionDone || s == SessionFailed }

// PermissionPrompt is a permission request of an agent. No answer before ExpiresAt is a deny.
type PermissionPrompt struct {
	ID        string    `json:"id"`
	Tool      string    `json:"tool"`
	Text      string    `json:"text"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// RunnerSession is a session of the oversire runtime on a runner. ID is Session.ID.
type RunnerSession struct {
	ID         string             `json:"id"`
	Runner     string             `json:"runner"`
	Session    SessionRef         `json:"session"`
	Order      string             `json:"order"`
	Agent      string             `json:"agent"`
	Repo       string             `json:"repo"`
	Branch     string             `json:"branch,omitempty"`
	Item       *ItemRef           `json:"item,omitempty"`
	State      RunnerSessionState `json:"state"`
	Summary    string             `json:"summary,omitempty"`
	Permission *PermissionPrompt  `json:"permission,omitempty"`
	Error      string             `json:"error,omitempty"`
	StartedAt  time.Time          `json:"startedAt"`
	UpdatedAt  time.Time          `json:"updatedAt"`
}
