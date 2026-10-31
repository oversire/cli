package oversire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

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

	// A new runtime builds the turns from the events of the visor: the same seqs, and no live turns.
	again := f.again(t, nil)
	src, _ = again.Chat(ctx, wire.DaemonSession{ID: ref.ID})
	history, live, err = src.Turns(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, history, brief{4, wire.RoleUser, "more"}, brief{5, wire.RoleTool, "Bash done npm test 2 ok"}, brief{6, wire.RoleAssistant, "turn 2"})
	if _, ok := <-live; ok {
		t.Fatal("live turns after a restart")
	}

}

func TestChatOfOldSession(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "sessions"), 0o700)
	lines := `{"seq":1,"role":"user","at":"2026-01-01T00:00:00Z","text":"go"}
{"seq":2,"role":"tool","at":"2026-01-01T00:00:00Z","tool":{"name":"Bash","status":"running"}}
{"seq":2,"role":"tool","at":"2026-01-01T00:00:00Z","tool":{"name":"Bash","status":"done"}}
`
	os.WriteFile(filepath.Join(data, "sessions", "s-0c.turns.jsonl"), []byte(lines), 0o600)
	rt := New(Options{DataDir: data}, nil, nil, nil)
	src, _ := rt.Chat(context.Background(), wire.DaemonSession{ID: "s-0c"})
	history, live, err := src.Turns(context.Background(), 0)
	if err != nil || len(history) != 2 || history[1].Tool.Status != wire.ToolDone {
		t.Fatalf("%+v %v", history, err)
	}
	if _, ok := <-live; ok {
		t.Fatal("live turns of an old session")
	}
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
		s.mu.Lock()
		s.handle(ev, time.Now())
		s.mu.Unlock()
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
	l := newTurnLog()
	l.tool(&agentwire.ToolEvent{ID: "t", Name: "Bash", Input: strings.Repeat("a", 300), Status: "started"}, time.Now())
	l.tool(&agentwire.ToolEvent{ID: "t", Name: "Bash", Output: strings.Repeat("b", 3000), Status: "failed"}, time.Now())
	l.tool(&agentwire.ToolEvent{Name: "Read", Status: "completed"}, time.Now())
	ts := l.turns
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
	// A session from before agent-visor has no turn log: the transcript of the harness gives the turns.
	os.MkdirAll(filepath.Join(f.data, "sessions"), 0o700)
	os.WriteFile(filepath.Join(f.data, "sessions", "s-0d.json"), []byte(`{"agent":"fake","harnessSession":"sess-1"}`), 0o600)
	again := New(Options{DataDir: f.data, Fake: true}, nil, nil, nil)
	again.digests = readers(root)
	src, _ := again.Chat(ctx, wire.DaemonSession{ID: "s-0d"})
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

func TestChatInterrupt(t *testing.T) {
	f := setup(t, tools, Options{})
	ref := f.launch(t, "APP-10", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	src, err := f.rt.Chat(context.Background(), wire.DaemonSession{ID: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Stop(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := src.Interrupt(context.Background()); err == nil {
		t.Fatal("interrupt of an ended session")
	}
}
