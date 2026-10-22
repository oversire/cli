package orca

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// replay answers with recorded CLI output, keyed by the first two arguments. The last answer repeats.
type replay struct {
	mu      sync.Mutex
	answers map[string][]string
	calls   [][]string
}

func newReplay() *replay { return &replay{answers: map[string][]string{}} }

func (r *replay) on(command string, replies ...string) { r.answers[command] = replies }

func (r *replay) Run(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	key := strings.Join(args[:2], " ")
	list := r.answers[key]
	if len(list) == 0 {
		return nil, []byte("unknown command " + key), errors.New("exit status 1")
	}
	out := list[0]
	if len(list) > 1 {
		r.answers[key] = list[1:]
	}
	return []byte(out), nil, nil
}

func (r *replay) args(command string) [][]string {
	var out [][]string
	for _, c := range r.calls {
		if strings.Join(c[:2], " ") == command {
			out = append(out, c)
		}
	}
	return out
}

func recorded(t *testing.T, name string) string {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setup() (*Orca, *replay) {
	r := newReplay()
	o := New("/bin/orca", r)
	o.shellWait = 0
	return o, r
}

func TestStartInANewWorkspaceUsesTheShell(t *testing.T) {
	o, r := setup()
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[]}}`)
	r.on("worktree create", `{"ok":true,"result":{"worktree":{"id":"c1"}}}`)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[]}}`,
		`{"ok":true,"result":{"terminals":[{"handle":"h1","worktreeId":"c1","title":"zsh","tabId":"t1","leafId":"l1"}]}}`)
	r.on("terminal wait", `{"ok":true,"result":{"wait":{"satisfied":false}}}`, `{"ok":true,"result":{"wait":{"satisfied":true}}}`)
	r.on("terminal send", `{"ok":true,"result":{}}`, `{"ok":true,"result":{"send":{"prompt":{"stages":["typed","turn_started"]}}}}`)

	ref, err := o.Start(context.Background(), runtime.Order{}, wire.LaunchRequest{Repo: "/w", Name: "ENG-4", Note: "https://x/ENG-4", Agent: "claude", Prompt: "go", Environment: "mbp"})
	if err != nil {
		t.Fatal(err)
	}
	want := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: "mbp"}, ID: "c1"}, ID: "h1"}
	if ref != want {
		t.Fatalf("ref %#v", ref)
	}
	create := r.args("worktree create")[0]
	if !slices.Equal(create, []string{"worktree", "create", "--repo", "path:/w", "--name", "ENG-4", "--comment", "https://x/ENG-4", "--json", "--environment", "mbp"}) {
		t.Fatalf("create %q", create)
	}
	sends := r.args("terminal send")
	if len(sends) != 2 || sends[0][5] != "claude -n 'ENG-4'" || sends[1][5] != "go" {
		t.Fatalf("sends %q", sends)
	}
	if !slices.Contains(sends[1], "--wait-submit") {
		t.Fatal("the prompt waits for the submit")
	}
	if len(r.args("terminal wait")) != 2 {
		t.Fatal("a second wait for a cold start")
	}
}

