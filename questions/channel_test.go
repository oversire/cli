package questions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// fakeScreen gives frames. perSend: the next frame after each send (the flow files). Else the next frame after each read; the last frame stays.
type fakeScreen struct {
	mu      sync.Mutex
	frames  [][]string
	i       int
	perSend bool
	sends   []string
	enters  []bool
	reads   int
}

func (f *fakeScreen) Read(_ context.Context, _ string, _ int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if len(f.frames) == 0 {
		return nil, nil
	}
	s := f.frames[min(f.i, len(f.frames)-1)]
	if !f.perSend {
		f.i++
	}
	return s, nil
}

func (f *fakeScreen) Send(_ context.Context, _, text string, enter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, text)
	f.enters = append(f.enters, enter)
	if f.perSend {
		f.i++
	}
	return nil
}

func (f *fakeScreen) show(perSend bool, frames ...[]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames, f.i, f.perSend = frames, 0, perSend
}

func (f *fakeScreen) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sends...)
}

// fakeFiles gives one transcript file for each folder.
type fakeFiles struct{ file string }

func (f *fakeFiles) Tail(path string, n int) ([]byte, error) { return os.ReadFile(path) }
func (f *fakeFiles) Find(_ context.Context, cwd, _ string) ([]string, error) {
	if cwd == "/w" && f.file != "" {
		return []string{f.file}, nil
	}
	return nil, nil
}

type events struct {
	mu  sync.Mutex
	all []wire.ProviderEvent
}

func (e *events) Append(ev wire.ProviderEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = append(e.all, ev)
}

func (e *events) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.all)
}

const (
	pane     = "tab1:leaf1"
	term     = "term_form"
	flowTool = "toolu_01Eo8u8RwXETDbPyaRoFUVy8"
)

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

type rig struct {
	book   *sessions.Book
	screen *fakeScreen
	files  *fakeFiles
	log    *events
	ch     *Channel
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{book: sessions.NewBook("mbp", "", nil, func() time.Time { return t0 }), screen: &fakeScreen{}, files: &fakeFiles{}, log: &events{}}
	r.ch = NewChannel(r.book, r.screen, r.files, r.log, func() time.Time { return t0 })
	r.ch.Wait = 0
	return r
}

// session puts an Orca session into the book.
func (r *rig) session(state wire.SessionPhase, tool, message string) {
	p := sessions.Patch{ID: pane, Source: sessions.SourceHook, At: t0, Runtime: ptr(wire.RuntimeOrca), Harness: ptr("claude"),
		Terminal: ptr(term), Cwd: ptr("/w"), State: &state, Tool: &tool}
	if message != "" {
		p.LastMessage = &message
	}
	r.book.Apply(p)
}

func (r *rig) open(t *testing.T) wire.DaemonQuestion {
	t.Helper()
	r.ch.Check(context.Background(), pane)
	qs := r.ch.Open()
	if len(qs) != 1 {
		t.Fatalf("open: %+v", qs)
	}
	return qs[0]
}

func frame(t *testing.T, i int) []string {
	return screen(t, "flow-2.1.308-"+[]string{"00", "01", "02", "03", "04", "05", "06", "07", "08", "09", "10"}[i])
}

type flowKeys struct {
	Transcript string                    `json:"transcript"`
	Answer     []wire.QuestionAnswerItem `json:"answer"`
	Sends      []string                  `json:"sends"`
	Pages      []struct {
		First, Last int
		Answer      []wire.QuestionAnswerItem `json:"answer"`
		Sends       []string                  `json:"sends"`
	} `json:"pages"`
}

