package oversire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/runner/digest"
	"github.com/oversire/runner/proc"
	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/wire"
)

// withID tells its harness session id, then answers each prompt with one turn.
const withID = `while read -r line; do
  case "$line" in
    *'"type":"user"'*) echo '{"type":"init","session_id":"sess-1"}'; echo '{"type":"assistant","text":"hi"}'; echo '{"type":"result","subtype":"success"}';;
  esac
done`

// projects makes a Claude projects folder with the basic fixture as the transcript of sess-1.
func projects(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "contract", "fixtures", "session-digest", "claude", "basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "-wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "-wt", "sess-1.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func readers(root string) func(string) (digest.Reader, error) {
	return func(h string) (digest.Reader, error) {
		if h != "fake" {
			return nil, errors.New("unknown harness " + h)
		}
		return digest.NewClaudeReader(root), nil
	}
}

func TestHistory(t *testing.T) {
	f := setup(t, withID, Options{})
	root := projects(t)
	f.rt.digests = readers(root)
	ctx := context.Background()
	ref := f.launch(t, "APP-7", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)

	reg := runtime.NewRegistry(f.rt)
	res, err := reg.Do(ctx, wire.WorkOrder{ID: "o2", Runtime: ID, Payload: wire.HistoryPayload{Session: ref}})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Digest
	if d == nil || d.Source != wire.DigestFromRunner || d.Harness != digest.Claude || d.Turns == 0 {
		t.Fatalf("%+v", d)
	}

	// A new runtime finds the harness session in the session file.
	again := New(Options{DataDir: f.data, Fake: true}, agentwire.New(agentwire.Options{}), proc.Local{}, nil)
	again.digests = readers(root)
	if d2, err := again.History(ctx, ref, wire.HistoryBudget{}); err != nil || d2.SessionID != d.SessionID {
		t.Fatal(d2, err)
	}
	if _, err := again.History(ctx, wire.SessionRef{ID: "s-00"}, wire.HistoryBudget{}); err == nil {
		t.Fatal("an unknown session passed")
	}
	if err := os.Remove(filepath.Join(root, "-wt", "sess-1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := again.History(ctx, ref, wire.HistoryBudget{}); err == nil || !strings.Contains(err.Error(), "no fake transcript") {
		t.Fatal(err)
	}
	again.digests = digest.ForUser
	if _, err := again.History(ctx, ref, wire.HistoryBudget{}); err == nil || !strings.Contains(err.Error(), `"fake"`) {
		t.Fatal(err)
	}
}

func TestHistoryWithoutHarnessID(t *testing.T) {
	f := setup(t, chat, Options{})
	f.rt.digests = readers(t.TempDir())
	ref := f.launch(t, "APP-8", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if _, err := f.rt.History(context.Background(), ref, wire.HistoryBudget{}); err == nil || !strings.Contains(err.Error(), "no fake session id") {
		t.Fatal(err)
	}
}
