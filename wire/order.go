package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// ContractVersion is the x-oversire-schema number of work-order.json.
const ContractVersion = 1

type Op string

const (
	OpSessionStart Op = "sessionStart"
	OpSessionStop  Op = "sessionStop"
	OpSessionRead  Op = "sessionRead"
	OpSessionSend  Op = "sessionSend"
	// OpSessionHistory reads the history of a session as a SessionDigest.
	OpSessionHistory Op = "sessionHistory"
)

type State string

// A state that the runner does not know keeps its raw value. The hub never sends a held order.
const (
	StateHeld    State = "held"
	StateQueued  State = "queued"
	StateSent    State = "sent"
	StateAcked   State = "acked"
	StateDone    State = "done"
	StateFailed  State = "failed"
	StateExpired State = "expired"
)

// Payload is the payload of one op: LaunchRequest, StopPayload, ReadPayload, SendPayload or HistoryPayload.
type Payload interface{ Op() Op }

type StopPayload struct {
	Session SessionRef `json:"session"`
}

type ReadPayload struct {
	Session SessionRef `json:"session"`
	Lines   int        `json:"lines"`
}

// SendPayload carries a user message or a permission answer. With a permission, Text is "".
type SendPayload struct {
	Session    SessionRef        `json:"session"`
	Text       string            `json:"text"`
	Permission *PermissionAnswer `json:"permission,omitempty"`
}

// PermissionAnswer answers the PermissionPrompt with the same id.
type PermissionAnswer struct {
	ID    string `json:"id"`
	Allow bool   `json:"allow"`
}

func (LaunchRequest) Op() Op { return OpSessionStart }
func (StopPayload) Op() Op   { return OpSessionStop }
func (ReadPayload) Op() Op   { return OpSessionRead }
func (SendPayload) Op() Op   { return OpSessionSend }

type WorkOrderResult struct {
	Session *SessionRef    `json:"session,omitempty"`
	Lines   []string       `json:"lines,omitempty"`
	Digest  *SessionDigest `json:"digest,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// WorkOrderHold says why the hub holds an order.
type WorkOrderHold struct {
	Blockers []ItemRef `json:"blockers"`
	Areas    []string  `json:"areas"`
	Since    time.Time `json:"since"`
}

// WorkOrder is a typed order from the hub. The op comes from the payload type.
type WorkOrder struct {
	ID         string
	Runner     string
	Runtime    string
	Payload    Payload
	Suggestion string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	State      State
	Hold       *WorkOrderHold
	Result     *WorkOrderResult
}

type orderJSON struct {
	ID         string           `json:"id"`
	Runner     string           `json:"runner"`
	Runtime    string           `json:"runtime"`
	Op         Op               `json:"op"`
	Payload    json.RawMessage  `json:"payload"`
	Suggestion string           `json:"suggestion"`
	CreatedAt  time.Time        `json:"createdAt"`
	ExpiresAt  time.Time        `json:"expiresAt"`
	State      State            `json:"state"`
	Hold       *WorkOrderHold   `json:"hold,omitempty"`
	Result     *WorkOrderResult `json:"result,omitempty"`
}

func (o WorkOrder) MarshalJSON() ([]byte, error) {
	if o.Payload == nil {
		return nil, fmt.Errorf("wire: order %s has no payload", o.ID)
	}
	p, err := json.Marshal(o.Payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(orderJSON{
		ID: o.ID, Runner: o.Runner, Runtime: o.Runtime, Op: o.Payload.Op(), Payload: p,
		Suggestion: o.Suggestion, CreatedAt: o.CreatedAt, ExpiresAt: o.ExpiresAt, State: o.State, Hold: o.Hold, Result: o.Result,
	})
}

func (o *WorkOrder) UnmarshalJSON(data []byte) error {
	var j orderJSON
	if err := Decode(data, &j); err != nil {
		return err
	}
	if j.ID == "" || j.Runner == "" || j.Suggestion == "" {
		return fmt.Errorf("wire: order needs id, runner and suggestion")
	}
	var p Payload
	var err error
	switch j.Op {
	case OpSessionStart:
		var v LaunchRequest
		err = Decode(j.Payload, &v)
		p = v
	case OpSessionStop:
		var v StopPayload
		err = Decode(j.Payload, &v)
		p = v
	case OpSessionRead:
		var v ReadPayload
		err = Decode(j.Payload, &v)
		p = v
	case OpSessionSend:
		var v SendPayload
		err = Decode(j.Payload, &v)
		p = v
	case OpSessionHistory:
		var v HistoryPayload
		err = Decode(j.Payload, &v)
		p = v
	default:
		return fmt.Errorf("wire: unknown op %q", j.Op)
	}
	if err != nil {
		return fmt.Errorf("wire: %s payload: %w", j.Op, err)
	}
	*o = WorkOrder{
		ID: j.ID, Runner: j.Runner, Runtime: j.Runtime, Payload: p, Suggestion: j.Suggestion,
		CreatedAt: j.CreatedAt, ExpiresAt: j.ExpiresAt, State: j.State, Hold: j.Hold, Result: j.Result,
	}
	return nil
}
