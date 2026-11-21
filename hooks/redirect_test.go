package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/ciredirect"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

func bashPayload(event, command string) []byte {
	in, _ := json.Marshal(map[string]string{"command": command})
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": event, "tool_name": "Bash", "tool_input": json.RawMessage(in)})
	return b
}

// postSpy runs a hook against a fake daemon and gives the posted call.
func postSpy(t *testing.T) (*Hook, *server.HookCall) {
	t.Helper()
	got := &server.HookCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewDecoder(r.Body).Decode(got) }))
	t.Cleanup(srv.Close)
	h := NewHook(srv.URL, "")
	h.Getenv = func(string) string { return "" }
	return h, got
}

func denied(t *testing.T, out []byte) string {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("stdout %q: %v", out, err)
	}
	o := raw["hookSpecificOutput"]
	if o["hookEventName"] != "PreToolUse" || o["permissionDecision"] != "deny" {
		t.Fatalf("stdout %q", out)
	}
	return o["permissionDecisionReason"]
}

func TestHookDeniesCIPoll(t *testing.T) {
	h, got := postSpy(t)
	h.Gate = ciredirect.NewGate()
	var out bytes.Buffer
	h.Run(context.Background(), "claude", bytes.NewReader(bashPayload("PreToolUse", "gh run watch 9")), &out)
	if r := denied(t, out.Bytes()); !strings.Contains(r, "oversire ci wait 9") {
		t.Fatalf("reason %q", r)
	}
	if !got.Redirected || readPayload(got.Payload).ToolName != "Bash" {
		t.Fatalf("post %+v", got)
	}
	// Other commands, and other events, keep the normal reply.
	for _, p := range [][]byte{bashPayload("PreToolUse", "go test ./..."), bashPayload("PostToolUse", "gh run watch 9")} {
		out.Reset()
		*got = server.HookCall{}
		h.Run(context.Background(), "claude", bytes.NewReader(p), &out)
		if out.String() != "{}\n" || got.Redirected || len(got.Payload) == 0 {
			t.Fatalf("stdout %q post %+v", out.String(), got)
		}
	}
}

func TestHookNoGate(t *testing.T) {
	h, got := postSpy(t)
	var out bytes.Buffer
	h.Run(context.Background(), "claude", bytes.NewReader(bashPayload("PreToolUse", "gh run watch 9")), &out)
	if out.String() != "{}\n" || got.Redirected {
		t.Fatalf("stdout %q post %+v", out.String(), got)
	}
}

func TestHookCodexDeny(t *testing.T) {
	h, got := postSpy(t)
	h.Gate = ciredirect.NewGate()
	var p map[string]any
	if err := json.Unmarshal(read(t, "payloads/codex/PreToolUse-shell.json"), &p); err != nil {
		t.Fatal(err)
	}
	p["tool_input"] = map[string]string{"command": "gh pr checks 4 --watch"}
	raw, _ := json.Marshal(p)
	var out bytes.Buffer
	h.Run(context.Background(), "codex", bytes.NewReader(raw), &out)
	if r := denied(t, out.Bytes()); !strings.Contains(r, "oversire ci wait '#4'") || !got.Redirected {
		t.Fatalf("reason %q post %+v", r, got)
	}
	// A call that passes gets no reply, as before.
	out.Reset()
	h.Run(context.Background(), "codex", bytes.NewReader(read(t, "payloads/codex/PreToolUse-shell.json")), &out)
	if out.Len() != 0 {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestHookBadPayloadKeepsReply(t *testing.T) {
	h, _ := postSpy(t)
	h.Gate = ciredirect.NewGate()
	var out bytes.Buffer
	big := append(bashPayload("PreToolUse", "gh run watch 9"), bytes.Repeat([]byte(" "), MaxPayload)...)
	h.Run(context.Background(), "claude", bytes.NewReader(big), &out)
	if out.String() != "{}\n" {
		t.Fatalf("stdout %q", out.String())
	}
	out.Reset()
	h.Run(context.Background(), "claude", strings.NewReader("not json"), &out)
	if out.String() != "{}\n" {
		t.Fatalf("stdout %q", out.String())
	}
}

func TestHookSpoolKeepsRedirected(t *testing.T) {
	spool := t.TempDir() + "/hooks.jsonl"
	h := NewHook("http://127.0.0.1:1", spool)
	h.Getenv = func(string) string { return "" }
	h.Gate = ciredirect.NewGate()
	h.Run(context.Background(), "claude", bytes.NewReader(bashPayload("PreToolUse", "gh run list")), &bytes.Buffer{})
	b, err := os.ReadFile(spool)
	var line spoolLine
	if err != nil || json.Unmarshal(b, &line) != nil || !line.Redirected {
		t.Fatalf("spool %v %s", err, b)
	}
}

func TestTurnEndedCountsRedirects(t *testing.T) {
	now := time.Unix(1000, 0)
	book := sessions.NewBook("mbp", "", nil, func() time.Time { return now })
	r := &rows{}
	g := NewIngest(book, nil).WithEvents(r).WithClock(func() time.Time { now = now.Add(time.Second); return now })
	ingest := func(payload string, redirected bool) {
		t.Helper()
		if err := g.Ingest(context.Background(), "claude", server.HookCall{Payload: []byte(payload), Redirected: redirected}); err != nil {
			t.Fatal(err)
		}
	}
	ingest(`{"session_id":"s1","cwd":"/tmp","hook_event_name":"UserPromptSubmit","prompt":"go"}`, false)
	ingest(string(bashPayload("PreToolUse", "gh run watch 9")), true)
	ingest(string(bashPayload("PreToolUse", "git status")), false)
	ingest(`{"session_id":"s1","cwd":"/tmp","hook_event_name":"Stop"}`, false)
	cat, err := wire.LoadEventCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	for _, d := range r.got {
		if p := cat.Problems(d); len(p) > 0 {
			t.Errorf("%s: %v", d.ID, p)
		}
		if d.Kind == wire.KindTurnEnded {
			_ = json.Unmarshal(d.Payload, &pl)
		}
	}
	if pl["ciRedirects"] != float64(1) || pl["tools"] != float64(1) {
		t.Fatalf("payload %v", pl)
	}
}
