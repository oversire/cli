package allow

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/oversire/runner/wire"
)

func abs(p string) string {
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}
	return p
}

var list = List{
	Runtimes:     []string{"orca"},
	Repos:        []string{abs("/Users/ada/Projects/app"), abs("/Users/ada/work/"), "https://github.com/ada/site"},
	Agents:       []string{"claude", "codex"},
	Environments: []string{"local", "mbp"},
}

func start(repo, agent, env string) wire.WorkOrder {
	return wire.WorkOrder{ID: "o", Runtime: "orca", Payload: wire.LaunchRequest{Repo: repo, Agent: agent, Prompt: "go", Environment: env}}
}

func session(env string) wire.SessionRef {
	return wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "orca", ID: env}, ID: "w"}, ID: "t"}
}

func TestStart(t *testing.T) {
	cases := []struct {
		name, repo, agent, env, err string
	}{
		{"exact repo", abs("/Users/ada/Projects/app"), "claude", "", ""},
		{"repo not clean", abs("/Users/ada/Projects/./app/"), "claude", "", ""},
		{"parent folder", abs("/Users/ada/work/api/sub"), "codex", "mbp", ""},
		{"listed URL", "https://github.com/ada/site", "claude", "local", ""},
		{"sibling with prefix", abs("/Users/ada/Projects/app2"), "claude", "", "no repo"},
		{"escape with dots", abs("/Users/ada/work/../secret"), "claude", "", "no repo"},
		{"relative path", "Projects/app", "claude", "", "no repo"},
		{"agent", abs("/Users/ada/Projects/app"), "opencode", "", "agent opencode"},
		{"environment", abs("/Users/ada/Projects/app"), "claude", "studio", "environment studio"},
	}
	for _, c := range cases {
		err := list.Check(start(c.repo, c.agent, c.env))
		if c.err == "" && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.err)
		}
	}
}

func TestRuntime(t *testing.T) {
	o := start(abs("/Users/ada/Projects/app"), "claude", "")
	o.Runtime = "oversire"
	if err := list.Check(o); err == nil || err.Error() != "The allow list does not have the runtime oversire." {
		t.Fatal(err)
	}
}

func TestOversireRuntime(t *testing.T) {
	l := list
	l.Runtimes = []string{"orca", "oversire"}
	o := start(abs("/Users/ada/Projects/app"), "claude", "")
	o.Runtime = "oversire"
	if err := l.Check(o); err != nil {
		t.Fatal(err)
	}
	o.Payload = wire.LaunchRequest{Repo: abs("/Users/ada/elsewhere"), Agent: "claude", Prompt: "go"}
	if err := l.Check(o); err == nil || !strings.Contains(err.Error(), "no repo") {
		t.Fatalf("repo: %v", err)
	}
	o.Payload = wire.LaunchRequest{Repo: abs("/Users/ada/Projects/app"), Agent: "gemini", Prompt: "go"}
	if err := l.Check(o); err == nil || !strings.Contains(err.Error(), "agent gemini") {
		t.Fatalf("agent: %v", err)
	}
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/APP-2"}, ID: "s-1"}
	for _, p := range []wire.Payload{
		wire.StopPayload{Session: s},
		wire.ReadPayload{Session: s, Lines: 4},
		wire.SendPayload{Session: s, Permission: &wire.PermissionAnswer{ID: "p-1", Allow: true}},
		wire.HistoryPayload{Session: s},
	} {
		if err := l.Check(wire.WorkOrder{Runtime: "oversire", Payload: p}); err != nil {
			t.Errorf("%s: %v", p.Op(), err)
		}
	}
}

func TestSessionOrders(t *testing.T) {
	for _, p := range []wire.Payload{
		wire.StopPayload{Session: session("mbp")},
		wire.ReadPayload{Session: session("local"), Lines: 4},
		wire.SendPayload{Session: session("mbp"), Text: "go"},
		wire.HistoryPayload{Session: session("local")},
	} {
		if err := list.Check(wire.WorkOrder{Runtime: "orca", Payload: p}); err != nil {
			t.Errorf("%s: %v", p.Op(), err)
		}
		bad := wire.WorkOrder{Runtime: "orca", Payload: wire.StopPayload{Session: session("studio")}}
		if err := list.Check(bad); err == nil {
			t.Errorf("%s: unknown environment passed", p.Op())
		}
	}
	if err := list.Check(wire.WorkOrder{Runtime: "orca", Payload: wire.HistoryPayload{Session: session("studio")}}); err == nil {
		t.Error("history: unknown environment passed")
	}
}

func TestFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.json")
	f := File{Path: path}
	err := f.Check(start(abs("/Users/ada/Projects/app"), "claude", ""))
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing file: %v", err)
	}
	if s := f.Summary(); s.Repos != 0 || s.Runtimes == nil || s.Agents == nil {
		t.Fatalf("summary of missing file: %#v", s)
	}
	os.WriteFile(path, []byte(`{"runtimes":["orca"],"repos":["/a","/b"],"agents":["claude"],"environments":["local"]}`), 0o600)
	if err := f.Check(start("/a", "claude", "")); err != nil {
		t.Fatal(err)
	}
	if s := f.Summary(); s.Repos != 2 || s.Agents[0] != "claude" {
		t.Fatalf("summary: %#v", s)
	}
	os.WriteFile(path, []byte(`{`), 0o600)
	if err := f.Check(start("/a", "claude", "")); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatal(err)
	}
}
