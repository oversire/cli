package wire

import "time"

// Review makes a step a review. Verdict gaps runs the Fix step, then the review again, up to Rounds more times.
type Review struct {
	Fix    string `json:"fix"`
	Rounds int    `json:"rounds"`
}

type RunbookStep struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Agent       string  `json:"agent"` // an OverSire agent name, a key of the agents of the start
	Prompt      string  `json:"prompt"`
	PauseBefore bool    `json:"pauseBefore,omitempty"`
	Review      *Review `json:"review,omitempty"`
}

// Runbook is a fixed list of steps. The app keeps it in config.json; a run keeps a full copy.
type Runbook struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Steps []RunbookStep `json:"steps"`
}

// Step gives the step with the id, else nil.
func (b *Runbook) Step(id string) *RunbookStep {
	for i := range b.Steps {
		if b.Steps[i].ID == id {
			return &b.Steps[i]
		}
	}
	return nil
}

// Index gives the place of the step, else -1.
func (b Runbook) Index(id string) int {
	for i, s := range b.Steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// AgentLaunch is one agent, resolved by the app at the start. The fields go into LaunchRequest.
type AgentLaunch struct {
	CLI     string `json:"cli"`
	Command string `json:"command,omitempty"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Profile string `json:"profile"`
	// Env and EnvSecrets go into LaunchRequest as they are. EnvSecrets holds refs, never values.
	Env        map[string]string         `json:"env,omitempty"`
	EnvSecrets map[string]AgentSecretRef `json:"envSecrets,omitempty"`
}

// RunbookStart is the body of POST /v1/runbook-runs.
type RunbookStart struct {
	Item    ItemRef                `json:"item"`
	Title   string                 `json:"title,omitempty"`
	Repo    string                 `json:"repo"`
	Runtime string                 `json:"runtime,omitempty"` // empty: the first runtime that runs and has workspaces
	Runbook Runbook                `json:"runbook"`
	Agents  map[string]AgentLaunch `json:"agents"`
	StartAt string                 `json:"startAt,omitempty"`
	Notes   string                 `json:"notes,omitempty"`
}

// RunState is the state of a RunbookRun. RunGate waits for approval. RunPaused needs the user.
type RunState string

const (
	RunRunning RunState = "running"
	RunGate    RunState = "gate"
	RunPaused  RunState = "paused"
	RunDone    RunState = "done"
	RunFailed  RunState = "failed"
	RunStopped RunState = "stopped"
)

// Open tells if the run can still go on.
func (s RunState) Open() bool { return s == RunRunning || s == RunGate || s == RunPaused }

// StepState is the state of a StepRun.
type StepState string

const (
	StepWaits    StepState = "waits"
	StepStarting StepState = "starting"
	StepWorking  StepState = "working"
	StepAsks     StepState = "asks"
	StepPass     StepState = "pass"
	StepGaps     StepState = "gaps"
	StepComplete StepState = "done"
	StepSkipped  StepState = "skipped"
	StepBlocked  StepState = "blocked"
	StepEnded    StepState = "ended" // the session ended with no verdict
	StepFailed   StepState = "failed"
)

// Live tells if the session of the step is open.
func (s StepState) Live() bool { return s == StepStarting || s == StepWorking || s == StepAsks }

// StepRun is one round of one step. Round 1 is the first run.
type StepRun struct {
	Step          string      `json:"step"`
	Round         int         `json:"round"`
	State         StepState   `json:"state"`
	Session       *SessionRef `json:"session,omitempty"`
	DaemonSession string      `json:"daemonSession,omitempty"` // the DaemonSession id
	Terminal      string      `json:"terminal,omitempty"`
	Note          string      `json:"note,omitempty"`
	Started       *time.Time  `json:"started,omitempty"`
	Ended         *time.Time  `json:"ended,omitempty"`
}

// RunbookRun is one run of a runbook on one item.
type RunbookRun struct {
	ID      string                 `json:"id"`
	Item    ItemRef                `json:"item"`
	Title   string                 `json:"title,omitempty"`
	Machine string                 `json:"machine"`
	Runtime string                 `json:"runtime"`
	Repo    string                 `json:"repo"`
	Runbook Runbook                `json:"runbook"`
	Agents  map[string]AgentLaunch `json:"agents"`
	Notes   string                 `json:"notes,omitempty"`
	State   RunState               `json:"state"`
	Reason  string                 `json:"reason,omitempty"`
	Current string                 `json:"current,omitempty"`
	Steps   []StepRun              `json:"steps"`
	Started time.Time              `json:"started"`
	Updated time.Time              `json:"updated"`
}

// Verdict is the result that the agent of a step gives.
type Verdict string

const (
	VerdictPass    Verdict = "pass"
	VerdictGaps    Verdict = "gaps"
	VerdictDone    Verdict = "done"
	VerdictSkipped Verdict = "skipped"
	VerdictBlocked Verdict = "blocked"
)

// StepDone is the body of POST /v1/runbook-runs/{id}/steps/{step}/done.
type StepDone struct {
	Verdict Verdict `json:"verdict"`
	Note    string  `json:"note,omitempty"`
}

// StepPatch is the body of PATCH /v1/runbook-runs/{id}/steps/{step}, only for a step that did not start.
type StepPatch struct {
	Agent  string       `json:"agent,omitempty"`
	Launch *AgentLaunch `json:"launch,omitempty"`
	Prompt string       `json:"prompt,omitempty"`
}
