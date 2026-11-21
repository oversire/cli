package wire

import (
	"slices"
	"time"
)

// The CI data of the mirror feed and the answers of GET /v1/ci. Contract: ci.json.

type CICheckKind string

const (
	CICheckRun    CICheckKind = "checkRun"
	CICheckStatus CICheckKind = "status"
)

type CICheckState string

const (
	CICheckPending   CICheckState = "pending"
	CICheckRunning   CICheckState = "running"
	CICheckSuccess   CICheckState = "success"
	CICheckFailure   CICheckState = "failure"
	CICheckCancelled CICheckState = "cancelled"
	CICheckNeutral   CICheckState = "neutral"
	CICheckSkipped   CICheckState = "skipped"
)

// CISource tells how the app gets the CI data.
type CISource string

const (
	CISourceWebhook CISource = "webhook"
	CISourcePoll    CISource = "poll"
)

type CIRollupState string

const (
	CIPending   CIRollupState = "pending"
	CIRunning   CIRollupState = "running"
	CISuccess   CIRollupState = "success"
	CIFailure   CIRollupState = "failure"
	CICancelled CIRollupState = "cancelled"
	CINone      CIRollupState = "none"
)

// CIRun is one run of a `runs` doc.
type CIRun struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"displayTitle"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion,omitempty"`
	HeadBranch   string `json:"headBranch"`
	HeadSha      string `json:"headSha"`
	Event        string `json:"event"`
	URL          string `json:"url"`
	UpdatedAt    string `json:"updatedAt"`
	CreatedAt    string `json:"createdAt"`
	StartedAt    string `json:"startedAt,omitempty"`
	Number       int    `json:"number"`
	WorkflowName string `json:"workflowName,omitempty"`
	ActorLogin   string `json:"actorLogin,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
}

type CIRunsDoc struct {
	Runs []CIRun `json:"runs"`
}

// CIPullRef holds the fields of a `prs` doc entry that the daemon reads.
type CIPullRef struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	HeadSha     string `json:"headSha,omitempty"`
	URL         string `json:"url"`
}

type CIPRsDoc struct {
	PRs []CIPullRef `json:"prs"`
}

type CIStep struct {
	Name        string `json:"name"`
	Status      string `json:"status,omitempty"`
	Conclusion  string `json:"conclusion,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

type CIJob struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status,omitempty"`
	Conclusion  string   `json:"conclusion,omitempty"`
	URL         string   `json:"url,omitempty"`
	StartedAt   string   `json:"startedAt,omitempty"`
	CompletedAt string   `json:"completedAt,omitempty"`
	RunnerName  string   `json:"runnerName,omitempty"`
	Steps       []CIStep `json:"steps,omitempty"`
}

// CIFailureView tells why a job failed.
type CIFailureView struct {
	Job      string   `json:"job"`
	JobID    string   `json:"jobId"`
	Step     string   `json:"step,omitempty"`
	Messages []string `json:"messages"`
	LogTail  []string `json:"logTail,omitempty"`
	URL      string   `json:"url,omitempty"`
}

// CIJobsDoc is the body of a `ciJobs` doc. The id is <owner>/<repo>/<run>/<attempt>.
type CIJobsDoc struct {
	Repo       string          `json:"repo"`
	Run        string          `json:"run"`
	Attempt    int             `json:"attempt"`
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion,omitempty"`
	Workflow   string          `json:"workflow,omitempty"`
	HeadSha    string          `json:"headSha"`
	HeadBranch string          `json:"headBranch"`
	Event      string          `json:"event"`
	URL        string          `json:"url"`
	Jobs       []CIJob         `json:"jobs"`
	Failures   []CIFailureView `json:"failures"`
	Final      bool            `json:"final"`
	FetchedAt  time.Time       `json:"fetchedAt"`
}