func TestCheckCommand(t *testing.T) {
	cases := []struct{ agent, cmd, err string }{
		{"claude", "claude -n ENG-4 --dangerously-skip-permissions", ""},
		{"claude", "claude -n 'Fix the retry' --model opus", ""},
		{"codex", "/opt/homebrew/bin/codex --full-auto", ""},
		{"claude", "codex --full-auto", "The command must start with claude."},
		{"claude", "claudex -n x", "The command must start with claude."},
		{"claude", "/bin/notclaude", "The command must start with claude."},
		{"claude", "  ", "The command must start with claude."},
		{"claude", "claude\nrm -rf ~", "newline"},
	}
	for _, op := range []string{";", "|", "&", "`", "$(", ">", "<"} {
		cases = append(cases, struct{ agent, cmd, err string }{"claude", "claude -n x " + op + " echo", "shell operator " + op + "."})
	}
	for _, c := range cases {
		err := checkCommand(c.agent, c.cmd)
		if c.err == "" {
			if err != nil {
				t.Errorf("%q: %v", c.cmd, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%q: got %v, want %q", c.cmd, err, c.err)
		}
	}
}

func TestStartUsesTheCheckedCommand(t *testing.T) {
	o, r := setup()
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[]}}`)
	r.on("worktree create", `{"ok":true,"result":{"worktree":{"id":"c1"}}}`)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[{"handle":"h1","worktreeId":"c1"}]}}`)
	r.on("terminal wait", `{"ok":true,"result":{"wait":{"satisfied":true}}}`)
	r.on("terminal send", `{"ok":true,"result":{}}`)

	custom := "claude -n ENG-4 --dangerously-skip-permissions"
	if _, err := o.Start(context.Background(), runtime.Order{}, wire.LaunchRequest{Repo: "/w", Name: "ENG-4", Agent: "claude", Prompt: "go", Command: custom}); err != nil {
		t.Fatal(err)
	}
	if sends := r.args("terminal send"); len(sends) != 2 || sends[0][5] != custom {
		t.Fatalf("sends %q", sends)
	}

	calls := len(r.calls)
	_, err := o.Start(context.Background(), runtime.Order{}, wire.LaunchRequest{Repo: "/w", Name: "ENG-4", Agent: "claude", Prompt: "go", Command: "claude; rm -rf ~"})
	if err == nil || err.Error() != "The command has the shell operator ;. The runner does not run it." {
		t.Fatalf("err %v", err)
	}
	if len(r.calls) != calls {
		t.Fatal("a refused command calls no Orca")
	}
}

func TestStartReusesTheWorkspace(t *testing.T) {
	o, r := setup()
	r.on("worktree ps", recorded(t, "worktree-ps.json"))
	r.on("terminal create", `{"ok":true,"result":{"terminal":{"handle":"h2","paneKey":"t2:l2","worktreeId":"x"}}}`)
	r.on("terminal wait", `{"ok":true,"result":{"wait":{"satisfied":true}}}`)
	r.on("terminal send", `{"ok":true,"result":{}}`)

	item := &wire.ItemRef{Key: "eng-4", URL: "https://x/ENG-4"}
	ref, err := o.Start(context.Background(), runtime.Order{}, wire.LaunchRequest{Repo: "/Users/ada/Projects/app", Item: item, Agent: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Workspace.ID != "88171858-3683-42ea-90f2-bb9e46c8a73e::/Users/ada/Projects/app::workspace:0717323b" || ref.ID != "h2" || ref.Workspace.Account.ID != "local" {
		t.Fatalf("ref %#v", ref)
	}
	if len(r.args("worktree create")) != 0 {
		t.Fatal("no new workspace")
	}
	tc := r.args("terminal create")[0]
	if !slices.Equal(tc[:8], []string{"terminal", "create", "--worktree", "id:" + ref.Workspace.ID, "--title", "eng-4", "--command", "codex"}) {
		t.Fatalf("terminal create %q", tc)
	}
	for _, c := range r.calls {
		if slices.Contains(c, "--environment") {
			t.Fatalf("a local call has --environment: %q", c)
		}
	}
}

func TestStartFailures(t *testing.T) {
	o, r := setup()
	ctx := context.Background()
	if _, err := o.Start(ctx, runtime.Order{}, wire.LaunchRequest{Repo: "/w", Agent: "claude", Prompt: "go"}); err == nil {
		t.Fatal("no name passed")
	}
	if _, err := o.Start(ctx, runtime.Order{}, wire.LaunchRequest{Repo: "/w", Name: "A-1", Agent: "codex; rm -rf ~", Prompt: "go"}); err == nil {
		t.Fatal("a bad agent name passed")
	}
	if len(r.calls) != 0 {
		t.Fatal("a refused launch called the CLI")
	}
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[]}}`)
	r.on("worktree create", `{"ok":false,"error":{"code":"repo_not_found","message":"no repo"}}`)
	_, err := o.Start(ctx, runtime.Order{}, wire.LaunchRequest{Repo: "/nope", Name: "A-1", Agent: "claude", Prompt: "go"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "repo_not_found" || e.Message != "Orca has no workspace at /nope" {
		t.Fatalf("got %v", err)
	}

	r.on("worktree create", `{"ok":true,"result":{"worktree":{"id":"c1"}}}`)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[{"handle":"h1"}]}}`)
	r.on("terminal send", `{"ok":true,"result":{}}`)
	r.on("terminal wait", `{"ok":true,"result":{"wait":{"satisfied":false}}}`)
	_, err = o.Start(ctx, runtime.Order{}, wire.LaunchRequest{Repo: "/w", Name: "A-1", Agent: "claude", Prompt: "go"})
	if !errors.As(err, &e) || e.Code != "not_started" {
		t.Fatalf("got %v", err)
	}
	if n := len(r.args("terminal send")); n != 1 {
		t.Fatalf("the prompt went with no idle agent: %d sends", n)
	}
}

