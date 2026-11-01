package hooks

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

// EventAdder takes the event rows of the hook calls (capture.Writer).
type EventAdder interface {
	Add(session string, d wire.EventDraft)
}

const maxPromptText = 2000

var (
	testCommand = regexp.MustCompile(`\b(go test|swift test|npm (run )?test|pnpm (run )?test|yarn test|npx (vitest|jest)|vitest|jest|pytest|cargo test|xcodebuild test|make test|bun test)\b`)
	testFailed  = regexp.MustCompile(`(?m)^(--- )?FAIL\b|\b[1-9]\d* (failed|failing|failures?)\b|Test Suite .* failed|\bTests? failed\b`)
	editTools   = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "apply_patch": true}
)

// recorder keeps the open turn and the open wait of each session in memory, and makes the rows of each hook call.
// Tool calls make no row: they add to the counts of the open turn.
type recorder struct {
	add EventAdder
	mu  sync.Mutex
	by  map[string]*sessionTrack
}

type sessionTrack struct {
	harness, model string
	started        time.Time
	turns          int
	turn           *turnTrack
	wait           *waitTrack
}

type turnTrack struct {
	n, tools, toolErrors, testsRun, testsFailed int
	id                                          string
	at                                          time.Time
	files                                       map[string]bool
}

type waitTrack struct {
	id, reason string
	at         time.Time
}

func newRecorder(add EventAdder) *recorder {
	return &recorder{add: add, by: map[string]*sessionTrack{}}
}

// record makes the rows of one hook call. reason is the session reason after the call (the book patch).
func (r *recorder) record(ev Event, p payload, session string, reason wire.SessionReason) {
	r.mu.Lock()
	rows := r.rows(ev, p, session, reason)
	r.mu.Unlock()
	for _, d := range rows {
		r.add.Add(session, d)
	}
}