func readKeys(t *testing.T) flowKeys {
	data, err := os.ReadFile(filepath.Join(screensDir, "flow-2.1.308.keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	var k flowKeys
	if err := json.Unmarshal(data, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestFormFromTheTranscriptSendsTheRecordedKeys(t *testing.T) {
	r := newRig(t)
	k := readKeys(t)
	r.files.file = filepath.Join(screensDir, k.Transcript)
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.screen.show(true, frame(t, 0))
	q := r.open(t)
	if q.ID != "form:"+flowTool || q.Source != wire.SourceTranscript || !q.Answerable || len(q.Items) != 2 {
		t.Fatalf("question: %+v", q)
	}
	var frames [][]string
	for i := 0; i <= 10; i++ {
		frames = append(frames, frame(t, i))
	}
	r.screen.show(true, frames...)
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: k.Answer})
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatalf("answer: %+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, k.Sends) {
		t.Errorf("sends %q, want %q", got, k.Sends)
	}
	for _, e := range r.screen.enters {
		if e {
			t.Error("a form key has no Enter")
		}
	}
	if len(r.ch.Open()) != 0 {
		t.Error("the question is still open")
	}
	again, _ := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: k.Answer})
	if again.Result != wire.AnswerAlreadyAnswered {
		t.Errorf("a second answer: %+v", again)
	}
	if g, _ := r.ch.Given(context.Background(), q.ID); g == nil || !reflect.DeepEqual(g.Items, k.Answer) {
		t.Errorf("given: %+v", g)
	}
}

// With no transcript, the channel answers one page at a time: Store, then Tests, then the review page.
func TestFormFromTheScreenIsAnsweredPageByPage(t *testing.T) {
	r := newRig(t)
	r.ch.tr = nil
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	for _, p := range readKeys(t).Pages {
		r.screen.show(true, frame(t, p.First))
		q := r.open(t)
		if q.Source != wire.SourceScreen || len(q.Items) != 1 || q.Items[0].ID != p.Answer[0].ID {
			t.Fatalf("page %d: %+v", p.First, q)
		}
		if !strings.HasPrefix(q.ID, "form:"+pane+":") {
			t.Errorf("page id %s", q.ID)
		}
		var frames [][]string
		for i := p.First; i <= p.Last; i++ {
			frames = append(frames, frame(t, i))
		}
		r.screen.show(true, frames...)
		before := len(r.screen.sent())
		res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: p.Answer})
		if err != nil || res.Result != wire.AnswerSent {
			t.Fatalf("page %d: %+v %v", p.First, res, err)
		}
		if got := r.screen.sent()[before:]; !reflect.DeepEqual(got, p.Sends) {
			t.Errorf("page %d sends %q, want %q", p.First, got, p.Sends)
		}
		if p.Answer[0].ID == ReviewID && q.Items[0].Why != "Which store? → JSON\nWhich tests? → Unit, perf" {
			t.Errorf("review why %q", q.Items[0].Why)
		}
	}
}

func TestFormAnsweredInTheTerminalSendsNoKey(t *testing.T) {
	r := newRig(t)
	r.files.file = filepath.Join(screensDir, "ask-flow-open.jsonl")
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.screen.show(true, frame(t, 0))
	q := r.open(t)
	r.screen.show(true, frame(t, 10))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{
		{ID: "q1", Choices: []string{"JSON"}}, {ID: "q2", Choices: []string{"Unit"}}}})
	if err != nil || res.Result != wire.AnswerAlreadyAnswered {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.screen.sent()) != 0 {
		t.Errorf("sends %q", r.screen.sent())
	}
}

func TestFormThatChangesStopsTheKeys(t *testing.T) {
	r := newRig(t)
	r.files.file = filepath.Join(screensDir, "ask-flow-open.jsonl")
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.screen.show(true, frame(t, 0))
	q := r.open(t)
	r.screen.show(false, frame(t, 0), screen(t, "form-single-2.1.308"))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{
		{ID: "q1", Choices: []string{"JSON"}}, {ID: "q2", Choices: []string{"Unit"}}}})
	if err != nil || res.Result != wire.AnswerError || res.Code != wire.CodeFormChanged {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("no key after the other form showed: %q", got)
	}
}

