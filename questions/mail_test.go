package questions

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// fakeMail is the coordinator mail. A reply sets the Reply of the ask.
type fakeMail struct {
	mu      sync.Mutex
	box     runtime.Mail
	replies []string
}

func (f *fakeMail) Mail(context.Context) (runtime.Mail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := runtime.Mail{Workers: f.box.Workers}
	for _, a := range f.box.Asks {
		out.Asks = append(out.Asks, a)
	}
	return out, nil
}

func (f *fakeMail) Reply(_ context.Context, ask, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, body)
	for i := range f.box.Asks {
		if f.box.Asks[i].ID == ask {
			f.box.Asks[i].Reply = &body
		}
	}
	return nil
}

// The body of an ask recorded from Orca 1.4.223.
const multiAsk = `{"round":2,"questions":[{"id":"q1","type":"multi","text":"Which tests?","options":["Unit","Live"]},{"id":"q2","type":"free","text":"Any note?"}]}`

func mailRig(t *testing.T, body string) (*rig, *fakeMail) {
	r := newRig(t)
	m := &fakeMail{box: runtime.Mail{Asks: []runtime.Ask{{ID: "msg_4492d9a3289a", Terminal: term, Body: body, At: t0}}, Workers: map[string]bool{term: true}}}
	r.ch.WithMail(m, nil)
	r.ch.readMail(context.Background())
	return r, m
}

func TestAskOfAWorker(t *testing.T) {
	r, m := mailRig(t, multiAsk)
	r.session(wire.PhaseWorking, "Bash", "")
	q := r.open(t)
	want := []wire.DaemonQuestionItem{
		{ID: "q1", Text: "Which tests?", Kind: wire.QuestionMulti, Options: []wire.QuestionOption{{Label: "Unit"}, {Label: "Live"}}, AllowsOther: true},
		{ID: "q2", Text: "Any note?", Kind: wire.QuestionFree, Options: []wire.QuestionOption{}},
	}
	if q.ID != "ask:msg_4492d9a3289a" || q.Via != wire.ViaAskReply || q.Source != wire.SourceMail || !q.Answerable || !reflect.DeepEqual(q.Items, want) {
		t.Fatalf("%+v", q)
	}
	if r.screen.reads != 0 {
		t.Fatal("an ask reads no screen")
	}
	a := wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"Unit"}, Other: "Smoke"}, {ID: "q2", Other: "None"}}, Note: "Thanks"}
	res, err := r.ch.Answer(context.Background(), q.ID, a)
	if err != nil || res.Result != wire.AnswerSent {
		t.Fatal(res, err)
	}
	body := `{"answers":{"q1":["Unit","Smoke"],"q2":"None"},"note":"Thanks","round":2}`
	if !reflect.DeepEqual(m.replies, []string{body}) {
		t.Fatal(m.replies)
	}
	r.ch.readMail(context.Background())
	if len(r.ch.Open()) != 0 {
		t.Fatal("an answered ask stays open")
	}
	if res, _ := r.ch.Answer(context.Background(), q.ID, a); res.Result != wire.AnswerAlreadyAnswered || len(m.replies) != 1 {
		t.Fatal(res, m.replies)
	}
}

func TestAskAnsweredElsewhere(t *testing.T) {
	r, m := mailRig(t, multiAsk)
	r.session(wire.PhaseWorking, "Bash", "")
	q := r.open(t)
	reply := `{"round":2,"answers":{"q1":["Live","Both"]},"note":"ok"}`
	m.box.Asks[0].Reply = &reply
	res, err := r.ch.Answer(context.Background(), q.ID, wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: "q2", Other: "x"}}})
	if err != nil || res.Result != wire.AnswerAlreadyAnswered || len(m.replies) != 0 {
		t.Fatal(res, err, m.replies)
	}
	g, err := r.ch.Given(context.Background(), q.ID)
	want := &wire.QuestionAnswer{ID: q.ID, Note: "ok", Items: []wire.QuestionAnswerItem{{ID: "q1", Choices: []string{"Live"}, Other: "Both"}}}
	if err != nil || !reflect.DeepEqual(g, want) {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestPlainAsk(t *testing.T) {
	r, m := mailRig(t, "May I delete the old cache dir?")
	r.session(wire.PhaseWorking, "Bash", "")
	q := r.open(t)
	if len(q.Items) != 1 || q.Items[0].ID != ItemReply || q.Items[0].Kind != wire.QuestionFree {
		t.Fatalf("%+v", q.Items)
	}
	a := wire.QuestionAnswer{ID: q.ID, Items: []wire.QuestionAnswerItem{{ID: ItemReply, Other: "Yes"}}, Note: "Keep a copy."}
	if res, err := r.ch.Answer(context.Background(), q.ID, a); err != nil || res.Result != wire.AnswerSent {
		t.Fatal(res, err)
	}
	if !reflect.DeepEqual(m.replies, []string{"Yes\n\nKeep a copy."}) {
		t.Fatal(m.replies)
	}
}

func TestWorkerTurnIsNotAQuestion(t *testing.T) {
	r := newRig(t)
	r.ch.WithMail(&fakeMail{}, func() map[string]bool { return map[string]bool{term: true} })
	r.session(wire.PhaseDone, "", "I read the code.\n\nShould I go on?")
	r.ch.Check(context.Background(), pane)
	if qs := r.ch.Open(); len(qs) != 0 {
		t.Fatalf("%+v", qs)
	}
}