// CICheck is a check that is not Actions.
type CICheck struct {
	Name        string       `json:"name"`
	App         string       `json:"app,omitempty"`
	Kind        CICheckKind  `json:"kind"`
	State       CICheckState `json:"state"`
	Conclusion  string       `json:"conclusion,omitempty"`
	URL         string       `json:"url,omitempty"`
	StartedAt   string       `json:"startedAt,omitempty"`
	CompletedAt string       `json:"completedAt,omitempty"`
}

// CIChecksDoc is the body of a `ciChecks` doc. The id is <owner>/<repo>/<sha>.
type CIChecksDoc struct {
	Repo      string    `json:"repo"`
	Sha       string    `json:"sha"`
	PRs       []int     `json:"prs"`
	Checks    []CICheck `json:"checks"`
	Final     bool      `json:"final"`
	FetchedAt time.Time `json:"fetchedAt"`
}

type CIBudget struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

// CIRepoState is the CI state of one tracked repo.
type CIRepoState struct {
	Repo         string     `json:"repo"`
	CI           string     `json:"ci"`
	Capabilities []string   `json:"capabilities"`
	PolledAt     *time.Time `json:"polledAt,omitempty"`
}

// Has tells if the CI provider of the repo has the capability c.
func (s CIRepoState) Has(c string) bool { return slices.Contains(s.Capabilities, c) }

// CIStateDoc is the body of the one `ciState` doc. The id is home.
type CIStateDoc struct {
	AsOf     time.Time     `json:"asOf"`
	Source   CISource      `json:"source"`
	Degraded bool          `json:"degraded"`
	Budget   *CIBudget     `json:"budget,omitempty"`
	Repos    []CIRepoState `json:"repos"`
}

// CIFreshness is in each answer. The JSON is flat.
type CIFreshness struct {
	AsOf     time.Time `json:"asOf"`
	Source   CISource  `json:"source"`
	Stale    bool      `json:"stale"`
	Degraded bool      `json:"degraded"`
}

// CIRunView is one run in an answer.
type CIRunView struct {
	ID         string          `json:"id"`
	Attempt    int             `json:"attempt"`
	Name       string          `json:"name"`
	Workflow   string          `json:"workflow,omitempty"`
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion,omitempty"`
	Branch     string          `json:"branch"`
	SHA        string          `json:"sha"`
	Event      string          `json:"event"`
	URL        string          `json:"url"`
	StartedAt  string          `json:"startedAt,omitempty"`
	UpdatedAt  string          `json:"updatedAt"`
	DurationS  int             `json:"durationS,omitempty"`
	ETAS       int             `json:"etaS,omitempty"`
	Jobs       []CIJob         `json:"jobs,omitempty"`
	Failures   []CIFailureView `json:"failures,omitempty"`
}

// CIRollup is the answer of GET /v1/ci/status and GET /v1/ci/wait.
type CIRollup struct {
	Repo   string        `json:"repo"`
	Ref    string        `json:"ref"`
	SHA    string        `json:"sha,omitempty"`
	PR     int           `json:"pr,omitempty"`
	State  CIRollupState `json:"state"`
	Done   bool          `json:"done"`
	Rev    string        `json:"rev"`
	Runs   []CIRunView   `json:"runs"`
	Checks []CICheck     `json:"checks"`
	CIFreshness
}

type CIRunsPage struct {
	Repo string      `json:"repo"`
	Runs []CIRunView `json:"runs"`
	CIFreshness
}

type CIRunAnswer struct {
	Run CIRunView `json:"run"`
	CIFreshness
}

type CIFailuresAnswer struct {
	Repo     string          `json:"repo"`
	Run      string          `json:"run"`
	Attempt  int             `json:"attempt"`
	Failures []CIFailureView `json:"failures"`
	CIFreshness
}

// CILogView is the tail of a job log.
type CILogView struct {
	Repo      string   `json:"repo"`
	JobID     string   `json:"jobId"`
	Run       string   `json:"run,omitempty"`
	Lines     []string `json:"lines"`
	Complete  bool     `json:"complete"`
	Truncated bool     `json:"truncated"`
	Cached    bool     `json:"cached"`
	CIFreshness
}

