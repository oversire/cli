package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

type rows struct{ got []wire.EventDraft }

func (r *rows) Add(_ string, d wire.EventDraft) { r.got = append(r.got, d) }

func TestRecordedClaudeSession(t *testing.T) {
	now := time.Unix(1000, 0)
	book := sessions.NewBook("mbp", "", nil, func() time.Time { return now })
	r := &rows{}
	g := NewIngest(book, nil).WithEvents(r).WithClock(func() time.Time { now = now.Add(10 * time.Second); return now })
	f, err := os.Open("testdata/events/claude-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := g.Ingest(context.Background(), "claude", wire.HookEnv{}, sc.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	var kinds []string
	for _, d := range r.got {
		kinds = append(kinds, d.Kind)
	}
	want := []string{
		"session.started",
		"prompt.submitted", "turn.started",
		"wait.started", "wait.ended", // the permission for go test
		"subagent.ended",
		"wait.started", "wait.ended", // AskUserQuestion
		"turn.ended",
		"wait.started", // idle_prompt
		"wait.ended", "prompt.submitted", "turn.started",
		"turn.ended", "turn.interrupted", "prompt.submitted", "turn.started", // a prompt in an open turn stops it
		"context.compacted",
		"turn.ended", "wait.started", // the turn ends with a question
		"wait.ended", "session.ended",
	}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("kinds:\n%s\nwant:\n%s", strings.Join(kinds, " "), strings.Join(want, " "))
	}
	cat, err := wire.LoadEventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, d := range r.got {
		if p := cat.Problems(d); len(p) > 0 {
			t.Errorf("%s: %v", d.ID, p)
		}
		if ids[d.ID] {
			t.Errorf("the id %s repeats", d.ID)
		}
		ids[d.ID] = true
	}
	payload := func(i int) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(r.got[i].Payload, &m)
		return m
	}
	first := payload(8)
	if first["turn"] != 1.0 || first["tools"] != 9.0 || first["filesEdited"] != 2.0 || first["testsRun"] != 2.0 ||
		first["testsFailed"] != 1.0 || first["toolErrors"] != 1.0 || first["durationSec"] != 140.0 {
		t.Fatalf("turn 1: %v", first)
	}
	if d := r.got[0]; d.Actor != "claude/claude-opus-5-5" || d.ActorType != wire.ActorAgent {
		t.Fatalf("actor %+v", d)
	}
	if p := r.got[1]; !p.Private || p.Summary != "Add the events table." || p.ActorType != wire.ActorHuman || p.Actor != "" {
		t.Fatalf("prompt %+v", p)
	}
	if r.got[3].Corr != r.got[4].Corr || r.got[3].Corr != r.got[3].ID || payload(4)["reason"] != "permission" || r.got[4].ActorType != wire.ActorHuman {
		t.Fatalf("wait pair %+v %+v", r.got[3], r.got[4])
	}
	if payload(6)["question"] != "Which database?" || payload(6)["reason"] != "question" {
		t.Fatalf("question %v", payload(6))
	}
	if steer := payload(12); steer["steer"] != true || steer["turn"] != 2.0 {
		t.Fatalf("steer %v", steer)
	}
	if stopped := payload(13); stopped["interrupted"] != true || r.got[14].Corr != r.got[12].ID {
		t.Fatalf("interrupted %v", stopped)
	}
	if end := payload(21); end["turns"] != 3.0 || end["reason"] != "prompt_input_exit" || r.got[21].ActorType != wire.ActorHuman {
		t.Fatalf("end %v", end)
	}
}