func TestSessionIO(t *testing.T) {
	o, r := setup()
	ctx := context.Background()
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: "local"}, ID: "c1"}, ID: "h1"}

	r.on("terminal read", `{"ok":true,"result":{"terminal":{"tail":["a b","","──────","  done  ","x y"]}}}`)
	lines, err := o.Read(ctx, s, 8)
	if err != nil || !slices.Equal(lines, []string{"a b", "done", "x y"}) {
		t.Fatalf("lines %q %v", lines, err)
	}
	if !slices.Contains(r.calls[len(r.calls)-1], "--screen") {
		t.Fatal("read uses --screen")
	}
	lines, _ = o.Read(ctx, s, 1)
	if !slices.Equal(lines, []string{"x y"}) {
		t.Fatalf("last line %q", lines)
	}

	r.on("terminal send", `{"ok":true,"result":{}}`)
	if err := o.Send(ctx, wire.SendPayload{Session: s, Text: "continue"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"terminal", "send", "--terminal", "h1", "--text", "continue", "--enter", "--wait-submit", "15", "--json"}
	if got := r.calls[len(r.calls)-1]; !slices.Equal(got, want) {
		t.Fatalf("send %q", got)
	}

	r.on("terminal close", `{"ok":true,"result":{}}`)
	s.Workspace.Account.ID = "mbp"
	if err := o.Stop(ctx, s); err != nil {
		t.Fatal(err)
	}
	want = []string{"terminal", "close", "--terminal", "h1", "--json", "--environment", "mbp"}
	if got := r.calls[len(r.calls)-1]; !slices.Equal(got, want) {
		t.Fatalf("close %q", got)
	}
}

func TestErrorMapping(t *testing.T) {
	o, r := setup()
	ctx := context.Background()
	s := wire.SessionRef{ID: "h1"}

	r.on("terminal close", `{"ok":false,"error":{"code":"terminal_not_found","message":"no terminal h1"}}`)
	var e *Error
	if err := o.Stop(ctx, s); !errors.As(err, &e) || e.Code != "terminal_not_found" || e.Message != "no terminal h1" {
		t.Fatalf("got %v", err)
	}
	r.on("terminal read", "boom")
	if _, err := o.Read(ctx, s, 4); err == nil || err.Error() != "orca terminal read: no JSON reply" {
		t.Fatalf("got %v", err)
	}
	if err := o.Send(ctx, wire.SendPayload{Session: s, Text: "x"}); err == nil || err.Error() != "orca terminal send: unknown command terminal send" {
		t.Fatalf("got %v", err)
	}
	r.on("terminal send", `{"ok":false}`)
	if err := o.Send(ctx, wire.SendPayload{Session: s, Text: "x"}); !errors.As(err, &e) || e.Message != "orca terminal send failed" {
		t.Fatalf("got %v", err)
	}
	n := len(r.calls)
	err := o.Send(ctx, wire.SendPayload{Session: s, Permission: &wire.PermissionAnswer{ID: "p-1", Allow: true}})
	if err == nil || !strings.Contains(err.Error(), "no permission prompts") || len(r.calls) != n {
		t.Fatalf("permission answer: %v", err)
	}
}