func TestBadAnswer(t *testing.T) {
	r := newRig(t)
	r.files.file = filepath.Join(screensDir, "ask-flow-open.jsonl")
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.screen.show(true, frame(t, 0))
	q := r.open(t)
	_, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"Redis"}}}})
	if err == nil || !strings.Contains(err.Error(), ErrBadAnswer.Error()) {
		t.Errorf("err %v", err)
	}
	if res, _ := r.ch.Answer(context.Background(), "form:nothing", wire.QuestionAnswer{}); res.Result != wire.AnswerGone {
		t.Errorf("unknown id: %+v", res)
	}
}

// cursorOn gives the screen with the cursor on choice n.
func cursorOn(lines []string, n int) []string {
	out := make([]string, len(lines))
	for i, row := range lines {
		r := []rune(row)
		if _, _, ok := option(string(r)); ok {
			for j, c := range r {
				if c == '❯' {
					r[j] = ' '
				}
			}
		}
		if m, _, ok := option(string(r)); ok && m == n {
			for j, c := range r {
				if c >= '0' && c <= '9' {
					if j >= 2 {
						r[j-2] = '❯'
					}
					break
				}
			}
		}
		out[i] = string(r)
	}
	return out
}

const command = "git -C . status --short > status.txt"

func TestPermissionFromTheScreen(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "Bash", "")
	r.book.Apply(sessions.Patch{ID: pane, Source: sessions.SourceHook, At: t0, ToolInput: ptr(command)})
	r.screen.show(false, screen(t, "permission-bash-2.1.308"))
	q := r.open(t)
	if q.Via != wire.ViaPermission || q.Source != wire.SourceScreen || !strings.HasPrefix(q.ID, "form:"+pane+":") {
		t.Fatalf("question: %+v", q)
	}
	if it := q.Items[0]; it.ID != ItemPermission || it.Header != "Bash" || it.Text != command || it.Kind != wire.QuestionSingle || len(it.Options) != 4 {
		t.Fatalf("item: %+v", it)
	}

	// The transcript gives the call.
	dir := t.TempDir()
	file := filepath.Join(dir, "s.jsonl")
	_ = os.WriteFile(file, []byte(`{"message":{"content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"`+command+`"}}]}}`), 0o600)
	r.files.file = file
	r.ch.set(wire.DaemonSession{ID: pane}, nil, "")
	if q := r.open(t); q.ID != "form:toolu_bash" {
		t.Errorf("id %s", q.ID)
	}
}

func TestTextForAPermission(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "Bash", "")
	bash := screen(t, "permission-bash-2.1.308")
	r.screen.show(false, bash)
	q := r.open(t)
	typing := screen(t, "permission-amend-typing-2.1.308")
	r.screen.show(false, bash, bash, cursorOn(bash, 2), cursorOn(bash, 3), cursorOn(bash, 4), typing, typing, screen(t, "turn-question-2.1.308"))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemPermission, Other: "use git status without a file"}}})
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, []string{KeyDown, KeyDown, KeyDown, KeyTab, "use git status without a file", KeyEnter}) {
		t.Errorf("sends %q", got)
	}
}

func TestTextForAPlan(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "ExitPlanMode", "")
	plan := screen(t, "plan-approval-2.1.308")
	r.screen.show(false, plan)
	q := r.open(t)
	if q.Items[0].ID != ItemPlan || q.Items[0].Header != "Plan" {
		t.Fatalf("item: %+v", q.Items[0])
	}
	r.screen.show(false, plan, plan, cursorOn(plan, 4), screen(t, "plan-feedback-typing-2.1.308"), screen(t, "turn-question-2.1.308"))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemPlan, Other: "use one step only"}}})
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, []string{"4", "use one step only", KeyEnter}) {
		t.Errorf("sends %q", got)
	}
}

func TestPromptThatMovesStopsTheKeys(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "Bash", "")
	bash := screen(t, "permission-bash-2.1.308")
	r.screen.show(false, bash)
	q := r.open(t)
	r.screen.show(false, bash, bash, cursorOn(bash, 3))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemPermission, Other: "no"}}})
	if err != nil || res.Code != wire.CodeFormChanged {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, []string{KeyDown}) {
		t.Errorf("the user moved the cursor: %q", got)
	}
}

