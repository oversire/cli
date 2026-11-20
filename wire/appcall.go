package wire

import "encoding/json"

// AppCall is one action that a CLI command asks the app on this Mac to run (app-call.json).
// Agent is empty when a human ran the command.
type AppCall struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args"`
	Agent  string          `json:"agent,omitempty"`
}

type AppCallStatus string

const (
	AppCallOK         AppCallStatus = "ok"
	AppCallFailed     AppCallStatus = "failed"
	AppCallNotAllowed AppCallStatus = "notAllowed"
)

// AppCallResult is the answer of the app. Data is the JSON result of a read action.
type AppCallResult struct {
	ID      string          `json:"id"`
	Status  AppCallStatus   `json:"status"`
	Summary string          `json:"summary"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// FrameAppCall is the type of AppCallFrame on GET /v1/connect.
const FrameAppCall = "appCall"

type AppCallFrame struct {
	Type string  `json:"type"`
	Call AppCall `json:"call"`
}

// MemberCaller is the app that sent a member call. The daemon fills it from the credential of the request.
type MemberCaller struct {
	Machine string `json:"machine"`
	Name    string `json:"name"`
	Device  string `json:"device,omitempty"`
}

// MemberCallRequest is the body of POST /v1/member-calls.
type MemberCallRequest struct {
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args"`
}

// MemberCall is one action of a client app for the source app of the mirror (app-call.json).
// The answer is AppCallResult.
type MemberCall struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args"`
	From   MemberCaller    `json:"from"`
}

// FrameMemberCall is the type of MemberCallFrame on GET /v1/connect.
const FrameMemberCall = "memberCall"

type MemberCallFrame struct {
	Type string     `json:"type"`
	Call MemberCall `json:"call"`
}
