package runbook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oversire/cli/wire"
)

func fixtures(t *testing.T, kind string) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "contract", "fixtures", kind, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures for %s: %v", kind, err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = data
	}
	return out
}

func TestFixturesAreValid(t *testing.T) {
	for name, data := range fixtures(t, "runbook") {
		var b wire.Runbook
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(name, err)
		}
		if p := Validate(b); len(p) > 0 {
			t.Errorf("runbook/%s: %+v", name, p)
		}
	}
	for name, data := range fixtures(t, "runbook-start") {
		var s wire.RunbookStart
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(name, err)
		}
		if p := validateStart(s); len(p) > 0 {
			t.Errorf("runbook-start/%s: %+v", name, p)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := func(id string) wire.RunbookStep {
		return wire.RunbookStep{ID: id, Name: id, Agent: "claude", Prompt: "Do {key}. Then run {done}."}
	}
	review := func(id, fix string, rounds int) wire.RunbookStep {
		s := ok(id)
		s.Review = &wire.Review{Fix: fix, Rounds: rounds}
		return s
	}
	with := func(s wire.RunbookStep, f func(*wire.RunbookStep)) wire.RunbookStep { f(&s); return s }
	cases := []struct {
		name  string
		b     wire.Runbook
		wants []string // "step: text part"
	}{
		{"good", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{ok("a"), review("b", "c", 2), ok("c")}}, nil},
		{"no name and no step", wire.Runbook{}, []string{": needs a name", ": at least one step"}},
		{"bad id", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{ok("Plan_1")}}, []string{"Plan_1: is not valid"}},
		{"same id", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{ok("a"), ok("a")}}, []string{"a: Two steps"}},
		{"no done", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{with(ok("a"), func(s *wire.RunbookStep) { s.Prompt = "Do {key}." })}}, []string{"a: no {done}"}},
		{"unknown placeholder", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{with(ok("a"), func(s *wire.RunbookStep) { s.Prompt += " {branch} {Key}" })}}, []string{"a: unknown placeholder {branch}"}},
		{"first pauses", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{with(ok("a"), func(s *wire.RunbookStep) { s.PauseBefore = true })}}, []string{"a: cannot pause"}},
		{"rounds", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{review("a", "b", 6), ok("b"), review("c", "d", -1), ok("d")}}, []string{"a: must be 0 to 5", "c: must be 0 to 5"}},
		{"fix before", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{ok("b"), review("a", "b", 1)}}, []string{"a: must be a later step"}},
		{"no fix step", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{review("a", "x", 1)}}, []string{"a: must be a later step"}},
		{"fix is a review", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{review("a", "b", 1), review("b", "c", 1), ok("c")}}, []string{"a: cannot be a review"}},
		{"one fix for two reviews", wire.Runbook{Name: "A", Steps: []wire.RunbookStep{review("a", "c", 1), review("b", "c", 1), ok("c")}}, []string{`b: is also the fix step of "a"`}},
	}
	for _, c := range cases {
		got := Validate(c.b)
		if len(got) != len(c.wants) {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		for i, w := range c.wants {
			step, text, _ := strings.Cut(w, ": ")
			if got[i].Step != step || !strings.Contains(got[i].Text, text) {
				t.Errorf("%s: %+v, want %s", c.name, got[i], w)
			}
		}
	}
}

func TestValidateStart(t *testing.T) {
	s := wire.RunbookStart{Runbook: linear(), Agents: map[string]wire.AgentLaunch{"claude": {CLI: "claude"}}, StartAt: "ship"}
	got := validateStart(s)
	want := []Problem{
		{Text: "The start needs an item with a key."},
		{Text: "The start needs a repo."},
		{Step: "ship", Text: `The runbook has no step "ship" to start at.`},
		{Step: "build", Text: `The agent "claude-max" is not in the agents of the start.`},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%+v, want %+v", got[i], want[i])
		}
	}
	data, _ := json.Marshal(Problem{Text: "x"})
	if string(data) != `{"step":"","text":"x"}` {
		t.Fatal(string(data))
	}
}

func TestRender(t *testing.T) {
	r := &wire.RunbookRun{ID: "rb_0a1b2c3d", Item: item, Title: "Retry a sync", Notes: "Keep the API.", Runbook: small(2),
		Steps: []wire.StepRun{
			{Step: "implement", Round: 1, State: wire.StepComplete, Note: "Store first.", Ended: &t0},
			{Step: "review", Round: 1, State: wire.StepGaps, Note: "No test.", Ended: &t0},
			{Step: "fixes", Round: 1, State: wire.StepComplete, Ended: &t0},
			{Step: "review", Round: 2, State: wire.StepGaps, Note: "Still no test.", Ended: &t0},
			{Step: "fixes", Round: 2, State: wire.StepWorking, Note: "not ended"},
		}}
	step := wire.RunbookStep{ID: "fixes", Prompt: "{key} {url} {title} {item} {workspace} {project} {round} {worktree} {branch} {done} | {notes}"}
	got := render(r, step, 2)
	want := "OVERSIRE-341 https://app.plane.so/ocral/browse/OVERSIRE-341/ Retry a sync i1 ocral OVERSIRE 2 .wt/oversire-341 {branch} " +
		"oversire step done --run rb_0a1b2c3d --step fixes | Keep the API.; Implement: Store first.; Review Implementation: No test.; Review Implementation (round 2): Still no test."
	if got != want {
		t.Fatalf("\n%s\n%s", got, want)
	}
	r.Title = ""
	if got := render(r, wire.RunbookStep{Prompt: "{title}"}, 1); got != "OVERSIRE-341" {
		t.Fatal(got)
	}
}
