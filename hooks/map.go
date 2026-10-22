package hooks

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// payload holds the hook fields that the mapping reads. Claude and Codex use the same names.
type payload struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	Source               string          `json:"source"`
	Prompt               string          `json:"prompt"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	NotificationType     string          `json:"notification_type"`
	LastAssistantMessage *string         `json:"last_assistant_message"`
	AgentID              string          `json:"agent_id"`
	IsInterrupt          bool            `json:"is_interrupt"`
}

func readPayload(raw []byte) payload {
	var p payload
	_ = json.Unmarshal(raw, &p)
	return p
}

// maxToolInput is the length of the short text of a tool input, in characters.
const maxToolInput = 300

// eventMap turns the hook events of one harness into patches. It follows Orca claude-events.ts and codex-events.ts.
type eventMap struct {
	harness   string
	questions []string // tools that ask the user
	plan      string   // the tool that asks for plan approval
}

func (m eventMap) patch(ev Event) (sessions.Patch, bool) {
	p := readPayload(ev.Payload)
	name := ev.Name
	if name == "" {
		name = p.HookEventName
	}
	out, ok := m.identity(ev, p)
	if !ok {
		return out, false
	}
	question := slices.Contains(m.questions, p.ToolName)
	// A subagent event changes the session only when it waits for the user.
	if p.AgentID != "" && !(name == "PermissionRequest" || name == "PreToolUse" && question) {
		return out, false
	}
	switch name {
	case "SessionStart":
		// A compact restart runs inside a turn.
		if p.Source == "compact" {
			return out, false
		}
		set(&out, wire.PhaseDone, wire.ReasonTurnEnd)
	case "UserPromptSubmit":
		set(&out, wire.PhaseWorking, "")
		out.Prompt = &p.Prompt
	case "PreToolUse":
		switch {
		case question:
			set(&out, wire.PhaseWaiting, wire.ReasonQuestion)
			m.tool(&out, p)
		case m.plan != "" && p.ToolName == m.plan:
			set(&out, wire.PhaseWaiting, wire.ReasonPermission)
			m.tool(&out, p)
		default:
			set(&out, wire.PhaseWorking, "")
		}
	case "PostToolUse":
		set(&out, wire.PhaseWorking, "")
	case "PermissionRequest":
		if question {
			set(&out, wire.PhaseWaiting, wire.ReasonQuestion)
		} else {
			set(&out, wire.PhaseWaiting, wire.ReasonPermission)
		}
		m.tool(&out, p)
	case "Notification":
		if p.NotificationType == "permission_prompt" {
			set(&out, wire.PhaseWaiting, wire.ReasonPermission)
		}
	case "Stop":
		reason := wire.ReasonTurnEnd
		if p.IsInterrupt {
			reason = wire.ReasonInterrupted
		}
		set(&out, wire.PhaseDone, reason)
		out.LastMessage = p.LastAssistantMessage
	case "Interrupt":
		set(&out, wire.PhaseDone, wire.ReasonInterrupted)
	case "StopFailure":
		set(&out, wire.PhaseDone, wire.ReasonError)
	case "SessionEnd":
		out.End = true
	default:
		return out, false
	}
	return out, true
}

// identity gives the session fields. An Orca pane is the session id; else the harness session id is.
func (m eventMap) identity(ev Event, p payload) (sessions.Patch, bool) {
	out := sessions.Patch{Source: sessions.SourceHook, At: ev.At, Harness: ptr(m.harness)}
	if p.SessionID != "" {
		out.HarnessSession = ptr(p.SessionID)
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = ev.Env.Pwd
	}
	if e := ev.Env; e.PaneKey != "" {
		out.ID, out.Runtime = e.PaneKey, ptr(wire.RuntimeOrca)
		if e.WorktreeID != "" {
			out.Workspace = ptr(e.WorktreeID)
		}
		if e.TerminalHandle != "" {
			out.Terminal = ptr(e.TerminalHandle)
		}
		if e.WorktreePath != "" {
			cwd = e.WorktreePath
		}
	} else {
		if p.SessionID == "" {
			return out, false
		}
		out.ID, out.Runtime = "cli:"+p.SessionID, ptr(wire.RuntimeCLI)
	}
	if cwd != "" {
		out.Cwd = ptr(cwd)
	}
	return out, true
}

func (m eventMap) tool(out *sessions.Patch, p payload) {
	out.Tool = ptr(p.ToolName)
	out.ToolInput = ptr(toolInputText(p.ToolInput))
}

// set gives the state and the reason. An empty reason clears it.
func set(out *sessions.Patch, state wire.SessionPhase, reason wire.SessionReason) {
	out.State, out.Reason = &state, &reason
}

// toolInputText gives a short text of a tool input: the command or the path when there is one, else the JSON.
func toolInputText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) == nil {
		for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
			if s, ok := fields[k].(string); ok && s != "" {
				return clip(s)
			}
		}
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return ""
	}
	return clip(b.String())
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxToolInput {
		return s
	}
	return string(r[:maxToolInput-1]) + "…"
}

func ptr[T any](v T) *T { return &v }
