package ticket

import (
	"strings"
	"testing"
)

func TestBodyRendersEachVerb(t *testing.T) {
	s := NewSkill()
	c := Context{Version: "v1", Machine: "mba", Cwd: "/w", Session: "s1", Harness: "claude", Items: []string{"OVERSIRE-1", "OVERSIRE-2"}}
	main, err := s.Body("", c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"oversire v1", "Session: s1", "Machine: mba", "OVERSIRE-1, OVERSIRE-2"} {
		if !strings.Contains(main, want) {
			t.Errorf("main body has no %q", want)
		}
	}
	if len(s.Verbs()) != 8 {
		t.Fatalf("verbs %v", s.Verbs())
	}
	for _, v := range s.Verbs() {
		b, err := s.Body(v, c)
		if err != nil || strings.TrimSpace(b) == "" || strings.Contains(b, "{{") {
			t.Errorf("%s: %v %q", v, err, b)
		}
	}
	pb, _ := s.Body("plan", c)
	for _, want := range []string{"## Many tickets", "all but 3", "at most 5"} {
		if !strings.Contains(pb, want) {
			t.Errorf("plan body has no %q", want)
		}
	}
	gb, _ := s.Body("groom", c)
	for _, want := range []string{"merge, done, obsolete, fold, relate", "## Merged from"} {
		if !strings.Contains(gb, want) {
			t.Errorf("groom body has no %q", want)
		}
	}
	sb, _ := s.Body("sweep", c)
	for _, want := range []string{"Approved, not merged", "Do not merge"} {
		if !strings.Contains(sb, want) {
			t.Errorf("sweep body has no %q", want)
		}
	}
	nb, _ := s.Body("new", c)
	if !strings.Contains(nb, "planner subagent") {
		t.Error("new has no planning steps")
	}
}

func TestBodyNoItems(t *testing.T) {
	b, _ := NewSkill().Body("", Context{})
	if !strings.Contains(b, "Session: unknown") || !strings.Contains(b, "Tickets of this session: none") {
		t.Fatal(b)
	}
}

func TestBodyUnknownVerb(t *testing.T) {
	for _, v := range []string{"enrich", "_planning", "main", "../x"} {
		if _, err := NewSkill().Body(v, Context{}); err == nil {
			t.Errorf("%s: no error", v)
		}
	}
}

// A planned ticket goes to Todo, so that OverSire ships it with no "Plan first" question (OVERSIRE-450).
func TestPlannedTicketGoesToTodo(t *testing.T) {
	s := NewSkill()
	c := Context{Version: "v1", Machine: "mba", Cwd: "/w", Session: "s1", Harness: "claude"}
	for _, v := range []string{"new", "plan"} {
		b, err := s.Body(v, c)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"state list --group unstarted", "Todo", "backlog"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s has no %q", v, want)
			}
		}
		if strings.Contains(b, "default state of the project") {
			t.Errorf("%s still uses the default state", v)
		}
	}
	nb, _ := s.Body("new", c)
	if !strings.Contains(nb, "In Progress") {
		t.Error("new lost the In Progress rule")
	}
	pb, _ := s.Body("plan", c)
	if !strings.Contains(pb, "wi update") {
		t.Error("plan does not move the ticket")
	}
}

func TestDelegateBodyHasTheRulesAndReports(t *testing.T) {
	b, err := NewSkill().Body("delegate", Context{Session: "main-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sonnet", "round", "3", "READY", "BLOCKED", "worker wait", "--timeout 8m", "worker brief", "[you]", "[overseer]", "[daemon]", "main-1", "worker escalate", "oversire review reply"} {
		if !strings.Contains(b, want) {
			t.Errorf("delegate body has no %q", want)
		}
	}
}

func TestDoneBodyStopsTheWorker(t *testing.T) {
	b, err := NewSkill().Body("done", Context{})
	if err != nil || !strings.Contains(b, "oversire worker stop") {
		t.Fatal(err, b)
	}
	if strings.Index(b, "oversire worker stop") > strings.Index(b, "oversire ticket done") {
		t.Fatal("the stop comes after ticket done")
	}
}