func (r *recorder) rows(ev Event, p payload, session string, reason wire.SessionReason) []wire.EventDraft {
	s := r.by[session]
	if s == nil {
		s = &sessionTrack{harness: ev.Harness, started: ev.At}
		r.by[session] = s
	}
	if p.Model != "" {
		s.model = p.Model
	}
	name := ev.Name
	at := ev.At
	mk := func(kind string, actor wire.ActorType, summary string, payload map[string]any) wire.EventDraft {
		d := wire.EventDraft{ID: "hook:" + session + ":" + strings.ToLower(name) + ":" + strconv.FormatInt(at.UnixMilli(), 10) + ":" + kind,
			At: at, Kind: kind, ActorType: actor, Via: "hook", Summary: summary}
		if actor == wire.ActorAgent {
			d.Actor = agentActor(s.harness, s.model)
		}
		if payload != nil {
			d.Payload, _ = json.Marshal(payload)
		}
		return d
	}
	var out []wire.EventDraft
	endWait := func(actor wire.ActorType) {
		if s.wait == nil {
			return
		}
		d := mk(wire.KindWaitEnded, actor, "Wait ended: "+s.wait.reason, map[string]any{"reason": s.wait.reason, "waitedSec": secs(at.Sub(s.wait.at))})
		d.Corr = s.wait.id
		out = append(out, d)
		s.wait = nil
	}
	startWait := func(why, tool, question string) {
		if s.wait != nil {
			return
		}
		pl := map[string]any{"reason": why}
		if tool != "" {
			pl["tool"] = tool
		}
		if question != "" {
			pl["question"] = clip(question)
		}
		summary := "Waits for " + why
		if question != "" {
			summary += ": " + question
		} else if tool != "" {
			summary += ": " + tool
		}
		d := mk(wire.KindWaitStarted, wire.ActorAgent, summary, pl)
		d.Corr = d.ID
		s.wait = &waitTrack{id: d.ID, reason: why, at: at}
		out = append(out, d)
	}
	endTurn := func(interrupted bool) {
		t := s.turn
		if t == nil {
			return
		}
		pl := map[string]any{"turn": t.n, "tools": t.tools, "durationSec": secs(at.Sub(t.at))}
		for k, v := range map[string]int{"toolErrors": t.toolErrors, "filesEdited": len(t.files), "testsRun": t.testsRun, "testsFailed": t.testsFailed} {
			if v > 0 {
				pl[k] = v
			}
		}
		if interrupted {
			pl["interrupted"] = true
		}
		summary := "Turn " + strconv.Itoa(t.n) + " ended: " + strconv.Itoa(t.tools) + " tools"
		if n := len(t.files); n > 0 {
			summary += ", " + strconv.Itoa(n) + " files"
		}
		if t.testsFailed > 0 {
			summary += ", tests failed"
		}
		d := mk(wire.KindTurnEnded, wire.ActorAgent, summary, pl)
		d.Corr = t.id
		out = append(out, d)
		if interrupted {
			i := mk(wire.KindTurnInterrupted, wire.ActorHuman, "You stopped turn "+strconv.Itoa(t.n), map[string]any{"turn": t.n})
			i.Corr = t.id
			out = append(out, i)
		}
		s.turn = nil
	}
	// A subagent call only counts its tools.
	if p.AgentID != "" && name != "SubagentStop" {
		if name == "PostToolUse" && s.turn != nil {
			s.turn.count(p)
		}
		return nil
	}
	switch name {
	case "SessionStart":
		if p.Source == "compact" {
			return nil
		}
		pl := map[string]any{"harness": s.harness}
		if s.model != "" {
			pl["model"] = s.model
		}
		if cwd := cmpOr(p.Cwd, ev.Env.WorktreePath); cwd != "" {
			pl["cwd"] = cwd
		}
		if ev.Env.PaneKey != "" {
			pl["runtime"] = wire.RuntimeOrca
		}
		summary := "Session started"
		if p.Source == "resume" {
			summary = "Session resumed"
		}
		out = append(out, mk(wire.KindSessionStarted, wire.ActorAgent, summary, pl))
	case "UserPromptSubmit":
		endWait(wire.ActorHuman)
		// A new prompt while a turn is open: the user stopped that turn.
		endTurn(true)
		s.turns++
		steer := s.turns > 1
		t := &turnTrack{n: s.turns, at: at, files: map[string]bool{}}
		start := mk(wire.KindTurnStarted, wire.ActorAgent, "Turn "+strconv.Itoa(t.n)+" started", map[string]any{"turn": t.n, "steer": steer})
		start.Corr = start.ID
		t.id = start.ID
		s.turn = t
		text := []rune(strings.TrimSpace(p.Prompt))
		if len(text) > maxPromptText {
			text = append(text[:maxPromptText-1], '…')
		}
		prompt := mk(wire.KindPromptSubmitted, wire.ActorHuman, string(text), map[string]any{"turn": t.n, "steer": steer, "text": string(text)})
		prompt.Private, prompt.Corr = true, start.ID
		out = append(out, prompt, start)
	case "PreToolUse":
		switch {
		case reason == wire.ReasonQuestion:
			startWait("question", p.ToolName, firstQuestion(p.ToolInput))
		case p.ToolName == "ExitPlanMode":
			startWait("plan", p.ToolName, "")
		default:
			endWait(wire.ActorHuman)
		}
	case "PostToolUse":
		endWait(wire.ActorHuman)
		if s.turn != nil {
			s.turn.count(p)
		}
	case "PermissionRequest":
		why := "permission"
		if reason == wire.ReasonQuestion {
			why = "question"
		}
		startWait(why, p.ToolName, toolInputText(p.ToolInput))
	case "Notification":
		switch p.NotificationType {
		case "permission_prompt":
			startWait("permission", "", "")
		case "idle_prompt", "elicitation_dialog":
			startWait("input", "", "")
		}
	case "PreCompact":
		trigger, actor := "auto", wire.ActorAgent
		if p.Trigger == "manual" {
			trigger, actor = "manual", wire.ActorHuman
		}
		out = append(out, mk(wire.KindContextCompacted, actor, "Context compacted ("+trigger+")", map[string]any{"trigger": trigger}))
	case "SubagentStop":
		var pl map[string]any
		summary := "Subagent ended"
		if p.AgentType != "" {
			pl = map[string]any{"agentType": p.AgentType}
			summary += ": " + p.AgentType
		}
		out = append(out, mk(wire.KindSubagentEnded, wire.ActorAgent, summary, pl))
	case "Stop", "StopFailure", "Interrupt":
		endWait(wire.ActorAgent)
		endTurn(reason == wire.ReasonInterrupted)
		// A turn that ends with a question waits for the answer. A plain end waits for the idle_prompt notification.
		if reason == wire.ReasonQuestion {
			startWait("question", "", "")
		}
	case "SessionEnd":
		endWait(wire.ActorSystem)
		endTurn(false)
		actor := wire.ActorHuman
		if p.Reason == "other" || p.Reason == "" {
			actor = wire.ActorAgent
		}
		pl := map[string]any{"turns": s.turns, "durationSec": secs(at.Sub(s.started))}
		if p.Reason != "" {
			pl["reason"] = p.Reason
		}
		out = append(out, mk(wire.KindSessionEnded, actor, "Session ended", pl))
		delete(r.by, session)
	}
	return out
}

// count adds one tool call to the turn.
func (t *turnTrack) count(p payload) {
	t.tools++
	var resp map[string]any
	_ = json.Unmarshal(p.ToolResponse, &resp)
	if e, _ := resp["is_error"].(bool); e {
		t.toolErrors++
	} else if e, _ := resp["error"].(string); e != "" {
		t.toolErrors++
	}
	var in map[string]any
	_ = json.Unmarshal(p.ToolInput, &in)
	if editTools[p.ToolName] {
		if f, _ := in["file_path"].(string); f != "" {
			t.files[f] = true
		} else if f, _ := in["notebook_path"].(string); f != "" {
			t.files[f] = true
		} else {
			t.files[p.ToolName+strconv.Itoa(t.tools)] = true
		}
	}
	cmd, _ := in["command"].(string)
	if cmd == "" || !testCommand.MatchString(cmd) {
		return
	}
	t.testsRun++
	out, _ := resp["stdout"].(string)
	errOut, _ := resp["stderr"].(string)
	if testFailed.MatchString(out) || testFailed.MatchString(errOut) {
		t.testsFailed++
	}
}

// firstQuestion gives the text of the first question of an AskUserQuestion input.
func firstQuestion(raw json.RawMessage) string {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
		Question string `json:"question"`
	}
	_ = json.Unmarshal(raw, &in)
	if len(in.Questions) > 0 {
		return in.Questions[0].Question
	}
	return in.Question
}

func agentActor(harness, model string) string {
	if model == "" {
		return harness
	}
	return harness + "/" + model
}

func secs(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d.Round(time.Second) / time.Second)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
