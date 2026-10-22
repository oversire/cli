package orca

import (
	"context"
	"os"
	"slices"
	"testing"
)

func watchData(t *testing.T, name string) string {
	b, err := os.ReadFile("../../orcawatch/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorktrees(t *testing.T) {
	o, r := setup()
	r.on("worktree ps", watchData(t, "worktree-ps.json"))
	cards, err := o.Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 7 {
		t.Fatalf("cards: %d", len(cards))
	}
	c := cards[0]
	if c.DisplayName != "OVERSIRE-234" || len(c.Agents) != 1 || c.Agents[0].WorkingMode != "monitoring" || c.Agents[0].ToolName != "Bash" || c.Agents[0].StateStartedAt == 0 {
		t.Fatalf("%+v", c)
	}
	want := []string{"worktree", "ps", "--limit", "500", "--json"}
	if got := r.args("worktree ps")[0]; !slices.Equal(got, want) {
		t.Fatalf("args %v", got)
	}
}

func TestTerminals(t *testing.T) {
	o, r := setup()
	r.on("terminal list", watchData(t, "terminal-list.json"))
	ts, err := o.Terminals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	orphans := 0
	for _, x := range ts {
		if x.Handle == "" || x.PaneKey() == ":" {
			t.Fatalf("%+v", x)
		}
		if x.Orphaned {
			orphans++
			if x.PaneKey() != x.RecordedPaneKey {
				t.Fatalf("orphan pane: %+v", x)
			}
		}
	}
	if orphans == 0 {
		t.Fatal("the data has no orphaned terminal")
	}
	if got := r.args("terminal list")[0]; !slices.Equal(got, []string{"terminal", "list", "--limit", "500", "--json"}) {
		t.Fatalf("args %v", got)
	}
}

func TestReachable(t *testing.T) {
	o, r := setup()
	r.on("status --json", watchData(t, "status.json"), `{"ok":true,"result":{"runtime":{"reachable":false}}}`, `{"ok":false,"error":{"code":"no_app","message":"Orca is not running"}}`)
	if !o.Reachable(context.Background()) {
		t.Fatal("recorded status is reachable")
	}
	if o.Reachable(context.Background()) {
		t.Fatal("reachable false")
	}
	if o.Reachable(context.Background()) {
		t.Fatal("error answer")
	}
	if o.Reachable(context.Background()) {
		t.Fatal("no CLI answer")
	}
}
