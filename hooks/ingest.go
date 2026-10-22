package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// Hinter gets a poke after each patch of an Orca pane (orcawatch.Watcher).
type Hinter interface{ Hint() }

// Ingest turns hook calls into patches of the session book. It implements server.HookSink.
type Ingest struct {
	book      *sessions.Book
	questions QuestionSink
	byName    map[string]Harness
	clock     func() time.Time
	hinter    Hinter
	// lastMessage reads the last reply from a transcript when the Stop payload has none.
	lastMessage func(harness, transcript string) (string, bool)

	mu          sync.Mutex
	transcripts map[string]string // session id to transcript_path
	asked       map[string]bool   // a PermissionRequest came in the current turn
}

// NewIngest takes all harnesses when hs is empty. questions can be nil.
func NewIngest(book *sessions.Book, questions QuestionSink, hs ...Harness) *Ingest {
	if len(hs) == 0 {
		hs = Harnesses()
	}
	g := &Ingest{book: book, questions: questions, byName: map[string]Harness{}, clock: time.Now,
		lastMessage: transcriptLastMessage, transcripts: map[string]string{}, asked: map[string]bool{}}
	for _, h := range hs {
		g.byName[h.Name()] = h
	}
	return g
}

// WithHinter sets the hinter. nil turns it off.
func (g *Ingest) WithHinter(h Hinter) *Ingest {
	g.hinter = h
	return g
}

// WithClock sets the clock, for tests.
func (g *Ingest) WithClock(c func() time.Time) *Ingest {
	g.clock = c
	return g
}

func (g *Ingest) Ingest(_ context.Context, harness string, env wire.HookEnv, payload []byte) error {
	return g.apply(harness, env, payload, g.clock())
}

// Transcript gives the last transcript_path of a session, or "".
func (g *Ingest) Transcript(session string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.transcripts[session]
}

func (g *Ingest) apply(harness string, env wire.HookEnv, payload []byte, at time.Time) error {
	// Sessions of the oversire runtime report through agent-wire.
	if env.OversireSession != "" {
		return nil
	}
	h, ok := g.byName[harness]
	if !ok {
		return fmt.Errorf("no hooks for the harness %q", harness)
	}
	if !json.Valid(payload) {
		return errors.New("the hook payload is not JSON")
	}
	p := readPayload(payload)
	ev := Event{Harness: harness, Name: p.HookEventName, Payload: json.RawMessage(payload), Env: env, At: at}
	patch, ok := h.Map(ev)
	if !ok {
		return nil
	}
	g.track(&patch, ev.Name, p)
	if ev.Name == "Stop" {
		if patch.LastMessage == nil && p.TranscriptPath != "" && g.lastMessage != nil {
			if msg, ok := g.lastMessage(harness, p.TranscriptPath); ok {
				patch.LastMessage = &msg
			}
		}
		// Needs input: a turn that ends with a question.
		if patch.LastMessage != nil && patch.Reason != nil && *patch.Reason == wire.ReasonTurnEnd && sessions.TurnQuestion(*patch.LastMessage) != "" {
			patch.Reason = ptr(wire.ReasonQuestion)
		}
	}
	g.book.Apply(patch)
	ev.Session = patch.ID
	if g.questions != nil {
		g.questions.OnHook(ev)
	}
	if env.PaneKey != "" && g.hinter != nil {
		g.hinter.Hint()
	}
	return nil
}

// track keeps the transcript of each session, and drops a permission Notification that follows a PermissionRequest.
func (g *Ingest) track(patch *sessions.Patch, name string, p payload) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := patch.ID
	if patch.End {
		delete(g.transcripts, id)
		delete(g.asked, id)
		return
	}
	if p.TranscriptPath != "" {
		g.transcripts[id] = p.TranscriptPath
	}
	switch name {
	case "PermissionRequest":
		g.asked[id] = true
	case "Notification":
		if g.asked[id] {
			patch.State, patch.Reason, patch.Tool, patch.ToolInput = nil, nil, nil, nil
		}
	default:
		delete(g.asked, id)
	}
}

// DrainSpool applies the calls that the hook command spooled, then removes the file. A bad line is skipped.
// The file moves aside first, so that a hook that runs now writes a new file.
func (g *Ingest) DrainSpool(path string) error {
	work := path + ".draining"
	if err := os.Rename(path, work); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.Open(work)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), MaxSpool)
	for sc.Scan() {
		var l spoolLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Harness == "" {
			continue
		}
		if l.At.IsZero() {
			l.At = g.clock()
		}
		_ = g.apply(l.Harness, l.Env, l.Payload, l.At)
	}
	err = sc.Err()
	f.Close()
	if rmErr := os.Remove(work); err == nil {
		err = rmErr
	}
	return err
}

// transcriptLastMessage reads the last reply from a Claude transcript.
func transcriptLastMessage(harness, path string) (string, bool) {
	if harness != digest.Claude {
		return "", false
	}
	b := wire.NewHistoryBudget(0, 1, wire.MaxSessionLastMessage)
	d, err := digest.ReadClaudeFile(path, &b, wire.DigestFromFile)
	if err != nil || len(d.Last) == 0 || d.Last[len(d.Last)-1].Text == "" {
		return "", false
	}
	return d.Last[len(d.Last)-1].Text, true
}