func TestAgentCommand(t *testing.T) {
	if got := AgentCommand("claude", "FRITZ-7"); got != "claude -n 'FRITZ-7'" {
		t.Fatal(got)
	}
	if got := AgentCommand("claude", "it's"); got != "claude -n 'its'" {
		t.Fatal(got)
	}
	if got := AgentCommand("codex", "X-1"); got != "codex" {
		t.Fatal(got)
	}
}

func TestHistory(t *testing.T) {
	o, r := setup()
	o.digests = func(h string) (digest.Reader, error) { return digest.ForHarness(h, t.TempDir()) }
	abs, err := filepath.Abs("../../contract/fixtures/session-digest/claude/basic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows path has backslashes, so it must be a JSON string.
	q, _ := json.Marshal(abs)
	file := strings.Trim(string(q), `"`)
	r.on("terminal show", `{"ok":true,"result":{"terminal":{"handle":"h1","title":"✳ ENG-4","worktreePath":"/w/eng-4","agentIdentity":"claude"}}}`)
	r.on("search --query", `{"ok":true,"result":{"kind":"results","hits":[`+
		`{"agent":"claude","sessionId":"x","title":"Other","source":{"presence":"present","filePath":"/nope.jsonl"}},`+
		`{"agent":"claude","sessionId":"y","title":"ENG-4","source":{"presence":"present","filePath":"`+file+`"}}]}}`)
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: "local"}, ID: "c1"}, ID: "h1"}
	d, err := o.History(context.Background(), ref, wire.NewHistoryBudget(12, 3, 4000))
	if err != nil || d.Source != wire.DigestFromRunner || d.Turns == 0 {
		t.Fatal(d, err)
	}
	want := []string{"search", "--query", "ENG-4", "--agent", "claude", "--path", "/w/eng-4", "--sort", "newest", "--limit", "100", "--json"}
	if got := r.args("search --query"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("search args %q", got)
	}
}

func TestHistoryFailures(t *testing.T) {
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: "mbp"}, ID: "c1"}, ID: "h1"}
	cases := map[string]struct{ show, search, want string }{
		"no agent":    {`{"ok":true,"result":{"terminal":{"title":"ENG-4","worktreePath":"/w","agentIdentity":""}}}`, "", "knows no agent"},
		"codex":       {`{"ok":true,"result":{"terminal":{"title":"ENG-4","worktreePath":"/w","agentIdentity":"codex"}}}`, "", `harness "codex"`},
		"no match":    {`{"ok":true,"result":{"terminal":{"title":"ENG-4","worktreePath":"/w","agentIdentity":"claude"}}}`, `{"ok":true,"result":{"hits":[{"title":"ENG-5","source":{"filePath":"/x"}}]}}`, "found no claude session"},
		"remote path": {`{"ok":true,"result":{"terminal":{"title":"ENG-4","worktreePath":"/w","agentIdentity":"claude"}}}`, `{"ok":true,"result":{"hits":[{"title":"ENG-4","source":{}}]}}`, "only local transcripts"},
	}
	for name, c := range cases {
		o, r := setup()
		o.digests = func(h string) (digest.Reader, error) { return digest.ForHarness(h, t.TempDir()) }
		r.on("terminal show", c.show)
		r.on("search --query", c.search)
		if _, err := o.History(context.Background(), ref, wire.HistoryBudget{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "no match" && !slices.Contains(r.calls[0], "mbp") {
			t.Errorf("no environment in %q", r.calls[0])
		}
	}
}