func TestPromptAnsweredInTheTerminalSendsNoKey(t *testing.T) {
	for _, after := range []string{"turn-question-2.1.308", "permission-edit-2.1.308"} {
		r := newRig(t)
		r.session(wire.PhaseWaiting, "Bash", "")
		r.screen.show(false, screen(t, "permission-bash-2.1.308"))
		q := r.open(t)
		r.screen.show(false, screen(t, after))
		res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemPermission, Choices: []string{"Yes"}}}})
		if err != nil || res.Result != wire.AnswerAlreadyAnswered {
			t.Errorf("%s: %+v %v", after, res, err)
		}
		if len(r.screen.sent()) != 0 {
			t.Errorf("%s: sends %q", after, r.screen.sent())
		}
	}
}

func hookEvent(t *testing.T, name string) hooks.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Name string `json:"hook_event_name"`
	}
	_ = json.Unmarshal(data, &head)
	return hooks.Event{Harness: "claude", Name: head.Name, Payload: data, At: t0.Add(time.Second), Session: pane}
}

func TestHookFormOpensAndCloses(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.ch.OnHook(hookEvent(t, "PreToolUse-AskUserQuestion"))
	qs := r.ch.Open()
	if len(qs) != 1 {
		t.Fatalf("open: %+v", qs)
	}
	q := qs[0]
	want := wire.DaemonQuestionItem{ID: "q1", Header: "Database", Text: "Which database?", Kind: wire.QuestionSingle, AllowsOther: true,
		Options: []wire.QuestionOption{{Label: "Postgres", Detail: "Use Postgres"}, {Label: "SQLite", Detail: "Use SQLite"}}}
	if q.ID != "form:toolu_01Q" || q.Source != wire.SourceHook || q.Via != wire.ViaForm || !q.Answerable || q.Terminal != term ||
		!reflect.DeepEqual(q.Items, []wire.DaemonQuestionItem{want}) || !q.AskedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("question: %+v", q)
	}
	if r.log.count() != 1 {
		t.Errorf("events: %d", r.log.count())
	}
	// A check keeps the exact form, with no read of the screen.
	r.ch.Check(context.Background(), pane)
	if r.screen.reads != 0 || len(r.ch.Open()) != 1 {
		t.Errorf("reads %d, open %d", r.screen.reads, len(r.ch.Open()))
	}

	r.ch.OnHook(hookEvent(t, "PostToolUse-AskUserQuestion"))
	if len(r.ch.Open()) != 0 {
		t.Fatal("PostToolUse closes the form")
	}
	g, err := r.ch.Given(context.Background(), "form:toolu_01Q")
	if err != nil || g == nil || !reflect.DeepEqual(g.Items, []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"Postgres"}}}) {
		t.Errorf("given: %+v %v", g, err)
	}
	if r.log.count() != 2 {
		t.Errorf("events: %d", r.log.count())
	}
	if res, _ := r.ch.Answer(context.Background(), "form:toolu_01Q", wire.QuestionAnswer{}); res.Result != wire.AnswerAlreadyAnswered {
		t.Errorf("a closed question: %+v", res)
	}
}

func TestHookPlanApproval(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "ExitPlanMode", "")
	r.ch.OnHook(hookEvent(t, "PreToolUse-ExitPlanMode"))
	q := r.ch.Open()[0]
	if q.ID != "form:toolu_01P" || q.Via != wire.ViaPermission || q.Items[0].ID != ItemPlan || q.Items[0].Kind != wire.QuestionApprove ||
		q.Items[0].Text != "1. Add the route.\n2. Add a test." || !q.Items[0].AllowsOther {
		t.Fatalf("question: %+v", q)
	}
	// The screen gives the choices. Allow picks the last "Yes" choice.
	plan := screen(t, "plan-approval-2.1.308")
	r.screen.show(false, plan, plan, plan, screen(t, "turn-question-2.1.308"))
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemPlan, Choices: []string{wire.OptionAllow}}}})
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatalf("%+v %v", res, err)
	}
	if got := r.screen.sent(); !reflect.DeepEqual(got, []string{"3"}) {
		t.Errorf("sends %q", got)
	}
}

