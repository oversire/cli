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

// QuestionKind is the kind of a RunnerQuestionItem.
type QuestionKind string

const (
	QuestionSingle QuestionKind = "single"
	QuestionMulti  QuestionKind = "multi"
	QuestionFree   QuestionKind = "free"
)

// RunnerQuestion is a question of an agent. ID is the pending request id of the runtime.
type RunnerQuestion struct {
	ID      string               `json:"id"`
	Round   int                  `json:"round"`
	Items   []RunnerQuestionItem `json:"items"`
	AskedAt time.Time            `json:"askedAt"`
}

type RunnerQuestionItem struct {
	ID          string           `json:"id"`
	Header      string           `json:"header,omitempty"`
	Text        string           `json:"text"`
	Why         string           `json:"why,omitempty"`
	Kind        QuestionKind     `json:"kind"`
	Options     []QuestionOption `json:"options"`
	AllowsOther bool             `json:"allowsOther"`
}

type QuestionOption struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
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
	Question   *RunnerQuestion    `json:"question,omitempty"`
	Error      string             `json:"error,omitempty"`
	StartedAt  time.Time          `json:"startedAt"`
	UpdatedAt  time.Time          `json:"updatedAt"`
}
