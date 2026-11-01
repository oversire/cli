//go:build !windows

package oversire

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// vendorChat is chat with a vendor session id, so the visor can resume it.
const vendorChat = `n=0
while read -r line; do
  case "$line" in
    *'"type":"user"'*) n=$((n+1)); echo '{"type":"init","session_id":"v1"}'; echo "{\"type\":\"assistant\",\"text\":\"turn $n\"}"; echo '{"type":"result","subtype":"success","session_id":"v1"}';;
  esac
done`

// tui is a fake agent CLI in a terminal. It ends on the line exit.
const tui = `stty -echo 2>/dev/null; echo "tui resume $0"; while IFS= read -r l; do case "$l" in exit) exit 0;; *) echo "got:$l";; esac; done`

func resumeTUI(l agentvisor.Launch) ([]string, error) {
	if !l.Resume {
		return nil, agentvisor.ErrUnsupported
	}
	return []string{"sh", "-c", tui, l.Session.VendorID}, nil
}

// waitView waits for a report of the session with the view.
func (r *recorder) waitView(t *testing.T, id string, v wire.SessionView) wire.RunnerSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, _ := r.latest(id)
		if s.View == v {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("view %q, want %q: %#v", s.View, v, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitOutput reads chunks until the terminal shows want.
func waitOutput(t *testing.T, snap term.Snapshot, ch <-chan term.Chunk, want string) {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(snap.Data)
	timeout := time.After(10 * time.Second)
	for !strings.Contains(buf.String(), want) {
		select {
		case c, ok := <-ch:
			if !ok {
				t.Fatalf("the terminal closed before %q: %q", want, buf.String())
			}
			buf.Write(c.Output)
			if c.Repaint != nil {
				buf.Write(c.Repaint.Data)
			}
		case <-timeout:
			t.Fatalf("no %q in %q", want, buf.String())
		}
	}
}

func TestHandoverToTerminalAndBack(t *testing.T) {
	f := setupDriver(t, agentwire.FakeDriver{Script: vendorChat}, Options{}, func(v *agentvisor.Visor) Host { return v }, resumeTUI)
	ctx := context.Background()
	ref := f.launch(t, "tui", runtime.Order{ID: "o1"})
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.View != wire.SessionViewChat {
		t.Fatalf("view %q", s.View)
	}
	d := wire.DaemonSession{ID: ref.ID}
	if _, err := f.rt.Terminal(ctx, d); !errors.Is(err, term.ErrNoView) {
		t.Fatalf("terminal of a chat session: %v", err)
	}

	if err := f.rt.Handover(ctx, ref.ID, wire.SessionViewTerminal); err != nil {
		t.Fatal(err)
	}
	f.rec.waitView(t, ref.ID, wire.SessionViewTerminal)
	src, err := f.rt.Terminal(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if c := src.Caps(); c.Source != wire.SourceStream || !c.Writable {
		t.Fatalf("caps %+v", c)
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	snap, ch, err := src.Attach(actx, term.Viewport{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	waitOutput(t, snap, ch, "tui resume v1")
	if err := src.Write(ctx, []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, term.Snapshot{}, ch, "got:hello")

	// The move to chat waits until the agent CLI exits.
	if err := f.rt.Handover(ctx, ref.ID, wire.SessionViewChat); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Handover(ctx, ref.ID, wire.SessionViewChat); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("a second move: %v", err)
	}
	if err := src.Write(ctx, []byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	f.rec.waitView(t, ref.ID, wire.SessionViewChat)
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Text: "again"}); err != nil {
		t.Fatal(err)
	}
	// The resumed CLI is a new process, so its count starts again.
	deadline := time.Now().Add(10 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		if lines, _ = f.rt.Read(ctx, ref, 200); slices.Equal(lines, []string{"> go", "turn 1", "> again", "turn 1"}) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s, _ := f.rec.latest(ref.ID); !slices.Equal(lines, []string{"> go", "turn 1", "> again", "turn 1"}) || s.View != wire.SessionViewChat {
		t.Fatalf("after the move back %q %#v %v", lines, s, f.rec.states(ref.ID))
	}
}

func TestHandoverRefusals(t *testing.T) {
	f := setupDriver(t, agentwire.FakeDriver{Script: vendorChat}, Options{}, func(v *agentvisor.Visor) Host { return v }, TerminalCommand)
	ctx := context.Background()
	if err := f.rt.Handover(ctx, "s-none", wire.SessionViewTerminal); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	ref := f.launch(t, "fake", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	// The fake harness has no resume command.
	if err := f.rt.Handover(ctx, ref.ID, wire.SessionViewTerminal); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("no resume command: %v", err)
	}
	if err := f.rt.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.Handover(ctx, ref.ID, wire.SessionViewTerminal); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("ended session: %v", err)
	}
}

func TestTerminalCommand(t *testing.T) {
	sess := agentvisor.Session{VendorID: "v1"}
	for _, c := range []struct {
		l    agentvisor.Launch
		want []string
	}{
		{agentvisor.Launch{Resume: true, Session: sess, Spec: agentvisor.StartSpec{Harness: "claude", ExtraArgs: []string{"--x"}}}, []string{"claude", "--resume", "v1", "--x"}},
		{agentvisor.Launch{Resume: true, Session: sess, Binary: "/bin/codex", Spec: agentvisor.StartSpec{Harness: "codex"}}, []string{"/bin/codex", "resume", "v1"}},
		{agentvisor.Launch{Spec: agentvisor.StartSpec{Harness: "claude"}}, []string{"claude"}},
		{agentvisor.Launch{Spec: agentvisor.StartSpec{Command: []string{"sh"}}}, []string{"sh"}},
	} {
		got, err := TerminalCommand(c.l)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%+v: %q %v, want %q", c.l, got, err, c.want)
		}
	}
	if _, err := TerminalCommand(agentvisor.Launch{Resume: true, Session: sess, Spec: agentvisor.StartSpec{Harness: "fake"}}); !errors.Is(err, agentvisor.ErrUnsupported) {
		t.Fatalf("fake: %v", err)
	}
}
