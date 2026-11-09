package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

var t0 = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

// book is a fake session book. Live sessions have a key.
type book struct {
	byID  map[string]wire.DaemonSession
	tools map[string][2]string
}

func newBook(ss ...wire.DaemonSession) *book {
	b := &book{byID: map[string]wire.DaemonSession{}, tools: map[string][2]string{}}
	for _, s := range ss {
		b.byID[s.ID] = s
	}
	return b
}

func (b *book) Get(id string) (wire.DaemonSession, bool) {
	s, ok := b.byID[id]
	return s, ok
}

func (b *book) ByKey(rt, key string) (wire.DaemonSession, bool) {
	for _, s := range b.byID {
		if s.Key == key && s.Runtime == rt {
			return s, true
		}
	}
	return wire.DaemonSession{}, false
}

func (b *book) Tool(id string) (string, string) { return b.tools[id][0], b.tools[id][1] }

// unkey ends the live key of a session, as a stop does.
func (b *book) unkey(id string) {
	s := b.byID[id]
	s.Key = ""
	b.byID[id] = s
}

// fakeTurns is a term.TurnSource with a fixed history.
type fakeTurns struct{ history []wire.SessionTurn }

func (f fakeTurns) Turns(_ context.Context, after int) ([]wire.SessionTurn, <-chan wire.SessionTurn, error) {
	var out []wire.SessionTurn
	for _, t := range f.history {
		if t.Seq > after {
			out = append(out, t)
		}
	}
	live := make(chan wire.SessionTurn)
	close(live)
	return out, live, nil
}

func (fakeTurns) Interrupt(context.Context) error    { return nil }
func (fakeTurns) Send(context.Context, string) error { return nil }

type chats struct {
	turns fakeTurns
	err   error
}

func (c chats) Chat(context.Context, wire.DaemonSession) (term.TurnSource, error) {
	return c.turns, c.err
}

type differ struct {
	mu    sync.Mutex
	calls [][2]string
	repos []wire.ReviewRepo
}

func (d *differ) GroupRepos(_ context.Context, root, key string) ([]wire.ReviewRepo, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, [2]string{root, key})
	return d.repos, true, nil
}

func assistant(seq int) wire.SessionTurn {
	return wire.SessionTurn{Seq: seq, Role: wire.RoleAssistant, At: t0, Text: "text"}
}

func toolTurn(seq int, status wire.ToolStatus) wire.SessionTurn {
	return wire.SessionTurn{Seq: seq, Role: wire.RoleTool, At: t0, Tool: &wire.TurnTool{Name: "Bash", Status: status, Input: "go test"}}
}

func turnsTo(n int) []wire.SessionTurn {
	var out []wire.SessionTurn
	for i := 1; i <= n; i++ {
		out = append(out, assistant(i))
	}
	return out
}

