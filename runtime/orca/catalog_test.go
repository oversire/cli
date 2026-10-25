package orca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

func TestProbe(t *testing.T) {
	o, r := setup()
	o.stat = func(string) (os.FileInfo, error) { return nil, nil }
	r.on("status --json", `{"ok":true,"result":{"runtime":{"state":"ready","reachable":true,"appVersion":"1.4.237"}}}`)
	if d := o.Probe(context.Background()); !d.Installed || !d.Reachable || d.Version != "1.4.237" || d.Name != "Orca" {
		t.Fatalf("probe %#v", d)
	}
	r.on("status --json", `{"ok":true,"result":{"runtime":{"reachable":false}}}`)
	if d := o.Probe(context.Background()); d.Reachable || d.Error != "Orca is not running." {
		t.Fatalf("down %#v", d)
	}
	o.exe = filepath.Join(os.TempDir(), "orca")
	o.stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	n := len(r.calls)
	if d := o.Probe(context.Background()); d.Installed || d.Reachable || len(r.calls) != n {
		t.Fatalf("no CLI %#v", d)
	}
}

func TestRepos(t *testing.T) {
	o, r := setup()
	r.on("repo list", `{"ok":true,"result":{"repos":[{"id":"r1","path":"/p/app","displayName":"App","kind":"git"},`+
		`{"id":"r2","path":"/p/ws","kind":"folder"},{"id":"","path":"/bad"}]}}`)
	repos, err := o.Repos(context.Background())
	want := []wire.DaemonRepo{{ID: "r1", Runtime: "orca", Name: "App", Path: "/p/app"}, {ID: "r2", Runtime: "orca", Name: "ws", Path: "/p/ws", Folder: true}}
	if err != nil || !slices.Equal(repos, want) {
		t.Fatalf("repos %#v %v", repos, err)
	}
}

func TestWorkspaces(t *testing.T) {
	o, r := setup()
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[`+
		`{"worktreeId":"r1::/p/app","repo":"app","path":"/p/app","isMainWorktree":true,"liveTerminalCount":0,"lastActivityAt":1792764909139},`+
		`{"worktreeId":"r1::/wt/eng-4","repo":"app","path":"/wt/eng-4","displayName":"ENG-4","comment":"https://x/ENG-4","branch":"refs/heads/eng-4","liveTerminalCount":1,"lastActivityAt":1792764909139},`+
		`{"worktreeId":"r2::/p/ws::workspace:9","repo":"ws","path":"/p/ws","displayName":"Notes","liveTerminalCount":0,"lastActivityAt":0},`+
		`{"worktreeId":"r1::/wt/old","isArchived":true}]}}`)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[`+
		`{"handle":"h1","worktreeId":"r1::/wt/eng-4","title":"claude","tabId":"t1","leafId":"l1"},`+
		`{"handle":"h2","worktreeId":"r1::/wt/eng-4","title":"zsh","tabId":"t2","leafId":"l2","orphaned":true},`+
		`{"handle":"h3","worktreeId":"r1::/wt/eng-4","title":"Enhance ENG-4","tabId":"pty:p3","leafId":"pty:p3","recordedPaneKey":"t3:l3"}]}}`)
	ws, err := o.Workspaces(context.Background())
	if err != nil || len(ws) != 3 {
		t.Fatalf("workspaces %#v %v", ws, err)
	}
	if ws[0].Kind != wire.KindMain || ws[1].Kind != wire.KindWorktree || ws[2].Kind != wire.KindFolder {
		t.Fatalf("kinds %v %v %v", ws[0].Kind, ws[1].Kind, ws[2].Kind)
	}
	w := ws[1]
	if w.Branch != "eng-4" || w.Name != "ENG-4" || w.Comment != "https://x/ENG-4" || w.LiveTerminals != 1 || !w.LastActivity.Equal(time.UnixMilli(1792764909139)) {
		t.Fatalf("card %#v", w)
	}
	terms := []wire.DaemonTerminal{{ID: "h1", Title: "claude", Session: "t1:l1"}, {ID: "h2", Title: "zsh", Session: "t2:l2", Orphaned: true},
		{ID: "h3", Title: "Enhance ENG-4", Session: "t3:l3"}}
	if !slices.Equal(w.Terminals, terms) || ws[0].Terminals != nil {
		t.Fatalf("terminals %#v", w.Terminals)
	}
	if got := r.args("terminal list")[0]; !slices.Contains(got, "--limit") || slices.Contains(got, "--worktree") {
		t.Fatalf("one list for all terminals: %q", got)
	}
	noEnvironment(t, r)
}

func TestRemoveWorkspace(t *testing.T) {
	o, r := setup()
	ctx := context.Background()
	r.on("worktree ps", `{"ok":true,"result":{"worktrees":[{"worktreeId":"r1::/p/app","isMainWorktree":true},{"worktreeId":"r1::/wt/eng-4"}]}}`)
	r.on("worktree rm", `{"ok":true,"result":{"warnings":["The branch eng-4 stays."],"warning":"The hook failed."}}`)

	if _, err := o.RemoveWorkspace(ctx, "r1::/nope", false, false); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := o.RemoveWorkspace(ctx, "r1::/p/app", true, false); err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Fatalf("main: %v", err)
	}
	if len(r.args("worktree rm")) != 0 {
		t.Fatal("a refused removal ran worktree rm")
	}
	rm, err := o.RemoveWorkspace(ctx, "r1::/wt/eng-4", true, true)
	if err != nil || rm.Warning != "The branch eng-4 stays. The hook failed." {
		t.Fatalf("removal %#v %v", rm, err)
	}
	if got := r.args("worktree rm")[0]; !slices.Equal(got, []string{"worktree", "rm", "--worktree", "id:r1::/wt/eng-4", "--force", "--run-hooks", "--json"}) {
		t.Fatalf("rm %q", got)
	}
}

func TestReveal(t *testing.T) {
	o, r := setup()
	r.on("terminal switch", `{"ok":true,"result":{}}`)
	r.on("-a Orca", ``)
	s := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: ID, ID: "local"}, ID: "c1"}, ID: "h1"}
	if err := o.Reveal(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || !slices.Equal(r.calls[1], []string{"-a", "Orca"}) {
		t.Fatalf("calls %q", r.calls)
	}
}
