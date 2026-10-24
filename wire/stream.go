package wire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"
)

// The live session stream: GET /v1/sessions/{id}/stream. Contract: session-stream.json.
// The JSON frames are Messages: Encode writes them with their type field.

type StreamView string

const (
	ViewTerminal StreamView = "terminal"
	ViewChat     StreamView = "chat"
)

type StreamSource string

const (
	SourceStream StreamSource = "stream"
	SourcePoll   StreamSource = "poll"
	SourceWire   StreamSource = "wire"
)

type LeaseHolder string

const (
	HolderYou     LeaseHolder = "you"
	HolderOther   LeaseHolder = "other"
	HolderDesktop LeaseHolder = "desktop"
	HolderNone    LeaseHolder = "none"
)

type TurnRole string

const (
	RoleUser      TurnRole = "user"
	RoleAssistant TurnRole = "assistant"
	RoleTool      TurnRole = "tool"
	RoleSystem    TurnRole = "system"
)

type ToolStatus string

const (
	ToolRunning ToolStatus = "running"
	ToolDone    ToolStatus = "done"
	ToolFailed  ToolStatus = "failed"
)

// TurnTool is the tool call of a tool turn.
type TurnTool struct {
	Name    string     `json:"name"`
	Status  ToolStatus `json:"status"`
	Input   string     `json:"input,omitempty"`   // at most 200 characters
	Summary string     `json:"summary,omitempty"` // at most 2000 characters
}

// SessionTurn is one turn of the chat view. A tool turn that changes state comes again with the same Seq.
type SessionTurn struct {
	Seq  int       `json:"seq"`
	Role TurnRole  `json:"role"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Tool *TurnTool `json:"tool,omitempty"`
}

// Client frames.

type StreamAttach struct {
	View  StreamView `json:"view"`
	Cols  int        `json:"cols,omitempty"`
	Rows  int        `json:"rows,omitempty"`
	After *int       `json:"after,omitempty"`
}

type StreamAck struct {
	Seq uint64 `json:"seq"`
}

type StreamControl struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type StreamRelease struct{}

type StreamSend struct {
	Text string `json:"text"`
}

type StreamInterrupt struct{}

// Daemon frames.

type StreamAttached struct {
	Source    StreamSource `json:"source"`
	Cols      int          `json:"cols"`
	Rows      int          `json:"rows"`
	Writable  bool         `json:"writable"`
	Resizable bool         `json:"resizable"`
	Title     string       `json:"title,omitempty"`
	Reason    string       `json:"reason,omitempty"` // why the source is poll
}

type StreamResized struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type StreamLease struct {
	Holder LeaseHolder `json:"holder"`
}

type StreamTurns struct {
	From  int           `json:"from"`
	To    int           `json:"to"`
	Turns []SessionTurn `json:"turns"`
}

type StreamEnded struct {
	Reason string `json:"reason"`
}

type StreamError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (StreamAttach) Type() string    { return "attach" }
func (StreamAck) Type() string       { return "ack" }
func (StreamControl) Type() string   { return "control" }
func (StreamRelease) Type() string   { return "release" }
func (StreamSend) Type() string      { return "send" }
func (StreamInterrupt) Type() string { return "interrupt" }
func (StreamAttached) Type() string  { return "attached" }
func (StreamResized) Type() string   { return "resized" }
func (StreamLease) Type() string     { return "lease" }
func (StreamTurns) Type() string     { return "turns" }
func (StreamEnded) Type() string     { return "ended" }
func (StreamError) Type() string     { return "error" }

// DecodeClientFrame reads one JSON frame from a client. An unknown type is an error.
func DecodeClientFrame(data []byte) (Message, error) {
	kind, body, err := splitType(data)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "attach":
		return decodeAs[StreamAttach](body)
	case "ack":
		return decodeAs[StreamAck](body)
	case "control":
		return decodeAs[StreamControl](body)
	case "release":
		return decodeAs[StreamRelease](body)
	case "send":
		return decodeAs[StreamSend](body)
	case "interrupt":
		return decodeAs[StreamInterrupt](body)
	}
	return nil, fmt.Errorf("wire: unknown frame type %q", kind)
}

// DecodeDaemonFrame reads one JSON frame from a daemon. An unknown type gives Unknown.
func DecodeDaemonFrame(data []byte) (Message, error) {
	kind, body, err := splitType(data)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "attached":
		return decodeAs[StreamAttached](body)
	case "resized":
		return decodeAs[StreamResized](body)
	case "lease":
		return decodeAs[StreamLease](body)
	case "turns":
		return decodeAs[StreamTurns](body)
	case "ended":
		return decodeAs[StreamEnded](body)
	case "error":
		return decodeAs[StreamError](body)
	}
	return Unknown{Kind: kind}, nil
}

func splitType(data []byte) (string, []byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", nil, err
	}
	var kind string
	if err := json.Unmarshal(fields["type"], &kind); err != nil {
		return "", nil, fmt.Errorf("wire: message has no type")
	}
	delete(fields, "type")
	body, err := json.Marshal(fields)
	return kind, body, err
}

// Binary frames: op (1 byte) | seq (8 bytes, big endian) | data.

type StreamOp byte

const (
	OpSnapshot StreamOp = 1
	OpOutput   StreamOp = 2
	OpInput    StreamOp = 3
)

const binaryHeader = 9

// EncodeBinary gives one binary frame.
func EncodeBinary(op StreamOp, seq uint64, data []byte) []byte {
	out := make([]byte, binaryHeader+len(data))
	out[0] = byte(op)
	binary.BigEndian.PutUint64(out[1:binaryHeader], seq)
	copy(out[binaryHeader:], data)
	return out
}

// DecodeBinary splits one binary frame. data shares the memory of b.
func DecodeBinary(b []byte) (op StreamOp, seq uint64, data []byte, err error) {
	if len(b) < binaryHeader {
		return 0, 0, nil, fmt.Errorf("wire: binary frame of %d bytes", len(b))
	}
	op = StreamOp(b[0])
	if op < OpSnapshot || op > OpInput {
		return 0, 0, nil, fmt.Errorf("wire: unknown binary op %d", op)
	}
	return op, binary.BigEndian.Uint64(b[1:binaryHeader]), b[binaryHeader:], nil
}