func seqs(ts []wire.SessionTurn) []int {
	out := make([]int, len(ts))
	for i, t := range ts {
		out[i] = t.Seq
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type rig struct {
	e     *Engine
	store *Store
	dir   string
	book  *book
	diff  *differ
}

func newRig(t *testing.T, history []wire.SessionTurn, ss ...wire.DaemonSession) rig {
	t.Helper()
	dir := t.TempDir()
	b, d := newBook(ss...), &differ{}
	store := NewStore(dir)
	e := NewEngine(store, b, chats{turns: fakeTurns{history: history}}, d, func() time.Time { return t0 })
	return rig{e: e, store: store, dir: dir, book: b, diff: d}
}

func live(id, key string) wire.DaemonSession {
	return wire.DaemonSession{ID: id, Runtime: wire.RuntimeOversire, Harness: "codex", Key: key, State: wire.PhaseWorking, StateSince: t0}
}

func TestReadGivesTheTurnsAfterTheCursorOfTheReader(t *testing.T) {
	r := newRig(t, turnsTo(6), live("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	first, err := r.e.Read(ctx, "s1", "overseer", 0)
	if err != nil || !equalInts(seqs(first.Turns), []int{1, 2, 3, 4, 5, 6}) || first.Next != 6 {
		t.Fatalf("%+v %v", first, err)
	}
	if err := r.e.Commit("s1", "overseer", 3); err != nil {
		t.Fatal(err)
	}
	got, err := r.e.Read(ctx, "s1", "overseer", 0)
	if err != nil || !equalInts(seqs(got.Turns), []int{4, 5, 6}) || got.Header.First != 4 || got.Header.Last != 6 {
		t.Fatalf("%+v %v", got, err)
	}
	if clipped, _ := r.e.Read(ctx, "s1", "overseer", 2); !equalInts(seqs(clipped.Turns), []int{4, 5}) || clipped.Next != 5 {
		t.Fatalf("limit %+v", clipped)
	}
	if empty, _ := r.e.Read(ctx, "s1", "fresh", 0); len(empty.Turns) != 6 {
		t.Fatalf("a new reader starts at 0: %+v", empty)
	}
	r.e.Commit("s1", "overseer", 6)
	if none, err := r.e.Read(ctx, "s1", "overseer", 0); err != nil || none.Turns == nil || len(none.Turns) != 0 || none.Next != 6 {
		t.Fatalf("no new turns %+v %v", none, err)
	}
}

func TestReadKeepsOneCursorForEachReader(t *testing.T) {
	r := newRig(t, turnsTo(8), live("s1", "work:OVERSIRE-12"))
	ctx := context.Background()
	if err := r.e.Commit("s1", "A", 5); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Commit("s1", "B", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := r.e.Read(ctx, "s1", "A", 0)
	b, _ := r.e.Read(ctx, "s1", "B", 0)
	if !equalInts(seqs(a.Turns), []int{6, 7, 8}) || len(b.Turns) != 8 {
		t.Fatalf("A %v, B %v", seqs(a.Turns), seqs(b.Turns))
	}
}

func TestReadStopsTheCursorBeforeARunningTool(t *testing.T) {
	h := turnsTo(6)
	h[3] = toolTurn(4, wire.ToolRunning)
	r := newRig(t, h, live("s1", "work:OVERSIRE-12"))
	got, err := r.e.Read(context.Background(), "s1", "overseer", 0)
	if err != nil || got.Next != 3 || len(got.Turns) != 6 {
		t.Fatalf("%+v %v", got, err)
	}
	h[3] = toolTurn(4, wire.ToolDone)
	if got, _ = r.e.Read(context.Background(), "s1", "overseer", 0); got.Next != 6 {
		t.Fatalf("after the tool ends: %d", got.Next)
	}
}

func TestCursorSurvivesANewStore(t *testing.T) {
	r := newRig(t, turnsTo(6), live("s1", "work:OVERSIRE-12"))
	if err := r.e.Commit("s1", "overseer", 4); err != nil {
		t.Fatal(err)
	}
	again := NewEngine(NewStore(r.dir), r.book, chats{turns: fakeTurns{history: turnsTo(6)}}, r.diff, nil)
	got, err := again.Read(context.Background(), "s1", "overseer", 0)
	if err != nil || !equalInts(seqs(got.Turns), []int{5, 6}) {
		t.Fatalf("%+v %v", got, err)
	}
	rec, err := NewStore(r.dir).Load("s1")
	if err != nil || rec.Cursors["overseer"] != 4 || rec.Key != "work:OVERSIRE-12" || rec.Session != "s1" {
		t.Fatalf("%+v %v", rec, err)
	}
	if empty, err := NewStore(r.dir).Load("nope"); err != nil || empty.Session != "" || empty.Cursors != nil {
		t.Fatalf("missing file: %+v %v", empty, err)
	}
}

func TestResolveFindsTheSessionByKey(t *testing.T) {
	r := newRig(t, nil, live("s1", "work:OVERSIRE-12"), live("s2", "plan:OVERSIRE-12"))
	for _, ref := range []string{"s1", "OVERSIRE-12", "oversire-12"} {
		if s, err := r.e.Resolve(ref); err != nil || s.ID != "s1" {
			t.Fatalf("%s: %+v %v", ref, s, err)
		}
	}
	var e *Error
	if _, err := r.e.Resolve("OVERSIRE-99"); !errors.As(err, &e) || e.Kind != NotFound {
		t.Fatalf("unknown: %v", err)
	}
}

func TestResolveFindsAStoppedSessionInTheStore(t *testing.T) {
	r := newRig(t, turnsTo(3), live("s1", "work:OVERSIRE-12"))
	if _, err := r.e.Read(context.Background(), "s1", "overseer", 0); err != nil {
		t.Fatal(err)
	}
	r.book.unkey("s1")
	if s, err := r.e.Resolve("OVERSIRE-12"); err != nil || s.ID != "s1" {
		t.Fatalf("%+v %v", s, err)
	}
	// A newer worker of the same item wins.
	r.book.byID["s3"] = live("s3", "")
	if err := r.store.Save(Record{Session: "s3", Key: "work:OVERSIRE-12", UpdatedAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if s, err := r.e.Resolve("OVERSIRE-12"); err != nil || s.ID != "s3" {
		t.Fatalf("newest: %+v %v", s, err)
	}
}

func TestReadRefusesAnotherRuntime(t *testing.T) {
	orca := wire.DaemonSession{ID: "o1", Runtime: "orca", Key: "work:OVERSIRE-12"}
	r := newRig(t, turnsTo(2), orca)
	var e *Error
	if _, err := r.e.Read(context.Background(), "o1", "overseer", 0); !errors.As(err, &e) || e.Kind != Conflict {
		t.Fatalf("read: %v", err)
	}
	if err := r.e.Commit("o1", "overseer", 1); !errors.As(err, &e) || e.Kind != Conflict {
		t.Fatalf("commit: %v", err)
	}
}

func TestReadRefusesBadInput(t *testing.T) {
	r := newRig(t, turnsTo(2), live("s1", ""))
	var e *Error
	if _, err := r.e.Read(context.Background(), "s1", "", 0); !errors.As(err, &e) || e.Kind != Invalid {
		t.Fatalf("no reader: %v", err)
	}
	if err := r.e.Commit("s1", "x", -1); !errors.As(err, &e) || e.Kind != Invalid {
		t.Fatalf("negative seq: %v", err)
	}
	if _, err := r.e.Read(context.Background(), "nope", "x", 0); !errors.As(err, &e) || e.Kind != NotFound {
		t.Fatalf("unknown: %v", err)
	}
	r.e.chats = chats{err: term.ErrNoView}
	if _, err := r.e.Read(context.Background(), "s1", "x", 0); !errors.As(err, &e) || e.Kind != Conflict {
		t.Fatalf("no view: %v", err)
	}
}

func TestHeaderTakesTheGroupFromTheRepo(t *testing.T) {
	s := live("s1", "work:OVERSIRE-12")
	s.Repo = "/work/ws/.wt/oversire-12"
	s.Model = "gpt-5"
	s.Reason = wire.ReasonPermission
	r := newRig(t, turnsTo(2), s)
	r.book.tools["s1"] = [2]string{"Bash", "npm test"}
	r.diff.repos = []wire.ReviewRepo{{Name: "cli", Added: 10, Removed: 2, Files: 3}}
	got, err := r.e.Read(context.Background(), "s1", "overseer", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.diff.calls) != 1 || r.diff.calls[0] != [2]string{"/work/ws", "oversire-12"} {
		t.Fatalf("calls %v", r.diff.calls)
	}
	h := got.Header
	if len(h.Repos) != 1 || h.Key != "work:OVERSIRE-12" || h.Harness != "codex" || h.Model != "gpt-5" || h.Request != "permission Bash npm test" {
		t.Fatalf("header %+v", h)
	}
	// A session outside a group asks for no diff.
	r2 := newRig(t, nil, live("s2", "work:X-1"))
	r2.e.Read(context.Background(), "s2", "overseer", 0)
	if len(r2.diff.calls) != 0 {
		t.Fatalf("calls %v", r2.diff.calls)
	}
}

func TestHeaderGivesTheQuestionId(t *testing.T) {
	s := live("s1", "")
	s.Reason, s.Question = wire.ReasonQuestion, "q-7"
	r := newRig(t, nil, s)
	got, err := r.e.Read(context.Background(), "s1", "overseer", 0)
	if err != nil || got.Header.Request != "question q-7" {
		t.Fatalf("%+v %v", got.Header, err)
	}
}
