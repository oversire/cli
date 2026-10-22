package hooks

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

var orcaEnv = wire.HookEnv{PaneKey: "tab1:leaf1", TabID: "tab1", WorktreeID: "repo::/Users/ada/Projects/app", TerminalHandle: "term_1", WorktreePath: "/Users/ada/Projects/app"}

// view is a patch with the pointers read, for compact expectations.
type view struct {
	ID, Runtime, State, Reason, Tool, ToolInput, Prompt, LastMessage, Cwd, Terminal string
	End                                                                             bool
}

func viewOf(p sessions.Patch) view {
	s := func(v *string) string {
		if v == nil {
			return "<nil>"
		}
		return *v
	}
	v := view{ID: p.ID, Runtime: s(p.Runtime), Tool: s(p.Tool), ToolInput: s(p.ToolInput), Prompt: s(p.Prompt), LastMessage: s(p.LastMessage), Cwd: s(p.Cwd), Terminal: s(p.Terminal), End: p.End, State: "<nil>", Reason: "<nil>"}
	if p.State != nil {
		v.State = string(*p.State)
	}
	if p.Reason != nil {
		v.Reason = string(*p.Reason)
	}
	return v
}

func event(t *testing.T, harness, file string, env wire.HookEnv) Event {
	t.Helper()
	raw := read(t, filepath.Join("payloads", harness, file+".json"))
	return Event{Harness: harness, Name: readPayload(raw).HookEventName, Payload: json.RawMessage(raw), Env: env, At: time.Unix(100, 0)}
}

const (
	cliClaude = "cli:5d0c7a52-3f1e-4b8e-9a51-2c6d1f0e8b11"
	cliCodex  = "cli:019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b"
	appPath   = "/Users/ada/Projects/app"
	none      = "<nil>"
)

func TestClaudeMap(t *testing.T) {
	cases := []struct {
		file string
		ok   bool
		want view
	}{
		{"SessionStart", true, view{ID: cliClaude, Runtime: "cli", State: "done", Reason: "turnEnd"}},
		{"SessionStart-compact", false, view{}},
		{"UserPromptSubmit", true, view{ID: cliClaude, Runtime: "cli", State: "working", Reason: "", Prompt: "Add a health route."}},
		{"PreToolUse-AskUserQuestion", true, view{ID: cliClaude, Runtime: "cli", State: "waiting", Reason: "question", Tool: "AskUserQuestion",
			ToolInput: `{"questions":[{"question":"Which database?","header":"Database","multiSelect":false,"options":[{"label":"Postgres","description":"Use Postgres"},{"label":"SQLite","description":"Use SQLite"}]}]}`}},
		{"PreToolUse-ExitPlanMode", true, view{ID: cliClaude, Runtime: "cli", State: "waiting", Reason: "permission", Tool: "ExitPlanMode", ToolInput: "{\"plan\":\"1. Add the route.\\n2. Add a test.\"}"}},
		{"PostToolUse-AskUserQuestion", true, view{ID: cliClaude, Runtime: "cli", State: "working", Reason: ""}},
		{"PermissionRequest", true, view{ID: cliClaude, Runtime: "cli", State: "waiting", Reason: "permission", Tool: "Bash", ToolInput: "rm -rf build"}},
		{"PermissionRequest-subagent", true, view{ID: cliClaude, Runtime: "cli", State: "waiting", Reason: "permission", Tool: "Bash", ToolInput: "ls"}},
		{"PreToolUse-subagent", false, view{}},
		{"Notification-permission", true, view{ID: cliClaude, Runtime: "cli", State: "waiting", Reason: "permission"}},
		{"Notification-idle", true, view{ID: cliClaude, Runtime: "cli", State: none, Reason: none}},
		{"Stop", true, view{ID: cliClaude, Runtime: "cli", State: "done", Reason: "turnEnd", LastMessage: "The route is in place. All tests pass."}},
		{"Stop-transcript", true, view{ID: cliClaude, Runtime: "cli", State: "done", Reason: "turnEnd"}},
		{"StopFailure", true, view{ID: cliClaude, Runtime: "cli", State: "done", Reason: "error"}},
		{"SessionEnd", true, view{ID: cliClaude, Runtime: "cli", State: none, Reason: none, End: true}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			p, ok := Claude{}.Map(event(t, "claude", c.file, wire.HookEnv{}))
			if ok != c.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !ok {
				return
			}
			fill(&c.want, appPath, none)
			if got := viewOf(p); got != c.want {
				t.Fatalf("\ngot  %+v\nwant %+v", got, c.want)
			}
			if p.Source != sessions.SourceHook || *p.Harness != "claude" || *p.HarnessSession == "" || !p.At.Equal(time.Unix(100, 0)) {
				t.Fatalf("identity: %+v", p)
			}
		})
	}
}

// fill sets the fields that most cases share.
func fill(v *view, cwd, terminal string) {
	for _, f := range []*string{&v.Tool, &v.ToolInput, &v.Prompt, &v.LastMessage} {
		if *f == "" {
			*f = none
		}
	}
	if v.Cwd == "" {
		v.Cwd = cwd
	}
	if v.Terminal == "" {
		v.Terminal = terminal
	}
}

func TestClaudeMapOrcaPane(t *testing.T) {
	p, ok := Claude{}.Map(event(t, "claude", "UserPromptSubmit", orcaEnv))
	if !ok || p.ID != "tab1:leaf1" || *p.Runtime != wire.RuntimeOrca || *p.Workspace != orcaEnv.WorktreeID || *p.Terminal != "term_1" || *p.Cwd != appPath {
		t.Fatalf("%+v", viewOf(p))
	}
}

func TestCodexMap(t *testing.T) {
	cases := []struct {
		file string
		want view
	}{
		{"SessionStart", view{ID: cliCodex, Runtime: "cli", State: "done", Reason: "turnEnd"}},
		{"UserPromptSubmit", view{ID: cliCodex, Runtime: "cli", State: "working", Reason: "", Prompt: "Fix the build."}},
		{"PreToolUse-request_user_input", view{ID: cliCodex, Runtime: "cli", State: "waiting", Reason: "question", Tool: "request_user_input",
			ToolInput: `{"questions":[{"id":"db","header":"Database","question":"Which database?","options":[{"label":"Postgres","description":""}]}]}`}},
		{"PreToolUse-shell", view{ID: cliCodex, Runtime: "cli", State: "working", Reason: ""}},
		{"PermissionRequest", view{ID: cliCodex, Runtime: "cli", State: "waiting", Reason: "permission", Tool: "Bash", ToolInput: "git push"}},
		{"Stop", view{ID: cliCodex, Runtime: "cli", State: "done", Reason: "turnEnd", LastMessage: "The build passes now."}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			p, ok := Codex{}.Map(event(t, "codex", c.file, wire.HookEnv{}))
			if !ok {
				t.Fatal("not mapped")
			}
			fill(&c.want, appPath, none)
			if got := viewOf(p); got != c.want {
				t.Fatalf("\ngot  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestToolInputText(t *testing.T) {
	long := make([]byte, 0, 400)
	for range 400 {
		long = append(long, 'a')
	}
	raw, _ := json.Marshal(map[string]string{"command": string(long)})
	if got := []rune(toolInputText(raw)); len(got) != maxToolInput || got[len(got)-1] != '…' {
		t.Fatalf("clip: %d", len(got))
	}
	if toolInputText(nil) != "" || toolInputText(json.RawMessage("null")) != "" {
		t.Fatal("empty input")
	}
}
