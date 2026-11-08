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
	if len(s.Verbs()) != 5 {
		t.Fatalf("verbs %v", s.Verbs())
	}
	for _, v := range s.Verbs() {
		b, err := s.Body(v, c)
		if err != nil || strings.TrimSpace(b) == "" || strings.Contains(b, "{{") {
			t.Errorf("%s: %v %q", v, err, b)
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