type CIWorkflow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	State string `json:"state,omitempty"`
}

type CIWorkflows struct {
	Repo      string       `json:"repo"`
	Workflows []CIWorkflow `json:"workflows"`
	CIFreshness
}

// CIActionRequest is the args of the write actions (later phase).
type CIActionRequest struct {
	Repo         string            `json:"repo"`
	Run          string            `json:"run,omitempty"`
	Job          string            `json:"job,omitempty"`
	FailedOnly   bool              `json:"failedOnly,omitempty"`
	Workflow     string            `json:"workflow,omitempty"`
	Ref          string            `json:"ref,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	Environments []string          `json:"environments,omitempty"`
	Comment      string            `json:"comment,omitempty"`
	Agent        string            `json:"agent,omitempty"`
}

// CIActionResult is the data of an ok result of a write action (later phase).
type CIActionResult struct {
	OK      bool       `json:"ok"`
	Noop    bool       `json:"noop"`
	Summary string     `json:"summary"`
	Run     *CIRunView `json:"run,omitempty"`
	URL     string     `json:"url,omitempty"`
}

// CIExplainRequest is the body of POST /v1/ci/runs/{id}/explain and the args of the app action ci.explain.
type CIExplainRequest struct {
	Repo    string `json:"repo"`
	Attempt int    `json:"attempt,omitempty"`
	Job     string `json:"job,omitempty"`
	Force   bool   `json:"force,omitempty"`
	// Agent is "<harness> <session>".
	Agent string `json:"agent,omitempty"`
}

// CIExplanation is the cause of a failed run. Source is "cache" or "ai".
type CIExplanation struct {
	Repo       string    `json:"repo"`
	Run        string    `json:"run"`
	Attempt    int       `json:"attempt"`
	Job        string    `json:"job,omitempty"`
	Cause      string    `json:"cause"`
	Fix        string    `json:"fix"`
	Confidence string    `json:"confidence"`
	At         time.Time `json:"at"`
	Source     string    `json:"source"`
}

// CIWatch is one commit that an agent waits on. The home daemon keeps the list and sends it to the source app.
type CIWatch struct {
	Repo      string     `json:"repo"`
	Ref       string     `json:"ref"`
	TTL       int        `json:"ttl"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	ID        string     `json:"id,omitempty"`
	SHA       string     `json:"sha,omitempty"`
	Branch    string     `json:"branch,omitempty"`
	PR        int        `json:"pr,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Machine   string     `json:"machine,omitempty"`
}

// CINotifyTarget is the agent session that gets the result of a wait.
type CINotifyTarget struct {
	Session        string `json:"session,omitempty"`
	PaneKey        string `json:"paneKey,omitempty"`
	Harness        string `json:"harness,omitempty"`
	HarnessSession string `json:"harnessSession,omitempty"`
}

// CIWatchRequest is the body of POST /v1/ci/watches and POST /v1/ci/notify.
type CIWatchRequest struct {
	Repo   string          `json:"repo"`
	Ref    string          `json:"ref"`
	TTL    int             `json:"ttl,omitempty"`
	Notify *CINotifyTarget `json:"notify,omitempty"`
}

// CIWatchAnswer is the answer of POST /v1/ci/watches and POST /v1/ci/notify.
type CIWatchAnswer struct {
	Watch  *CIWatch  `json:"watch,omitempty"`
	Rollup *CIRollup `json:"rollup,omitempty"`
	Done   bool      `json:"done"`
	Notify string    `json:"notify,omitempty"`
}

// CIFrame is a frame of GET /v1/connect with the full watch list.
type CIFrame struct {
	Type    string    `json:"type"`
	Event   string    `json:"event"`
	Watches []CIWatch `json:"watches"`
}
