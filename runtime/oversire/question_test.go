package oversire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/wire"
)

const askJSON = `{"questions":[` +
	`{"question":"Which retry rule?","header":"Retry","options":[{"label":"Exponential","description":"1 s, 2 s"},{"label":"Fixed"}],"multiSelect":false},` +
	`{"question":"Which tests?","header":"Tests","options":[{"label":"Unit"},{"label":"Live"}],"multiSelect":true}]}`

// askScript asks AskUserQuestion in the first turn. The fake has no tool input, so the input goes in "question".
// With wait, the script waits for the decision; else it goes on after a short time.
func askScript(wait bool) string {
	input, _ := json.Marshal(askJSON)
	ask, _ := json.Marshal(map[string]any{"type": "permission", "id": "q-1", "tool": askTool, "question": json.RawMessage(input)})
	next := "read -r d"
	if !wait {
		next = "sleep 0.3"
	}
	return fmt.Sprintf(`read -r line
echo '%s'
%s
echo '{"type":"assistant","text":"went on"}'
echo '{"type":"result","subtype":"success"}'
while read -r l; do :; done`, ask, next)
}

// askHarness gives the fake permission its input and records each decision.
type askHarness struct {
	*agentwire.Runtime
	mu        sync.Mutex
	decisions []agentwire.Decision
}

func (h *askHarness) Start(ctx context.Context, req agentwire.StartRequest) (agentwire.Session, error) {
	s, err := h.Runtime.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan agentwire.Event)
	go func() {
		defer close(out)
		for ev := range s.Events() {
			if p := ev.Permission; p != nil && p.Tool == askTool {
				p.Input, p.Question = p.Question, ""
			}
			out <- ev
		}
	}()
	return &askSession{Session: s, h: h, events: out}, nil
}

func (h *askHarness) all() []agentwire.Decision {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]agentwire.Decision{}, h.decisions...)
}

type askSession struct {
	agentwire.Session
	h      *askHarness
	events chan agentwire.Event
}

func (s *askSession) Events() <-chan agentwire.Event { return s.events }

func (s *askSession) AnswerPermission(ctx context.Context, id string, d agentwire.Decision) error {
	s.h.mu.Lock()
	s.h.decisions = append(s.h.decisions, d)
	s.h.mu.Unlock()
	return s.Session.AnswerPermission(ctx, id, d)
}

func setupAsk(t *testing.T, script string) (fixture, *askHarness) {
	t.Helper()
	var h *askHarness
	f := setupWith(t, script, Options{}, func(aw *agentwire.Runtime) Harnesses {
		h = &askHarness{Runtime: aw}
		return h
	})
	return f, h
}

func TestAskUserQuestion(t *testing.T) {
	f, h := setupAsk(t, askScript(true))
	ctx := context.Background()
	ref := f.launch(t, "APP-8", runtime.Order{ID: "o1"})
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	q := s.Question
	if q == nil || q.ID != "q-1" || q.Round != 1 || len(q.Items) != 2 || s.Permission != nil {
		t.Fatalf("question %#v", s)
	}
	if it := q.Items[0]; it.ID != "q1" || it.Kind != wire.QuestionSingle || it.Header != "Retry" || !it.AllowsOther ||
		it.Options[0] != (wire.QuestionOption{Label: "Exponential", Detail: "1 s, 2 s"}) {
		t.Fatalf("item 1 %#v", it)
	}
	if it := q.Items[1]; it.Kind != wire.QuestionMulti || len(it.Options) != 2 {
		t.Fatalf("item 2 %#v", it)
	}

	wrong := wire.QuestionAnswer{ID: "q-9", Items: []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"Fixed"}}}}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &wrong}); !errors.Is(err, ErrAnswered) {
		t.Fatalf("wrong id: %v", err)
	}
	a := wire.QuestionAnswer{ID: "q-1", Note: "Keep it short.", Items: []wire.QuestionAnswerItem{
		{ID: "q1", Choices: []string{"Exponential"}},
		{ID: "q2", Choices: []string{"Unit", "Live"}, Other: "and lint"},
	}}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &a}); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Answer: &a}); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a repeat: %v", err)
	}
	d := h.all()
	if len(d) != 1 || !d[0].Allow {
		t.Fatalf("decisions %#v", d)
	}
	var got map[string]any
	if err := json.Unmarshal(d[0].UpdatedInput, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"Which retry rule?": "Exponential", "Which tests?": "Unit, Live, and lint"}
	notes := map[string]any{"Which retry rule?": map[string]any{"notes": "Keep it short."}}
	if fmt.Sprint(got["answers"]) != fmt.Sprint(want) || fmt.Sprint(got["annotations"]) != fmt.Sprint(notes) || got["questions"] == nil {
		t.Fatalf("updated input %s", d[0].UpdatedInput)
	}
	s = f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Question != nil || s.Summary != "went on" {
		t.Fatalf("after answer %#v", s)
	}
}

func TestAssistantClearsQuestion(t *testing.T) {
	f, h := setupAsk(t, askScript(false))
	ref := f.launch(t, "APP-9", runtime.Order{ID: "o1"})
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Question == nil {
		f.rec.mu.Lock()
		asked := false
		for _, r := range f.rec.all {
			asked = asked || r.Question != nil
		}
		f.rec.mu.Unlock()
		if !asked {
			t.Fatal("the question never showed")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, _ = f.rec.latest(ref.ID)
		if s.Question == nil && strings.Contains(s.Summary, "went on") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the question stays %#v", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.all()) != 0 {
		t.Fatal("no answer must go to the agent")
	}
}

func TestBadAskInputIsAPermission(t *testing.T) {
	f, _ := setupAsk(t, `read -r line
echo '{"type":"permission","id":"q-1","tool":"AskUserQuestion","question":"not json"}'
while read -r l; do :; done`)
	ref := f.launch(t, "APP-10", runtime.Order{ID: "o1"})
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingPermission)
	if s.Question != nil || s.Permission == nil || s.Permission.Tool != askTool {
		t.Fatalf("%#v", s)
	}
}
