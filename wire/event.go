package wire

import (
	"encoding/json"
	"time"
)

// ProviderEvent is a hint that a subject changed (provider-event.json).
type ProviderEvent struct {
	ID       string        `json:"id"`
	Schema   int           `json:"schema"`
	Provider string        `json:"provider"`
	Account  AccountID     `json:"account"`
	Kind     string        `json:"kind"`
	Action   string        `json:"action"`
	Subject  SubjectRef    `json:"subject"`
	At       time.Time     `json:"at"`
	Summary  *EventSummary `json:"summary,omitempty"`
}

type EventSummary struct {
	Title      string `json:"title,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Actor      string `json:"actor,omitempty"`
}

// EventsPage is the answer of GET /v1/events?after=<seq>.
type EventsPage struct {
	Events    []ProviderEvent `json:"events"`
	Head      int64           `json:"head"`
	Next      int64           `json:"next"`
	More      bool            `json:"more"`
	Truncated bool            `json:"truncated"`
}

func SessionSubject(r SessionRef) SubjectRef {
	b, _ := json.Marshal(r)
	return SubjectRef{Kind: "session", Ref: b}
}

// ActorType says who did it (event.json).
type ActorType string

const (
	ActorHuman    ActorType = "human"
	ActorAgent    ActorType = "agent"
	ActorSystem   ActorType = "system"
	ActorProvider ActorType = "provider"
)

const (
	MaxEventSummary = 200
	MaxEventPayload = 4096
)

// EventDraft is one row of the events table before the home daemon gets it (event.json).
type EventDraft struct {
	ID         string          `json:"id"`
	At         time.Time       `json:"at"`
	Kind       string          `json:"kind"`
	Item       string          `json:"item,omitempty"`
	ParentItem string          `json:"parentItem,omitempty"`
	Project    string          `json:"project,omitempty"`
	Repo       string          `json:"repo,omitempty"`
	Machine    string          `json:"machine,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	Session    string          `json:"session,omitempty"`
	Corr       string          `json:"corr,omitempty"`
	ActorType  ActorType       `json:"actorType"`
	Actor      string          `json:"actor"`
	OnBehalf   string          `json:"onBehalf,omitempty"`
	Via        string          `json:"via"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Private    bool            `json:"private,omitempty"`
}

// Event is one row of the events table: a draft plus its place on the daemon that answers.
type Event struct {
	Seq    int64     `json:"seq"`
	SeenAt time.Time `json:"seenAt"`
	EventDraft
}

// EventQuery is the URL query of GET /v1/items/{key}/events.
type EventQuery struct {
	Item  string
	Tree  bool
	After int64
	Kinds []string
	Limit int
}

// EventPage is the answer of GET /v1/items/{key}/events.
type EventPage struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
	Head   int64   `json:"head"`
	More   bool    `json:"more"`
}

// EventAppend is the body of POST /v1/events.
type EventAppend struct {
	Events []EventDraft `json:"events"`
}

type EventAppendResult struct {
	Accepted int   `json:"accepted"`
	Head     int64 `json:"head"`
}

// ItemLink is the parent of an item, from the tracker. An empty parent: no parent now.
type ItemLink struct {
	Item   string `json:"item"`
	Parent string `json:"parent,omitempty"`
}

// ItemLinks is the body of POST /v1/events/links.
type ItemLinks struct {
	Links []ItemLink `json:"links"`
}

// The kinds of contract/schema/event-kinds.json that the daemon writes.
const (
	KindSessionStarted     = "session.started"
	KindSessionEnded       = "session.ended"
	KindTurnStarted        = "turn.started"
	KindTurnEnded          = "turn.ended"
	KindTurnInterrupted    = "turn.interrupted"
	KindWaitStarted        = "wait.started"
	KindWaitEnded          = "wait.ended"
	KindContextCompacted   = "context.compacted"
	KindSubagentEnded      = "subagent.ended"
	KindReviewReady        = "review.ready"
	KindPromptSubmitted    = "prompt.submitted"
	KindQuestionAnswered   = "question.answered"
	KindReviewSubmitted    = "review.submitted"
	KindReviewApproved     = "review.approved"
	KindWorktreeAdded      = "worktree.added"
	KindWorktreeRemoved    = "worktree.removed"
	KindGitCommit          = "git.commit"
	KindGitRebase          = "git.rebase"
	KindGitReset           = "git.reset"
	KindGitMerge           = "git.merge"
	KindGitPush            = "git.push"
	KindQueuePicked        = "queue.picked"
	KindQueueHeld          = "queue.held"
	KindRunbookStepStarted = "runbook.step_started"
	KindRunbookStepEnded   = "runbook.step_ended"
	KindAutomationRan      = "automation.ran"
)
