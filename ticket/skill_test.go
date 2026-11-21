package ticket

import (
	"io/fs"
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
	if len(s.Verbs()) != 9 {
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
	eb, _ := s.Body("epic", c)
	for _, want := range []string{"oversire epic create", "oversire epic add", "oversire epic show", "### Plan with a subagent", "### Set the plan to Todo"} {
		if !strings.Contains(eb, want) {
			t.Errorf("epic body has no %q", want)
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
	if !strings.Contains(nb, "- Epic:") || !strings.Contains(nb, "oversire epic add") {
		t.Error("new has no epic step")
	}
	if !strings.Contains(main, "oversire project north-star") {
		t.Error("main has no north star command")
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
		for _, want := range []string{"oversire item status", "todo", "Todo", "backlog"} {
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
	if !strings.Contains(pb, "oversire item status <KEY> todo") {
		t.Error("plan does not move the ticket")
	}
}

// The bodies change items with oversire, so that they work on each tracker.
func TestBodiesUseOversireItem(t *testing.T) {
	s := NewSkill()
	for _, v := range append([]string{""}, s.Verbs()...) {
		b, err := s.Body(v, Context{})
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range []string{"wi update", "wi list", "wi relation", "attachment upload", "state list"} {
			if strings.Contains(b, old) {
				t.Errorf("%q still has %q", v, old)
			}
		}
		if strings.Contains(b, "Big Timer has no sub-items") {
			t.Errorf("%q says that Big Timer has no sub-items", v)
		}
	}
	main, _ := s.Body("", Context{})
	if !strings.Contains(main, "oversire item") {
		t.Error("main has no oversire item")
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

func TestBodiesHaveNoPlaneCLI(t *testing.T) {
	err := fs.WalkDir(NewSkill().Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(NewSkill().Files(), p)
		if strings.Contains(string(b), "plane-cli") {
			t.Errorf("%s names plane-cli", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGroomBodyUsesIntakeCommands(t *testing.T) {
	b, _ := NewSkill().Body("groom", Context{})
	for _, want := range []string{"oversire intake list", "oversire intake decline", "oversire intake duplicate", "oversire intake accept", "oversire intake edit", "--comment-file", "--status all"} {
		if !strings.Contains(b, want) {
			t.Errorf("groom body has no %q", want)
		}
	}
}

func TestPlanBodyUsesCreatedSinceAndIntake(t *testing.T) {
	b, _ := NewSkill().Body("plan", Context{})
	for _, want := range []string{"--created-since", "oversire intake list"} {
		if !strings.Contains(b, want) {
			t.Errorf("plan body has no %q", want)
		}
	}
}

func TestDoneBodyUsesVendorRun(t *testing.T) {
	b, _ := NewSkill().Body("done", Context{})
	last := -1
	for _, want := range []string{"oversire vendor run", "attachment list", "attachment delete", "oversire item attach"} {
		i := strings.Index(b, want)
		if i < 0 || i < last {
			t.Fatalf("done body has %q missing or out of order", want)
		}
		last = i
	}
}

func TestMainBodyNamesThePassThrough(t *testing.T) {
	b, _ := NewSkill().Body("", Context{})
	for _, want := range []string{"oversire vendor run", "oversire vendor ls", "oversire intake"} {
		if !strings.Contains(b, want) {
			t.Errorf("main body has no %q", want)
		}
	}
}
