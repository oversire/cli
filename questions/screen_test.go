package questions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oversire/cli/wire"
)

var screensDir = filepath.Join("..", "contract", "fixtures", "claude-screens")

func screen(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(screensDir, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

// expectOf has the shape of the .expect.json files that ClaudeScreensTests in the app writes.
func expectOf(t *testing.T, s []string) any {
	t.Helper()
	out := map[string]any{"form": nil, "prompt": nil}
	if f, ok := ParseForm(s); ok {
		form := map[string]any{"headers": f.Headers, "items": f.Items, "review": f.Review}
		if f.Cursor != "" {
			form["cursor"] = f.Cursor
		}
		if len(f.Boxes) > 0 {
			form["boxes"] = f.Boxes
		}
		out["form"] = form
	}
	if p, ok := ParsePrompt(s); ok {
		opts := []map[string]any{}
		for _, o := range p.Options {
			opts = append(opts, map[string]any{"n": o.N, "label": o.Label})
		}
		prompt := map[string]any{"kind": p.Kind, "title": p.Title, "body": p.Body, "question": p.Question, "options": opts, "amending": p.Amending}
		if p.Cursor != "" {
			prompt["cursor"] = p.Cursor
		}
		if p.Typed != 0 {
			prompt["typed"] = p.Typed
		}
		out["prompt"] = prompt
	}
	return canonical(t, out)
}

func canonical(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	return back
}

func TestScreensMatchTheExpectFiles(t *testing.T) {
	names, _ := filepath.Glob(filepath.Join(screensDir, "*.txt"))
	if len(names) < 30 {
		t.Fatalf("only %d screens", len(names))
	}
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		data, err := os.ReadFile(filepath.Join(screensDir, name+".expect.json"))
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		if got := expectOf(t, screen(t, name)); !reflect.DeepEqual(got, want) {
			g, _ := json.MarshalIndent(got, "", "  ")
			t.Errorf("%s:\n got %s\nwant %s", name, g, data)
		}
	}
}

func TestNoForm(t *testing.T) {
	for _, name := range []string{"no-form-answered", "false-footer"} {
		if _, ok := ParseForm(screen(t, name)); ok {
			t.Errorf("%s gives a form", name)
		}
	}
	if _, ok := ParseForm(nil); ok {
		t.Error("an empty screen gives a form")
	}
	if _, ok := ParsePrompt(nil); ok {
		t.Error("an empty screen gives a prompt")
	}
}

func TestFormKeys(t *testing.T) {
	str := func(s string) *string { return &s }
	item := func(id, text string, kind wire.QuestionKind, labels ...string) wire.DaemonQuestionItem {
		it := wire.DaemonQuestionItem{ID: id, Text: text, Kind: kind, AllowsOther: len(labels) > 0}
		for _, l := range labels {
			it.Options = append(it.Options, wire.QuestionOption{Label: l})
		}
		return it
	}
	plan := func(items []wire.DaemonQuestionItem, answers ...wire.QuestionAnswerItem) []Step {
		return FormKeys(wire.DaemonQuestion{ID: "form:t", Items: items}, wire.QuestionAnswer{ID: "form:t", Items: answers}, false)
	}
	keys := func(steps []Step) []string {
		out := []string{}
		for _, s := range steps {
			out = append(out, s.Keys)
		}
		return out
	}
	same := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}

	single := plan([]wire.DaemonQuestionItem{item("q1", "Pick", wire.QuestionSingle, "A", "B")}, wire.QuestionAnswerItem{ID: "q1", Choices: []string{"B"}})
	same("single", single, []Step{{Question: str("Pick"), Keys: "2"}})

	other := plan([]wire.DaemonQuestionItem{item("q1", "Pick", wire.QuestionSingle, "A", "B")}, wire.QuestionAnswerItem{ID: "q1", Other: " Neither,\nsee the logs "})
	same("other keys", keys(other), []string{"3", "Neither, see the logs", KeyEnter})
	same("other cursors", []string{other[0].Cursor, other[1].Cursor, other[2].Cursor}, []string{"", "3.", "3."})

	multi := plan([]wire.DaemonQuestionItem{item("q1", "Tests", wire.QuestionMulti, "Unit", "uishots", "Live")},
		wire.QuestionAnswerItem{ID: "q1", Choices: []string{"Live", "Unit"}})
	same("multi keys", keys(multi), []string{"3", "1", KeyDown, KeyDown, KeyDown, KeyDown, KeyEnter, "1"})
	same("multi toggle", multi[0].Boxes, map[int]bool{3: false})
	same("multi submit", multi[6].Cursor, "Submit")
	same("multi boxes", multi[6].Boxes, map[int]bool{1: true, 2: false, 3: true, 4: false})
	same("multi review", multi[7], Step{Cursor: "1. Submit answers", Keys: "1"})

	free := plan([]wire.DaemonQuestionItem{item("q1", "Why?", wire.QuestionFree)}, wire.QuestionAnswerItem{ID: "q1", Other: "Disk"})
	same("free", keys(free), []string{"Disk", KeyEnter})

	three := plan([]wire.DaemonQuestionItem{item("q1", "Store", wire.QuestionSingle, "SQLite", "JSON"), item("q2", "Tests", wire.QuestionMulti, "Unit", "Live"),
		item("q3", "Mode", wire.QuestionSingle, "Fast", "Safe")},
		wire.QuestionAnswerItem{ID: "q1", Choices: []string{"JSON"}}, wire.QuestionAnswerItem{ID: "q2", Choices: []string{"Unit"}, Other: "perf"},
		wire.QuestionAnswerItem{ID: "q3", Choices: []string{"Safe"}})
	same("three", keys(three), []string{"2", "1", "3", KeyDown, KeyDown, "perf", KeyDown, KeyEnter, "2", "1"})
	same("three next", three[7].Cursor, "Next")
	same("three boxes", three[7].Boxes, map[int]bool{1: true, 2: false, 3: true})
	if three[9].Question != nil {
		t.Error("the last step is the review page")
	}
}