func TestHookPermissionTakesTheChoicesFromTheScreen(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "Bash", "")
	r.ch.OnHook(hookEvent(t, "PermissionRequest"))
	q := r.ch.Open()[0]
	if q.Via != wire.ViaPermission || q.Source != wire.SourceHook || q.Items[0].Kind != wire.QuestionApprove || q.Items[0].Header != "Bash" ||
		q.Items[0].Text != "rm -rf build" || q.ID != "form:"+pane+":"+fnv("Bash\nrm -rf build") {
		t.Fatalf("question: %+v", q)
	}
	// Before the prompt shows, the hook question stays.
	r.screen.show(false, screen(t, "turn-question-2.1.308"))
	r.ch.Check(context.Background(), pane)
	if got := r.ch.Open()[0]; !reflect.DeepEqual(got, q) {
		t.Fatalf("changed: %+v", got)
	}
	r.screen.show(false, screen(t, "permission-bash-2.1.308"))
	r.ch.Check(context.Background(), pane)
	got := r.ch.Open()[0]
	if got.ID != q.ID || got.Items[0].Kind != wire.QuestionSingle || len(got.Items[0].Options) != 4 || got.Items[0].Text != "rm -rf build" {
		t.Fatalf("merged: %+v", got)
	}
	// The prompt left the screen: the user answered it in the terminal.
	r.screen.show(false, screen(t, "no-form-answered"))
	r.ch.Check(context.Background(), pane)
	if open := r.ch.Open(); len(open) != 1 || open[0].Via != wire.ViaWaiting {
		t.Fatalf("after: %+v", open)
	}
}

func TestHookPermissionFindsTheCallInTheTranscript(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "Bash", "")
	file := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(file, []byte(`{"message":{"content":[{"type":"tool_use","id":"toolu_rm","name":"Bash","input":{"command":"rm -rf build"}}]}}`), 0o600)
	ev := hookEvent(t, "PermissionRequest")
	var m map[string]any
	_ = json.Unmarshal(ev.Payload, &m)
	m["transcript_path"] = file
	ev.Payload, _ = json.Marshal(m)
	r.ch.OnHook(ev)
	if q := r.ch.Open()[0]; q.ID != "form:toolu_rm" {
		t.Errorf("id %s", q.ID)
	}
}

func TestStopGivesATurnQuestion(t *testing.T) {
	r := newRig(t)
	msg := "I found two options.\n\nShould I use Postgres?"
	r.session(wire.PhaseDone, "", msg)
	r.ch.OnHook(hookEvent(t, "Stop-question"))
	qs := r.ch.Open()
	if len(qs) != 1 {
		t.Fatalf("open: %+v", qs)
	}
	q := qs[0]
	want := wire.DaemonQuestionItem{ID: ItemReply, Text: "Should I use Postgres?", Why: msg, Kind: wire.QuestionFree, Options: []wire.QuestionOption{}}
	if q.Via != wire.ViaTurn || q.ID != "turn:"+pane+":"+"1792659600000" || !q.Answerable || !reflect.DeepEqual(q.Items, []wire.DaemonQuestionItem{want}) {
		t.Fatalf("question: %+v", q)
	}
	r.ch.Check(context.Background(), pane)
	if r.screen.reads != 0 || len(r.ch.Open()) != 1 || r.ch.Open()[0].ID != q.ID {
		t.Fatal("a check keeps the turn question with no screen read")
	}
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemReply, Other: "Yes, use it"}}})
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatalf("%+v %v", res, err)
	}
	if !reflect.DeepEqual(r.screen.sends, []string{"Yes, use it"}) || !r.screen.enters[0] {
		t.Errorf("sends %q %v", r.screen.sends, r.screen.enters)
	}
	r.ch.Check(context.Background(), pane)
	if len(r.ch.Open()) != 0 {
		t.Error("a sent turn question does not open again")
	}
}

