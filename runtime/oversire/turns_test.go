package oversire

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// tools uses one tool in each turn, then answers.
const tools = `n=0
while read -r line; do
  case "$line" in
    *'"type":"user"'*) n=$((n+1))
      echo "{\"type\":\"tool\",\"id\":\"t$n\",\"name\":\"Bash\",\"input\":\"npm test $n\",\"output\":\"\",\"status\":\"started\"}"
      echo "{\"type\":\"tool\",\"id\":\"t$n\",\"name\":\"Bash\",\"input\":\"npm test $n\",\"output\":\"ok\",\"status\":\"completed\"}"
      echo "{\"type\":\"assistant\",\"text\":\"turn $n\"}"
      echo '{"type":"result","subtype":"success"}';;
  esac
done`

type brief struct {
	seq  int
	role wire.TurnRole
	text string
}

func briefs(ts []wire.SessionTurn) []brief {
	out := make([]brief, len(ts))
	for i, t := range ts {
		text := t.Text
		if t.Tool != nil {
			text = t.Tool.Name + " " + string(t.Tool.Status) + " " + t.Tool.Input + " " + t.Tool.Summary
		}
		out[i] = brief{t.Seq, t.Role, text}
	}
	return out
}

func equal(t *testing.T, got []wire.SessionTurn, want ...brief) {
	t.Helper()
	b := briefs(got)
	if len(b) != len(want) {
		t.Fatalf("turns %+v, want %+v", b, want)
	}
	for i := range b {
		if b[i] != want[i] {
			t.Fatalf("turns %+v, want %+v", b, want)
		}
	}
}

func next(t *testing.T, live <-chan wire.SessionTurn) wire.SessionTurn {
	t.Helper()
	select {
	case tu, ok := <-live:
		if !ok {
			t.Fatal("the live turns ended")
		}
		return tu
	case <-time.After(5 * time.Second):
		t.Fatal("no live turn")
	}
	return wire.SessionTurn{}
}