func TestStepHolds(t *testing.T) {
	f, _ := ParseForm(screen(t, "flow-2.1.308-04"))
	q := "Which tests?"
	for _, c := range []struct {
		step Step
		want bool
	}{
		{Step{Question: &q, Cursor: "2."}, true},
		{Step{Question: &q, Cursor: "Next|2."}, true},
		{Step{Question: &q, Cursor: "3."}, false},
		{Step{Question: &q, Boxes: map[int]bool{1: true, 4: true}}, true},
		{Step{Question: &q, Boxes: map[int]bool{2: true}}, false},
		{Step{Question: &q, Boxes: map[int]bool{9: false}}, false},
		{Step{Cursor: "2."}, false},
	} {
		if got := c.step.Holds(f); got != c.want {
			t.Errorf("%+v: got %v", c.step, got)
		}
	}
}

func TestPromptItems(t *testing.T) {
	bash, ok := ParsePrompt(screen(t, "permission-bash-2.1.308"))
	if !ok {
		t.Fatal("no prompt")
	}
	it := bash.Item(ItemPermission, "Bash", "git -C . status --short > status.txt")
	if it.Header != "Bash" || it.Text != "git -C . status --short > status.txt" || it.Why != "Do you want to proceed?" || len(it.Options) != 4 || !it.AllowsOther {
		t.Errorf("bash item: %+v", it)
	}
	write, _ := ParsePrompt(screen(t, "permission-write-2.1.308"))
	if it := write.Item(ItemPermission, "Write", "/private/tmp/formlab/notes.md"); it.Header != "Write" || it.Text != "notes.md" {
		t.Errorf("write item: %+v", it)
	}
	plan, _ := ParsePrompt(screen(t, "plan-approval-2.1.308"))
	if it := plan.Item(ItemPlan, "ExitPlanMode", ""); it.Header != "Plan" || it.Text != `Append "world" to notes.md` {
		t.Errorf("plan item: %+v", it)
	}
}

func TestTranscript(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(screensDir, "ask-open.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	open, ok := LastAsk(data)
	if !ok || !open.Open() || open.ID != "toolu_01DmZe1tnBzub1Aa8VinKYuf" || len(open.Items) != 2 || open.Items[1].Kind != wire.QuestionMulti {
		t.Fatalf("open: %+v", open)
	}
	f, _ := ParseForm(screen(t, "form-local"))
	if !open.Matches(f) {
		t.Error("the form does not match the call")
	}
	if open.Matches(Form{Headers: []string{"Other"}, Items: []wire.DaemonQuestionItem{{Text: "Else?"}}}) {
		t.Error("another form matches the call")
	}
	data, _ = os.ReadFile(filepath.Join(screensDir, "ask-answered.jsonl"))
	done, ok := LastAsk(data[40:])
	if !ok || done.Open() {
		t.Fatal("a cut first line is skipped, and the call has a result")
	}
	got := done.Answer("x")
	want := &wire.QuestionAnswer{ID: "x", Items: []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"SQLite"}}, {ID: "q2", Choices: []string{"Unit", "Live"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answer: %+v", got)
	}
	free := Ask{Items: []wire.DaemonQuestionItem{{ID: "q1", Text: "Pick", Options: []wire.QuestionOption{{Label: "A"}}}},
		Answers: map[string]string{"Pick": "Neither, see the logs"}, Notes: []string{"Look at run 7."}}
	if a := free.Answer("x"); a.Items[0].Other != "Neither, see the logs" || a.Note != "Look at run 7." {
		t.Errorf("free text: %+v", a)
	}
}

func TestOpenTool(t *testing.T) {
	jsonl := []byte(`{"message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}
{"message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{}}]}}`)
	if got := OpenTool("Bash", "", jsonl); got != "t2" {
		t.Errorf("got %q", got)
	}
	if got := OpenTool("Bash", "", jsonl[:160]); got != "" {
		t.Errorf("the call has a result: %q", got)
	}
	if got := OpenTool("Edit", "", jsonl); got != "" {
		t.Errorf("no Edit call: %q", got)
	}
	if got := OpenTool("Bash", "rm -rf build", jsonl); got != "" {
		t.Errorf("the open call has another command: %q", got)
	}
}

// The cases of TurnQuestionTests in the app. sessions.TestTurnQuestion has the full table.
func TestTurnQuestion(t *testing.T) {
	for in, want := range map[string]string{
		"I fixed the test.\n\nShould I push the branch?\n": "Should I push the branch?",
		"The rule:\n\n```\nwhy?\n```":                      "",
		"All tests pass.":                                  "",
	} {
		got, ok := TurnQuestion(in)
		if got != want || ok != (want != "") {
			t.Errorf("TurnQuestion(%q) = %q, %v", in, got, ok)
		}
	}
}

func TestFNVMatchesTheApp(t *testing.T) {
	// OrcaRuntime.fnv("") in the app is the FNV-1a offset basis.
	if got := fnv(""); got != "cbf29ce484222325" {
		t.Errorf("got %s", got)
	}
	if got := fnv("a"); got != "af63dc4c8601ec8c" {
		t.Errorf("got %s", got)
	}
}
