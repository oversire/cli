package wire

import (
	"encoding/json"
	"fmt"
)

// Message is one RunnerMessage on the hub channel.
type Message interface{ Type() string }

type AllowSummary struct {
	Runtimes []string `json:"runtimes"`
	Repos    int      `json:"repos"`
	Agents   []string `json:"agents"`
}

type Hello struct {
	Version  string        `json:"version"`
	Contract int           `json:"contract"`
	Runtimes []string      `json:"runtimes"`
	OS       string        `json:"os,omitempty"`
	Allow    *AllowSummary `json:"allow,omitempty"`
}

type Ack struct {
	Order string `json:"order"`
}

type ResultState string

const (
	ResultDone    ResultState = "done"
	ResultFailed  ResultState = "failed"
	ResultExpired ResultState = "expired"
)

type Result struct {
	Order  string           `json:"order"`
	State  ResultState      `json:"state"`
	Result *WorkOrderResult `json:"result,omitempty"`
}

type Welcome struct {
	Device   string `json:"device"`
	Contract int    `json:"contract"`
}

type Refused struct {
	Reason    string `json:"reason"`
	Contracts []int  `json:"contracts"`
}

type Order struct {
	Order WorkOrder `json:"order"`
}

type Update struct {
	Version string `json:"version"`
}

// Unknown is a message type that this runner does not know. The runner ignores it.
type Unknown struct{ Kind string }

func (Hello) Type() string     { return "hello" }
func (Ack) Type() string       { return "ack" }
func (Result) Type() string    { return "result" }
func (Welcome) Type() string   { return "welcome" }
func (Refused) Type() string   { return "refused" }
func (Order) Type() string     { return "order" }
func (Update) Type() string    { return "update" }
func (u Unknown) Type() string { return u.Kind }

// Encode writes m with its type field.
func Encode(m Message) ([]byte, error) {
	if _, ok := m.(Unknown); ok {
		return nil, fmt.Errorf("wire: cannot encode an unknown message")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["type"], _ = json.Marshal(m.Type())
	return json.Marshal(fields)
}

// DecodeMessage reads one message. A type that the runner does not know gives Unknown.
func DecodeMessage(data []byte) (Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	var kind string
	if err := json.Unmarshal(fields["type"], &kind); err != nil {
		return nil, fmt.Errorf("wire: message has no type")
	}
	delete(fields, "type")
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "hello":
		return decodeAs[Hello](body)
	case "ack":
		return decodeAs[Ack](body)
	case "result":
		return decodeAs[Result](body)
	case "welcome":
		return decodeAs[Welcome](body)
	case "refused":
		return decodeAs[Refused](body)
	case "order":
		return decodeAs[Order](body)
	case "update":
		return decodeAs[Update](body)
	}
	return Unknown{Kind: kind}, nil
}

func decodeAs[T Message](body []byte) (Message, error) {
	var v T
	if err := Decode(body, &v); err != nil {
		return nil, fmt.Errorf("wire: %s: %w", v.Type(), err)
	}
	return v, nil
}