func TestTurnAnsweredInTheTerminal(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseDone, "", "Added the line.\n\nDo you want a third line?")
	q := r.open(t)
	r.session(wire.PhaseWorking, "", "")
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemReply, Other: "Yes"}}})
	if err != nil || res.Result != wire.AnswerAlreadyAnswered || len(r.screen.sent()) != 0 {
		t.Fatalf("%+v %v %q", res, err, r.screen.sent())
	}
	r.ch.Check(context.Background(), pane)
	if len(r.ch.Open()) != 0 {
		t.Error("a working session has no question")
	}
}

func TestWaitingWithAnEmptyScreen(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "WebFetch", "")
	q := r.open(t)
	if q.Via != wire.ViaWaiting || q.Answerable || q.ID != "wait:"+pane+":1792659600000" || q.Items[0].Text != WaitingText {
		t.Fatalf("question: %+v", q)
	}
	res, _ := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemWait, Other: "x"}}})
	if res.Result != wire.AnswerError || res.Code != CodeNotAnswerable || len(r.screen.sent()) != 0 {
		t.Errorf("%+v", res)
	}
}

func TestCLISessionIsNotAnswerable(t *testing.T) {
	r := newRig(t)
	r.book.Apply(sessions.Patch{ID: pane, Source: sessions.SourceHook, At: t0, Runtime: ptr(wire.RuntimeCLI), Harness: ptr("claude"),
		Cwd: ptr("/w"), State: ptr(wire.PhaseWaiting)})
	r.ch.OnHook(hookEvent(t, "PreToolUse-AskUserQuestion"))
	q := r.ch.Open()[0]
	if q.Answerable || q.Items[0].Why != "Answer in the terminal on mbp." {
		t.Fatalf("question: %+v", q)
	}
	r.ch.Check(context.Background(), pane)
	if r.screen.reads != 0 || r.ch.Open()[0].ID != q.ID {
		t.Error("a cli session keeps the hook question with no read")
	}
}

func TestRunChecksOnBookChanges(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.ch.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // Run subscribes to the book
	r.session(wire.PhaseDone, "", "Done.\n\nShould I push?")
	deadline := time.Now().Add(2 * time.Second)
	for len(r.ch.Open()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if qs := r.ch.Open(); len(qs) != 1 || qs[0].Via != wire.ViaTurn {
		t.Errorf("open: %+v", qs)
	}
	r.session(wire.PhaseWorking, "", "")
	deadline = time.Now().Add(2 * time.Second)
	for len(r.ch.Open()) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.ch.Open()) != 0 {
		t.Error("the question stays open for a working session")
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestRoutes(t *testing.T) {
	r := newRig(t)
	r.session(wire.PhaseWaiting, "AskUserQuestion", "")
	r.ch.OnHook(hookEvent(t, "PreToolUse-AskUserQuestion"))
	mux := http.NewServeMux()
	for _, rt := range r.ch.Routes() {
		mux.HandleFunc(rt.Pattern, rt.Handle)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/questions")
	if err != nil {
		t.Fatal(err)
	}
	var page wire.DaemonQuestionsPage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if len(page.Questions) != 1 || page.Questions[0].ID != "form:toolu_01Q" {
		t.Fatalf("page: %+v", page)
	}

	resp, _ = http.Post(srv.URL+"/v1/questions/form:gone/answer", "application/json", strings.NewReader(`{"id":"form:gone","items":[]}`))
	var res wire.DaemonAnswerResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res.Result != wire.AnswerGone {
		t.Errorf("gone: %d %+v", resp.StatusCode, res)
	}

	resp, _ = http.Post(srv.URL+"/v1/questions/form:toolu_01Q/answer", "application/json", strings.NewReader(`{"id":"form:toolu_01Q","items":[{"id":"q9","choices":[]}]}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad answer: %d", resp.StatusCode)
	}

	resp, _ = http.Get(srv.URL + "/v1/questions/form:toolu_01Q/given")
	body := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if v, ok := body["answer"]; !ok || v != nil {
		t.Errorf("given: %+v", body)
	}
}
