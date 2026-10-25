package wire

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionPhase is the state of a DaemonSession. The same values as AgentSession.Phase in the app.
type SessionPhase string

const (
	PhaseStarting SessionPhase = "starting"
	PhaseWorking  SessionPhase = "working"
	PhaseWaiting  SessionPhase = "waiting"
	PhaseDone     SessionPhase = "done"
	PhaseFailed   SessionPhase = "failed"
)

// SessionReason tells why a session waits or stopped. ReasonMonitoring: the session works, but only a background shell runs.
type SessionReason string

const (
	ReasonQuestion    SessionReason = "question"
	ReasonPermission  SessionReason = "permission"
	ReasonTurnEnd     SessionReason = "turnEnd"
	ReasonInterrupted SessionReason = "interrupted"
	ReasonError       SessionReason = "error"
	ReasonMonitoring  SessionReason = "monitoring"
)

// Values of DaemonSession.Sources.
const (
	SessionSourceHook = "hook"
	SessionSourceOrca = "orca"
	SessionSourceWire = "wire"
)

// Values of DaemonSession.Runtime. Open set.
const (
	RuntimeOrca     = "orca"
	RuntimeOversire = "oversire"
	RuntimeCLI      = "cli"
)

// DaemonSession is one agent session on the machine, merged from all sources.
type DaemonSession struct {
	ID             string        `json:"id"`
	Machine        string        `json:"machine"`
	Runtime        string        `json:"runtime"`
	Harness        string        `json:"harness"`
	Workspace      string        `json:"workspace,omitempty"`
	Terminal       string        `json:"terminal,omitempty"`
	HarnessSession string        `json:"harnessSession,omitempty"`
	Cwd            string        `json:"cwd"`
	Repo           string        `json:"repo,omitempty"`
	Branch         string        `json:"branch,omitempty"`
	State          SessionPhase  `json:"state"`
	Reason         SessionReason `json:"reason,omitempty"`
	Question       string        `json:"question,omitempty"`
	Prompt         string        `json:"prompt,omitempty"`
	LastMessage    string        `json:"lastMessage,omitempty"`
	StateSince     time.Time     `json:"stateSince"`
	UpdatedAt      time.Time     `json:"updatedAt"`
	Sources        []string      `json:"sources"`
	Key            string        `json:"key,omitempty"` // the start key of the order that made the session
}

// Limits of DaemonSession text fields, in characters.
const (
	MaxSessionPrompt      = 2000
	MaxSessionLastMessage = 4000
)

// DaemonSessionsPage is the answer of GET /v1/sessions.
type DaemonSessionsPage struct {
	Sessions []DaemonSession `json:"sessions"`
}

// HookEnv is the environment of the agent process. The oversire hook command sends it with each hook call.
type HookEnv struct {
	PaneKey         string `json:"paneKey,omitempty"`
	TabID           string `json:"tabId,omitempty"`
	WorktreeID      string `json:"worktreeId,omitempty"`
	TerminalHandle  string `json:"terminalHandle,omitempty"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	Pwd             string `json:"pwd,omitempty"`
	PPID            int    `json:"ppid,omitempty"`
	OversireSession string `json:"oversireSession,omitempty"`
}

// HookState is the hook state of a harness. HooksUntrusted: Codex did not approve the hooks yet.
type HookState string

const (
	HooksInstalled   HookState = "installed"
	HooksMissing     HookState = "missing"
	HooksUntrusted   HookState = "untrusted"
	HooksOutdated    HookState = "outdated"
	HooksUnsupported HookState = "unsupported"
)

type HarnessHooks struct {
	Name  string    `json:"name"`
	Hooks HookState `json:"hooks"`
}

// Values of the roles of a daemon. Open set.
const (
	RoleServe  = "serve"
	RoleRunner = "runner"
)

// DaemonInfo is the answer of GET /v1/whoami on a daemon.
// Device is set only on a call with a credential. Group is empty before the machine has a group.
type DaemonInfo struct {
	Machine   string         `json:"machine"`
	Version   string         `json:"version"`
	Contract  int            `json:"contract"`
	Roles     []string       `json:"roles"`
	Runtimes  []string       `json:"runtimes"`
	Harnesses []HarnessHooks `json:"harnesses"`
	Group     string         `json:"group,omitempty"`
	Home      string         `json:"home,omitempty"`
	Device    string         `json:"device,omitempty"`
	Update    *UpdateStatus  `json:"update,omitempty"`
	Listen    DaemonListen   `json:"listen"`
}

// UpdateStatus is the last self-update check of a daemon.
type UpdateStatus struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest,omitempty"`
	Installed string    `json:"installed,omitempty"` // the daemon restarts with it
	Error     string    `json:"error,omitempty"`
}

