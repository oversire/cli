package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

var _ server.HookSink = (*Ingest)(nil)

type sink struct {
	mu  sync.Mutex
	evs []Event
}

func (s *sink) OnHook(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

type hints struct{ n atomic.Int32 }

func (h *hints) Hint() { h.n.Add(1) }

func newIngest(t *testing.T) (*Ingest, *sessions.Book, *sink, *hints) {
	t.Helper()
	now := time.Unix(1000, 0)
	book := sessions.NewBook("mbp", "", nil, func() time.Time { return now })
	s, h := &sink{}, &hints{}
	g := NewIngest(book, s).WithHinter(h).WithClock(func() time.Time { now = now.Add(time.Second); return now })
	return g, book, s, h
}

func ingest(t *testing.T, g *Ingest, harness, file string, env wire.HookEnv) {
	t.Helper()
	if err := g.Ingest(context.Background(), harness, env, read(t, filepath.Join("payloads", harness, file+".json"))); err != nil {
		t.Fatal(err)
	}
}

func TestIngestTurn(t *testing.T) {
	g, book, s, h := newIngest(t)
	ingest(t, g, "claude", "SessionStart", wire.HookEnv{})
	ingest(t, g, "claude", "UserPromptSubmit", wire.HookEnv{})
	got, _ := book.Get(cliClaude)
	if got.State != wire.PhaseWorking || got.Prompt != "Add a health route." || got.Runtime != wire.RuntimeCLI || got.Harness != "claude" {
		t.Fatalf("%+v", got)
	}
	ingest(t, g, "claude", "PreToolUse-AskUserQuestion", wire.HookEnv{})
	if got, _ = book.Get(cliClaude); got.State != wire.PhaseWaiting || got.Reason != wire.ReasonQuestion {
		t.Fatalf("%+v", got)
	}
	if tool, _ := book.Tool(cliClaude); tool != "AskUserQuestion" {
		t.Fatalf("tool %q", tool)
	}
	ingest(t, g, "claude", "PostToolUse-AskUserQuestion", wire.HookEnv{})
	ingest(t, g, "claude", "Stop-question", wire.HookEnv{})
	if got, _ = book.Get(cliClaude); got.State != wire.PhaseDone || got.Reason != wire.ReasonQuestion {
		t.Fatalf("%+v", got)
	}
	if len(s.evs) != 5 || s.evs[4].Session != cliClaude || s.evs[4].Name != "Stop" {
		t.Fatalf("question sink: %+v", s.evs)
	}
	if g.Transcript(cliClaude) == "" {
		t.Fatal("no transcript path")
	}
	if h.n.Load() != 0 {
		t.Fatal("a cli session must not hint the Orca watcher")
	}
	ingest(t, g, "claude", "SessionEnd", wire.HookEnv{})
	if g.Transcript(cliClaude) != "" {
		t.Fatal("the transcript path stays after the end")
	}
}

func TestIngestOrcaHint(t *testing.T) {
	g, book, _, h := newIngest(t)
	ingest(t, g, "claude", "PermissionRequest", orcaEnv)
	got, ok := book.Get("tab1:leaf1")
	if !ok || got.State != wire.PhaseWaiting || got.Reason != wire.ReasonPermission || got.Terminal != "term_1" || got.Runtime != wire.RuntimeOrca {
		t.Fatalf("%+v", got)
	}
	if h.n.Load() != 1 {
		t.Fatalf("hints: %d", h.n.Load())
	}
}

func TestIngestIgnores(t *testing.T) {
	g, book, s, _ := newIngest(t)
	ingest(t, g, "claude", "UserPromptSubmit", wire.HookEnv{OversireSession: "s1"})
	ingest(t, g, "claude", "PreToolUse-subagent", wire.HookEnv{})
	if len(book.All()) != 0 || len(s.evs) != 0 {
		t.Fatalf("%+v", book.All())
	}
	if err := g.Ingest(context.Background(), "cursor", wire.HookEnv{}, []byte(`{}`)); err == nil {
		t.Fatal("an unknown harness must fail")
	}
	if err := g.Ingest(context.Background(), "claude", wire.HookEnv{}, []byte(`{`)); err == nil {
		t.Fatal("bad JSON must fail")
	}
}

func TestIngestPermissionNotification(t *testing.T) {
	g, _, _, _ := newIngest(t)
	note := readPayload(read(t, "payloads/claude/Notification-permission.json"))
	patch := func() sessions.Patch {
		p, _ := Claude{}.Map(event(t, "claude", "Notification-permission", wire.HookEnv{}))
		g.track(&p, "Notification", note)
		return p
	}
	if p := patch(); p.State == nil {
		t.Fatal("a permission notification without a request must set waiting")
	}
	ingest(t, g, "claude", "PermissionRequest", wire.HookEnv{})
	if p := patch(); p.State != nil || p.Reason != nil {
		t.Fatal("a notification after a PermissionRequest must not change the state")
	}
	ingest(t, g, "claude", "UserPromptSubmit", wire.HookEnv{})
	if p := patch(); p.State == nil {
		t.Fatal("a new turn must clear the request")
	}
}

func TestIngestTranscriptLastMessage(t *testing.T) {
	g, book, _, _ := newIngest(t)
	var path string
	g.lastMessage = func(harness, p string) (string, bool) { path = p; return "Done.\n\nShould I deploy?", true }
	ingest(t, g, "claude", "Stop-transcript", wire.HookEnv{})
	got, _ := book.Get(cliClaude)
	if !strings.HasSuffix(path, ".jsonl") || got.LastMessage != "Done.\n\nShould I deploy?" || got.Reason != wire.ReasonQuestion {
		t.Fatalf("%q %+v", path, got)
	}
}

func TestTranscriptLastMessage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	lines := `{"type":"user","sessionId":"s","message":{"role":"user","content":"hi"}}
{"type":"assistant","sessionId":"s","message":{"role":"assistant","content":[{"type":"text","text":"Hello."}]}}
`
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, ok := transcriptLastMessage("claude", p); !ok || msg != "Hello." {
		t.Fatalf("%q %v", msg, ok)
	}
	if _, ok := transcriptLastMessage("codex", p); ok {
		t.Fatal("codex has no reader")
	}
}

