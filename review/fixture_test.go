package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// ws is a workspace root with primary checkouts and worktree groups in .wt, made with the real git.
type ws struct {
	t    *testing.T
	root string
}

func newWS(t *testing.T) *ws {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@t")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &ws{t: t, root: root}
}

func (w *ws) git(dir string, args ...string) string {
	w.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *ws) write(path, text string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// repo makes the primary checkout name on main with one commit.
func (w *ws) repo(name string) string {
	w.t.Helper()
	dir := filepath.Join(w.root, name)
	w.git(w.root, "init", "-q", "-b", "main", dir)
	w.commit(dir, "init", map[string]string{"README.md": "# " + name + "\n", "lib.go": "package lib\n\nfunc A() {}\n"})
	return dir
}

// group makes .wt/<name> with a worktree on the branch name for each repo.
func (w *ws) group(name string, repos ...string) string {
	w.t.Helper()
	dir := filepath.Join(w.root, ".wt", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	for _, r := range repos {
		w.git(filepath.Join(w.root, r), "worktree", "add", "-q", "-b", name, filepath.Join(dir, r), "main")
	}
	return dir
}

// commit writes the files in dir and commits them.
func (w *ws) commit(dir, msg string, files map[string]string) string {
	w.t.Helper()
	for p, text := range files {
		w.write(filepath.Join(dir, p), text)
	}
	w.git(dir, "add", "-A")
	w.git(dir, "commit", "-q", "-m", msg)
	return w.git(dir, "rev-parse", "HEAD")
}

// fakeRepos gives the root as the folder repo of a runtime.
type fakeRepos struct{ roots []string }

func (f fakeRepos) Repos(context.Context) wire.DaemonReposPage {
	var page wire.DaemonReposPage
	for _, r := range f.roots {
		page.Repos = append(page.Repos, wire.DaemonRepo{ID: r, Runtime: wire.RuntimeOrca, Name: filepath.Base(r), Path: r, Folder: true})
	}
	return page
}

type fakeBook struct {
	mu       sync.Mutex
	sessions []wire.DaemonSession
	ch       chan string
}

func newFakeBook() *fakeBook { return &fakeBook{ch: make(chan string, 16)} }

func (b *fakeBook) All() []wire.DaemonSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]wire.DaemonSession(nil), b.sessions...)
}

func (b *fakeBook) Get(id string) (wire.DaemonSession, bool) {
	for _, s := range b.All() {
		if s.ID == id {
			return s, true
		}
	}
	return wire.DaemonSession{}, false
}

func (b *fakeBook) Watch() (<-chan string, func()) { return b.ch, func() {} }

// put adds or replaces the session s, newest first.
func (b *fakeBook) put(s wire.DaemonSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.sessions {
		if b.sessions[i].ID == s.ID {
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			break
		}
	}
	b.sessions = append([]wire.DaemonSession{s}, b.sessions...)
}

type fakeSender struct {
	mu   sync.Mutex
	no   bool
	err  error
	sent []string // "<session>: <text>"
}

func (f *fakeSender) CanSend(wire.DaemonSession) bool { return !f.no }

func (f *fakeSender) Deliver(_ context.Context, s wire.DaemonSession, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, s.ID+": "+text)
	return nil
}

func (f *fakeSender) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

type fakeLauncher struct {
	mu   sync.Mutex
	rts  []string
	reqs []wire.LaunchRequest
}

func (f *fakeLauncher) Launch(_ context.Context, rt string, _ runtime.Order, r wire.LaunchRequest) (runtime.Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rts, f.reqs = append(f.rts, rt), append(f.reqs, r)
	id := fmt.Sprintf("new%d", len(f.reqs))
	return runtime.Started{Session: wire.SessionRef{ID: id}, PaneKey: id, TurnStarted: true}, nil
}

type fakePicker struct{}

func (fakePicker) Pick(_ context.Context, id string) (string, error) {
	if id == "" {
		return wire.RuntimeOversire, nil
	}
	return id, nil
}

// clock is a test clock that moves only when the test moves it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// rig is an engine with fakes around it.
type rig struct {
	*Engine
	book   *fakeBook
	sender *fakeSender
	launch *fakeLauncher
	clock  *clock
	store  string
	run    Runner // proc.Local when nil
	events []wire.ReviewEvent
	rows   []wire.EventDraft
	mu     sync.Mutex
}

// Add takes the event rows of the engine.
func (r *rig) Add(_ string, d wire.EventDraft) {
	r.mu.Lock()
	r.rows = append(r.rows, d)
	r.mu.Unlock()
}

// row gives the first event row of the kind.
func (r *rig) row(kind string) (wire.EventDraft, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.rows {
		if d.Kind == kind {
			return d, true
		}
	}
	return wire.EventDraft{}, false
}

func newRig(t *testing.T, roots ...string) *rig {
	t.Helper()
	r := &rig{book: newFakeBook(), sender: &fakeSender{}, launch: &fakeLauncher{},
		clock: &clock{at: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, store: t.TempDir()}
	r.Engine = r.engine(roots...)
	return r
}

// engine makes an engine on the store and the fakes of r, as a daemon restart does.
func (r *rig) engine(roots ...string) *Engine {
	var run Runner = proc.Local{}
	if r.run != nil {
		run = r.run
	}
	e := NewEngine(Deps{Store: NewStore(r.store), Git: NewGit(run), Repos: fakeRepos{roots}, Sessions: r.book,
		Sender: r.sender, Launcher: r.launch, Picker: fakePicker{}, Machine: "mbp", Order: Order{"oversire", "cli"},
		Notify: func(ev wire.ReviewEvent) { r.mu.Lock(); r.events = append(r.events, ev); r.mu.Unlock() }, Clock: r.clock.now, Events: r})
	e.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return e
}

func (r *rig) eventNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		out = append(out, ev.Event)
	}
	return out
}

func kindIs(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}