// DaemonListen holds host:port addresses.
type DaemonListen struct {
	Loopback  string `json:"loopback"`
	Tailscale string `json:"tailscale,omitempty"`
}

// DeviceKind: DeviceDaemon is a machine of the group, DeviceApp an app on a machine without a daemon.
type DeviceKind string

const (
	DeviceDaemon DeviceKind = "daemon"
	DeviceApp    DeviceKind = "app"
)

// GroupMember is one machine (daemon) of the group.
type GroupMember struct {
	Machine  string     `json:"machine"`
	Host     string     `json:"host"`
	Addrs    []string   `json:"addrs"`
	Port     int        `json:"port"`
	Roles    []string   `json:"roles"`
	JoinedAt time.Time  `json:"joinedAt"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
	Version  string     `json:"version,omitempty"` // the CLI version; each member writes its own entry
}

// GroupDevice is one credential holder: a daemon or an app.
type GroupDevice struct {
	DeviceID string     `json:"deviceId"`
	Name     string     `json:"name"`
	Kind     DeviceKind `json:"kind"`
	Machine  string     `json:"machine"`
	IssuedBy string     `json:"issuedBy"`
	IssuedAt time.Time  `json:"issuedAt"`
}

type RemovedMember struct {
	Machine string    `json:"machine"`
	At      time.Time `json:"at"`
}

// GroupCloud is the cloud hub of a group.
type GroupCloud struct {
	Hub     string `json:"hub"`
	Account string `json:"account"`
}

// Group is the "My machines" group. It never holds the secret. Only a JoinResult holds it.
type Group struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Home    string          `json:"home"`
	Version int             `json:"version"`
	Members []GroupMember   `json:"members"`
	Devices []GroupDevice   `json:"devices"`
	Revoked []string        `json:"revoked"`
	Removed []RemovedMember `json:"removed"`
	Cloud   *GroupCloud     `json:"cloud,omitempty"`
}

// GroupInvite is a join code that a member made.
type GroupInvite struct {
	Code      string    `json:"code"`
	Via       string    `json:"via"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// JoinRequest is the body of POST /v1/group/join and POST /v1/group/requests.
// Code is empty for a request that waits for approval.
type JoinRequest struct {
	Code    string     `json:"code,omitempty"`
	Machine string     `json:"machine"`
	Host    string     `json:"host"`
	Addrs   []string   `json:"addrs,omitempty"`
	Port    int        `json:"port,omitempty"`
	Roles   []string   `json:"roles,omitempty"`
	Kind    DeviceKind `json:"kind"`
	Nonce   string     `json:"nonce"`
}

// JoinPending is a join request that waits for approval. Both sides show Confirm.
type JoinPending struct {
	Request   string    `json:"request"`
	Confirm   string    `json:"confirm"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// DeviceCredential holds a token that is opaque to clients.
type DeviceCredential struct {
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
}

// JoinResult is an accepted join. Only a result for DeviceDaemon holds Secret (base64 of 32 bytes).
type JoinResult struct {
	Group      Group            `json:"group"`
	Secret     string           `json:"secret,omitempty"`
	Credential DeviceCredential `json:"credential"`
}

// QuestionVia has the same values as SessionQuestion.Via in the app.
type QuestionVia string

const (
	ViaForm       QuestionVia = "form"
	ViaPermission QuestionVia = "permission"
	ViaTurn       QuestionVia = "turn"
	ViaWaiting    QuestionVia = "waiting"
)

// QuestionSource tells where the items of a question came from.
type QuestionSource string

const (
	SourceHook       QuestionSource = "hook"
	SourceScreen     QuestionSource = "screen"
	SourceTranscript QuestionSource = "transcript"
	SourceMail       QuestionSource = "mail" // an ask of an old Enhance worker; not sent
)

// QuestionApprove is for daemon questions only. Its options are Allow and Deny.
const QuestionApprove QuestionKind = "approve"

// Option labels of a QuestionApprove item.
const (
	OptionAllow = "Allow"
	OptionDeny  = "Deny"
)

// DaemonQuestionItem has the shape of RunnerQuestionItem. Its kind can also be QuestionApprove.
type DaemonQuestionItem RunnerQuestionItem

// DaemonQuestion is one open pause of a session.
type DaemonQuestion struct {
	ID         string               `json:"id"`
	Session    string               `json:"session"`
	Terminal   string               `json:"terminal,omitempty"`
	Harness    string               `json:"harness"`
	Via        QuestionVia          `json:"via"`
	Items      []DaemonQuestionItem `json:"items"`
	Answerable bool                 `json:"answerable"`
	Source     QuestionSource       `json:"source"`
	AskedAt    time.Time            `json:"askedAt"`
	ExpiresAt  *time.Time           `json:"expiresAt,omitempty"`
}

// DaemonQuestionsPage is the answer of GET /v1/questions.
type DaemonQuestionsPage struct {
	Questions []DaemonQuestion `json:"questions"`
}

// AnswerOutcome is the result of an answer.
type AnswerOutcome string

const (
	AnswerSent            AnswerOutcome = "sent"
	AnswerAlreadyAnswered AnswerOutcome = "alreadyAnswered"
	AnswerGone            AnswerOutcome = "gone"
	AnswerError           AnswerOutcome = "error"
)

// DaemonAnswerResult is the answer of POST /v1/questions/{id}/answer. The body of the call is a QuestionAnswer (RunnerAnswer).
// Code and Message are set with AnswerError.
type DaemonAnswerResult struct {
	Result  AnswerOutcome `json:"result"`
	Code    string        `json:"code,omitempty"`
	Message string        `json:"message,omitempty"`
}

// CodeFormChanged: the form in the terminal changed after the question was read.
const CodeFormChanged = "form_changed"

// Limits and defaults of a SessionAsk and a SessionAskReply, in characters and seconds.
const (
	MaxAskText        = 8000
	MaxAskReply       = 16000
	MaxAskScreenTail  = 8000
	DefaultAskTimeout = 180
	MaxAskTimeout     = 600
)

// AskReadOnlyLine is the line that a read-only ask adds to the text.
const AskReadOnlyLine = "Answer only. Do not change files."

// SessionAsk is the body of POST /v1/sessions/{id}/ask.
type SessionAsk struct {
	Text       string `json:"text"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	ReadOnly   *bool  `json:"readOnly,omitempty"`
}

// Check fails on an empty or long text and on a negative timeout.
func (a SessionAsk) Check() error {
	n := len([]rune(a.Text))
	switch {
	case strings.TrimSpace(a.Text) == "":
		return errors.New("text is empty")
	case n > MaxAskText:
		return fmt.Errorf("text has %d characters, the limit is %d", n, MaxAskText)
	case a.TimeoutSec < 0:
		return errors.New("timeoutSec is negative")
	}
	return nil
}

// Timeout is TimeoutSec, else the default, at most MaxAskTimeout.
func (a SessionAsk) Timeout() time.Duration {
	sec := a.TimeoutSec
	if sec <= 0 {
		sec = DefaultAskTimeout
	}
	return time.Duration(min(sec, MaxAskTimeout)) * time.Second
}

// Prompt is the text for the agent. Read-only is the default: it adds AskReadOnlyLine.
func (a SessionAsk) Prompt() string {
	if a.ReadOnly != nil && !*a.ReadOnly {
		return a.Text
	}
	return strings.TrimRight(a.Text, "\n") + "\n" + AskReadOnlyLine
}

// Values of SessionAskReply.Error. Open set.
const (
	AskBusy        = "busy"        // 409: a turn is active and the runtime takes no steering
	AskTimeout     = "timeout"     // 504
	AskBadRequest  = "badRequest"  // 400
	AskNoSession   = "noSession"   // 404
	AskUnsupported = "unsupported" // 409: the session has no input path
	AskSendFailed  = "sendFailed"  // 502
	AskGone        = "gone"        // 410: the session went away during the wait
)

// SessionAskReply is the answer of POST /v1/sessions/{id}/ask. 200 has Reply, an error status has Error and Message.
type SessionAskReply struct {
	Reply       string     `json:"reply,omitempty"`
	TurnEndedAt *time.Time `json:"turnEndedAt,omitempty"`
	ScreenTail  string     `json:"screenTail,omitempty"`
	Error       string     `json:"error,omitempty"`
	Message     string     `json:"message,omitempty"`
}

// LeaseRequest is the body of POST /v1/leases/{name} on the home daemon.
type LeaseRequest struct {
	Holder     string `json:"holder"` // the store device id of the app
	HolderName string `json:"holderName"`
	TTLSeconds int    `json:"ttlSeconds"`
	Take       bool   `json:"take"` // take the lease also when another holder has it
}

// Limits of LeaseRequest.TTLSeconds.
const (
	LeaseMinTTL = 30
	LeaseMaxTTL = 600
)

// Lease is one named lease: one holder until Until. Epoch increases at each new holder.
type Lease struct {
	Name       string    `json:"name"`
	Holder     string    `json:"holder"`
	HolderName string    `json:"holderName"`
	Epoch      int       `json:"epoch"`
	Until      time.Time `json:"until"`
}