func TestDrainSpool(t *testing.T) {
	g, book, _, _ := newIngest(t)
	path := filepath.Join(t.TempDir(), "spool", "hooks.jsonl")
	h := NewHook("http://127.0.0.1:1", path)
	h.Getenv = func(string) string { return "" }
	h.spool("claude", wire.HookEnv{}, read(t, "payloads/claude/UserPromptSubmit.json"))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	h.spool("codex", wire.HookEnv{}, read(t, "payloads/codex/UserPromptSubmit.json"))

	if err := g.DrainSpool(path); err != nil {
		t.Fatal(err)
	}
	if len(book.All()) != 2 {
		t.Fatalf("%+v", book.All())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the spool stays")
	}
	if err := g.DrainSpool(path); err != nil {
		t.Fatalf("a missing spool: %v", err)
	}
}

func TestHookNoDaemon(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + ln.Addr().String()
	ln.Close()
	spool := filepath.Join(t.TempDir(), "spool", "hooks.jsonl")
	h := NewHook(url, spool)
	h.Getenv = func(string) string { return "" }
	var out bytes.Buffer
	start := time.Now()
	h.Run(context.Background(), "claude", bytes.NewReader(read(t, "payloads/claude/Stop.json")), &out)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %s", d)
	}
	if out.String() != "{}\n" {
		t.Fatalf("stdout %q", out.String())
	}
	b, err := os.ReadFile(spool)
	if err != nil || bytes.Count(b, []byte("\n")) != 1 {
		t.Fatalf("spool: %v %s", err, b)
	}
	var line spoolLine
	if err := json.Unmarshal(b, &line); err != nil || line.Harness != "claude" || !json.Valid(line.Payload) || line.At.IsZero() {
		t.Fatalf("line: %v %+v", err, line)
	}
}

func TestHookSlowDaemon(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	spool := filepath.Join(t.TempDir(), "hooks.jsonl")
	h := NewHook(srv.URL, spool)
	h.Getenv = func(string) string { return "" }
	start := time.Now()
	h.Run(context.Background(), "codex", bytes.NewReader(read(t, "payloads/codex/Stop.json")), io.Discard)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %s", d)
	}
	if _, err := os.Stat(spool); err != nil {
		t.Fatal("no spool after a timeout")
	}
}

func TestHookPosts(t *testing.T) {
	var calls atomic.Int32
	var got server.HookCall
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	spool := filepath.Join(t.TempDir(), "hooks.jsonl")
	h := NewHook(srv.URL, spool)
	env := map[string]string{"ORCA_PANE_KEY": "tab1:leaf1", "ORCA_TERMINAL_HANDLE": "term_1", "PWD": "/tmp"}
	h.Getenv = func(k string) string { return env[k] }
	h.PPID = 42
	var out bytes.Buffer
	h.Run(context.Background(), "claude", bytes.NewReader(read(t, "payloads/claude/Stop.json")), &out)
	if calls.Load() != 1 || path != "/v1/hooks/claude" || got.Env.PaneKey != "tab1:leaf1" || got.Env.TerminalHandle != "term_1" || got.Env.PPID != 42 || readPayload(got.Payload).HookEventName != "Stop" {
		t.Fatalf("calls %d %s %+v", calls.Load(), path, got.Env)
	}
	if out.String() != "{}\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Fatal("a posted call was spooled")
	}

	// A session of the oversire runtime does not post. Codex gets no reply.
	env["OVERSIRE_SESSION"] = "s1"
	out.Reset()
	h.Run(context.Background(), "codex", bytes.NewReader(read(t, "payloads/codex/Stop.json")), &out)
	if calls.Load() != 1 || out.Len() != 0 {
		t.Fatalf("calls %d stdout %q", calls.Load(), out.String())
	}
}