func TestChatTurns(t *testing.T) {
	f := setup(t, tools, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref := f.launch(t, "APP-9", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)

	src, err := f.rt.Chat(ctx, wire.DaemonSession{ID: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	history, live, err := src.Turns(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, history,
		brief{1, wire.RoleUser, "go"},
		brief{2, wire.RoleTool, "Bash done npm test 1 ok"},
		brief{3, wire.RoleAssistant, "turn 1"})

	if err := src.Send(ctx, "more"); err != nil {
		t.Fatal(err)
	}
	var got []wire.SessionTurn
	for len(got) < 4 {
		got = append(got, next(t, live))
	}
	equal(t, got,
		brief{4, wire.RoleUser, "more"},
		brief{5, wire.RoleTool, "Bash running npm test 2 "},
		brief{5, wire.RoleTool, "Bash done npm test 2 ok"},
		brief{6, wire.RoleAssistant, "turn 2"})

	// after gives only the newer turns. The log keeps the last state of a tool turn.
	newer, _, err := src.Turns(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, newer, brief{5, wire.RoleTool, "Bash done npm test 2 ok"}, brief{6, wire.RoleAssistant, "turn 2"})

	if err := f.rt.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-live:
		if ok {
			t.Fatal("a turn after the stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the live turns did not end with the session")
	}

	// A new runtime reads the log: the same seqs, and no live turns.
	again := New(Options{DataDir: f.data, Fake: true}, agentwire.New(agentwire.Options{}), proc.Local{}, nil)
	src, _ = again.Chat(ctx, wire.DaemonSession{ID: ref.ID})
	history, live, err = src.Turns(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, history, brief{4, wire.RoleUser, "more"}, brief{5, wire.RoleTool, "Bash done npm test 2 ok"}, brief{6, wire.RoleAssistant, "turn 2"})
	if _, ok := <-live; ok {
		t.Fatal("live turns after a restart")
	}

	// The seq goes on in a log that opens again.
	l, err := openTurns(again.turnsPath(ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	l.add(wire.SessionTurn{Role: wire.RoleSystem, Text: "x"})
	l.close()
	all, _ := readTurns(again.turnsPath(ref.ID), 6)
	equal(t, all, brief{7, wire.RoleSystem, "x"})
}

func TestChatDeltasThenFullBlock(t *testing.T) {
	f := setup(t, tools, Options{})
	ref := f.launch(t, "APP-9", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	f.rt.mu.Lock()
	s := f.rt.sessions[ref.ID]
	f.rt.mu.Unlock()
	for _, ev := range []agentwire.Event{
		{Type: agentwire.EventAssistant, Text: "Hel", Delta: true},
		{Type: agentwire.EventAssistant, Text: "lo.", Delta: true},
		{Type: agentwire.EventAssistant, Text: "Hello."},
		{Type: agentwire.EventAssistant, Text: "Only", Delta: true},
		{Type: agentwire.EventResult},
	} {
		s.handle(ev)
	}
	src, err := f.rt.Chat(context.Background(), wire.DaemonSession{ID: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	history, _, err := src.Turns(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, history, brief{4, wire.RoleAssistant, "Hello."}, brief{5, wire.RoleAssistant, "Only"})
}

func TestChatClipsTools(t *testing.T) {
	l, err := openTurns(t.TempDir() + "/s-1.turns.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	l.tool(&agentwire.ToolEvent{ID: "t", Name: "Bash", Input: strings.Repeat("a", 300), Status: "started"}, time.Now())
	l.tool(&agentwire.ToolEvent{ID: "t", Name: "Bash", Output: strings.Repeat("b", 3000), Status: "failed"}, time.Now())
	l.tool(&agentwire.ToolEvent{Name: "Read", Status: "completed"}, time.Now())
	ts, _ := readTurns(l.file.Name(), 0)
	if len(ts) != 2 || ts[0].Seq != 1 || ts[1].Seq != 2 {
		t.Fatalf("%+v", ts)
	}
	tool := ts[0].Tool
	if tool.Status != wire.ToolFailed || len([]rune(tool.Input)) != maxToolInput || len([]rune(tool.Summary)) != maxToolSummary {
		t.Fatalf("%+v", tool)
	}
	if ts[1].Tool.Name != "Read" || ts[1].Tool.Status != wire.ToolDone {
		t.Fatalf("%+v", ts[1].Tool)
	}
}

func TestChatFromTranscript(t *testing.T) {
	f := setup(t, withID, Options{})
	root := projects(t)
	ctx := context.Background()
	ref := f.launch(t, "APP-7", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if err := f.rt.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// An old session has no turn log.
	if err := os.Remove(f.rt.turnsPath(ref.ID)); err != nil {
		t.Fatal(err)
	}
	again := New(Options{DataDir: f.data, Fake: true}, agentwire.New(agentwire.Options{}), proc.Local{}, nil)
	again.digests = readers(root)
	src, _ := again.Chat(ctx, wire.DaemonSession{ID: ref.ID})
	history, _, err := src.Turns(ctx, 0)
	if err != nil || len(history) < 2 || history[0].Seq != 1 || history[0].Role != wire.RoleUser {
		t.Fatalf("%+v %v", history, err)
	}
	newer, _, _ := src.Turns(ctx, 1)
	if len(newer) != len(history)-1 || newer[0].Seq != 2 {
		t.Fatalf("%+v", newer)
	}
	if _, err := again.Chat(ctx, wire.DaemonSession{ID: "../x"}); err == nil {
		t.Fatal("a bad session id passed")
	}
	if _, err := again.Terminal(ctx, wire.DaemonSession{ID: ref.ID}); err != term.ErrNoView {
		t.Fatal(err)
	}
}

// The daemon opens the chat view through the runtime registry.
var (
	_ runtime.Streamer     = (*Runtime)(nil)
	_ runtime.StreamStater = (*Runtime)(nil)
)
