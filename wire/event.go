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
