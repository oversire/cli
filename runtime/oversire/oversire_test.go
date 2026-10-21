package oversire

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/runner/proc"
	"github.com/oversire/runner/runtime"
	"github.com/oversire/runner/wire"
)

// chat answers each prompt with one turn and stays alive until stdin closes.
const chat = `n=0
while read -r line; do
  case "$line" in
    *'"type":"user"'*) n=$((n+1)); echo "{\"type\":\"assistant\",\"text\":\"turn $n\"}"; echo '{"type":"result","subtype":"success"}';;
  esac
done`

// asks asks for a permission in the first turn and tells the decision.
const asks = `read -r line
echo '{"type":"permission","id":"p-1","tool":"Bash","question":"npm test"}'
read -r d
case "$d" in
  *'"behavior":"allow"'*) echo '{"type":"assistant","text":"allowed"}';;
  *) echo '{"type":"assistant","text":"denied"}';;
esac
echo '{"type":"result","subtype":"success"}'
while read -r l; do :; done`

type recorder struct {
	mu  sync.Mutex
	all []wire.RunnerSession
}

func (r *recorder) Report(s wire.RunnerSession) {
	r.mu.Lock()
	r.all = append(r.all, s)
	r.mu.Unlock()
}

func (r *recorder) latest(id string) (wire.RunnerSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.all) - 1; i >= 0; i-- {
		if r.all[i].ID == id {
			return r.all[i], true
		}
	}
	return wire.RunnerSession{}, false
}

func (r *recorder) states(id string) []wire.RunnerSessionState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []wire.RunnerSessionState
	for _, s := range r.all {
		if s.ID == id && (len(out) == 0 || out[len(out)-1] != s.State) {
			out = append(out, s.State)
		}
	}
	return out
}

// wait gives the first report of id in state.
func (r *recorder) wait(t *testing.T, id string, state wire.RunnerSessionState) wire.RunnerSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := r.latest(id); ok && s.State == state {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := r.latest(id)
	t.Fatalf("session %s is %s, not %s (%s)", id, s.State, state, s.Error)
	return s
}

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// The user's own hooks and settings stay out of the test.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(t.TempDir(), "app")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return repo
}

type fixture struct {
	rt   *Runtime
	rec  *recorder
	repo string
	data string
}

func setup(t *testing.T, script string, opts Options) fixture {
	t.Helper()
	return setupWith(t, script, opts, func(aw *agentwire.Runtime) Harnesses { return aw })
}

// setupWith lets a test wrap the agent-wire runtime.
func setupWith(t *testing.T, script string, opts Options, wrap func(*agentwire.Runtime) Harnesses) fixture {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the fake harness needs a POSIX sh")
	}
	aw := agentwire.New(agentwire.Options{ClientName: "oversire-runner-test"})
	aw.SetDriver(agentwire.Fake, agentwire.FakeDriver{Script: script})
	opts.Runner, opts.Fake = "r1", true
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	f := fixture{rec: &recorder{}, repo: gitRepo(t), data: opts.DataDir}
	f.rt = New(opts, wrap(aw), proc.Local{}, f.rec)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.rt.Close(ctx)
		aw.Close(ctx)
	})
	return f
}

func (f fixture) launch(t *testing.T, name string, o runtime.Order) wire.SessionRef {
	t.Helper()
	ref, err := f.rt.Start(context.Background(), o, wire.LaunchRequest{Repo: f.repo, Name: name, Agent: "fake", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestStartReadSendStop(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	item := &wire.ItemRef{Project: wire.ProjectRef{Account: wire.AccountID{Provider: "plane", ID: "ocral"}, ID: "p", Key: "APP"}, ID: "9a", Key: "APP-2", URL: "https://x/APP-2"}
	ref, err := f.rt.Start(ctx, runtime.Order{ID: "o1"}, wire.LaunchRequest{Repo: f.repo, Item: item, Agent: "fake", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	want := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/APP-2"}, ID: ref.ID}
	if ref != want || !strings.HasPrefix(ref.ID, "s-") {
		t.Fatalf("ref %#v", ref)
	}
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Runner != "r1" || s.Order != "o1" || s.Agent != "fake" || s.Branch != "app-2" || s.Item.Key != "APP-2" || s.Summary != "turn 1" || s.Session != ref {
		t.Fatalf("session %#v", s)
	}
	if got := f.rec.states(ref.ID); !slices.Equal(got, []wire.RunnerSessionState{"queued", "starting", "working", "waitingInput"}) {
		t.Fatalf("states %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.data, "worktrees", "app", "APP-2", ".git")); err != nil {
		t.Fatal("no worktree:", err)
	}

	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Text: "more"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s, _ = f.rec.latest(ref.ID); s.Summary != "turn 2" && time.Now().Before(deadline); s, _ = f.rec.latest(ref.ID) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.State != wire.SessionWaitingInput || s.Summary != "turn 2" {
		t.Fatalf("after send %#v", s)
	}
	lines, err := f.rt.Read(ctx, ref, 200)
	if err != nil || !slices.Equal(lines, []string{"> go", "turn 1", "> more", "turn 2"}) {
		t.Fatalf("lines %q %v", lines, err)
	}
	if lines, _ := f.rt.Read(ctx, ref, 1); !slices.Equal(lines, []string{"turn 2"}) {
		t.Fatalf("tail %q", lines)
	}

	if err := f.rt.Stop(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.rec.latest(ref.ID); s.State != wire.SessionDone || s.Error != "" {
		t.Fatalf("after stop %#v", s)
	}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Text: "x"}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("send after stop: %v", err)
	}
	if err := f.rt.Stop(ctx, ref); err != nil {
		t.Fatal("second stop:", err)
	}
	if _, err := os.Stat(filepath.Join(f.data, "worktrees", "app", "APP-2")); err != nil {
		t.Fatal("stop removed the worktree")
	}

	// A new runtime reads the transcript file.
	again := New(Options{DataDir: f.data, Fake: true}, agentwire.New(agentwire.Options{}), proc.Local{}, nil)
	if lines, err := again.Read(ctx, ref, 2); err != nil || !slices.Equal(lines, []string{"> more", "turn 2"}) {
		t.Fatalf("file %q %v", lines, err)
	}
	if _, err := again.Read(ctx, wire.SessionRef{ID: "../config"}, 2); err == nil {
		t.Fatal("a bad session id passed")
	}
}

func TestPermission(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name    string
		answer  *bool
		timeout time.Duration
		says    string
	}{
		{"allow", &yes, 0, "allowed"},
		{"deny", &no, 0, "denied"},
		{"expiry deny", nil, 100 * time.Millisecond, "denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, asks, Options{PermissionTimeout: c.timeout})
			ctx := context.Background()
			ref := f.launch(t, "APP-7", runtime.Order{ID: "o1"})
			s := f.rec.wait(t, ref.ID, wire.SessionWaitingPermission)
			p := s.Permission
			if p == nil || p.ID != "p-1" || p.Tool != "Bash" || p.Text != "npm test" || !p.ExpiresAt.After(time.Now().Add(c.timeout/2)) {
				t.Fatalf("prompt %#v", p)
			}
			if c.answer != nil {
				bad := wire.SendPayload{Session: ref, Permission: &wire.PermissionAnswer{ID: "p-9", Allow: true}}
				if err := f.rt.Send(ctx, bad); err == nil || !strings.Contains(err.Error(), "no open permission prompt p-9") {
					t.Fatalf("wrong id: %v", err)
				}
				if err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Permission: &wire.PermissionAnswer{ID: "p-1", Allow: *c.answer}}); err != nil {
					t.Fatal(err)
				}
			}
			s = f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
			if s.Permission != nil || s.Summary != c.says {
				t.Fatalf("after answer %#v", s)
			}
			if got := f.rec.states(ref.ID); !slices.Contains(got, wire.SessionWaitingPermission) || got[len(got)-2] != wire.SessionWorking {
				t.Fatalf("states %v", got)
			}
			err := f.rt.Send(ctx, wire.SendPayload{Session: ref, Permission: &wire.PermissionAnswer{ID: "p-1", Allow: true}})
			if err == nil || !strings.Contains(err.Error(), "no open permission prompt.") {
				t.Fatalf("second answer: %v", err)
			}
		})
	}
}

func TestSlotQueue(t *testing.T) {
	f := setup(t, chat, Options{Slots: 2})
	ctx := context.Background()
	a := f.launch(t, "A-1", runtime.Order{ID: "o1"})
	b := f.launch(t, "B-1", runtime.Order{ID: "o2"})
	f.rec.wait(t, a.ID, wire.SessionWaitingInput)
	f.rec.wait(t, b.ID, wire.SessionWaitingInput)
	c := f.launch(t, "C-1", runtime.Order{ID: "o3", ExpiresAt: time.Now().Add(time.Hour)})
	time.Sleep(200 * time.Millisecond)
	if s, _ := f.rec.latest(c.ID); s.State != wire.SessionQueued {
		t.Fatalf("third session is %s", s.State)
	}
	if err := f.rt.Send(ctx, wire.SendPayload{Session: c, Text: "x"}); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("send to a queued session: %v", err)
	}
	if err := f.rt.Stop(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, c.ID, wire.SessionWaitingInput)
}

func TestQueuedOrderExpires(t *testing.T) {
	f := setup(t, chat, Options{Slots: 1})
	a := f.launch(t, "A-1", runtime.Order{ID: "o1"})
	f.rec.wait(t, a.ID, wire.SessionWaitingInput)
	b := f.launch(t, "B-1", runtime.Order{ID: "o2", ExpiresAt: time.Now().Add(100 * time.Millisecond)})
	s := f.rec.wait(t, b.ID, wire.SessionFailed)
	if !strings.Contains(s.Error, "expired") {
		t.Fatalf("error %q", s.Error)
	}
	if got := f.rec.states(b.ID); !slices.Equal(got, []wire.RunnerSessionState{"queued", "failed"}) {
		t.Fatalf("states %v", got)
	}
}

func TestStopQueued(t *testing.T) {
	f := setup(t, chat, Options{Slots: 1})
	a := f.launch(t, "A-1", runtime.Order{ID: "o1"})
	f.rec.wait(t, a.ID, wire.SessionWaitingInput)
	b := f.launch(t, "B-1", runtime.Order{ID: "o2"})
	if err := f.rt.Stop(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.rec.latest(b.ID); s.State != wire.SessionDone {
		t.Fatalf("stopped queued session %#v", s)
	}
}

func TestAgentExitFails(t *testing.T) {
	f := setup(t, `read -r line; echo 'no login' >&2; exit 3`, Options{})
	ref := f.launch(t, "A-1", runtime.Order{ID: "o1"})
	s := f.rec.wait(t, ref.ID, wire.SessionFailed)
	if !strings.Contains(s.Error, "no login") {
		t.Fatalf("error %q", s.Error)
	}
}

func TestStartRefuses(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	cases := map[string]wire.LaunchRequest{
		"no name":       {Repo: f.repo, Agent: "fake", Prompt: "go"},
		"path name":     {Repo: f.repo, Name: "../x", Agent: "fake", Prompt: "go"},
		"remote repo":   {Repo: "https://github.com/ada/app", Name: "A-1", Agent: "fake", Prompt: "go"},
		"agent":         {Repo: f.repo, Name: "A-1", Agent: "vim", Prompt: "go"},
		"native":        {Repo: f.repo, Name: "A-1", Agent: "native", Prompt: "go"},
		"not a git dir": {Repo: t.TempDir(), Name: "A-1", Agent: "fake", Prompt: "go"},
	}
	for name, r := range cases {
		if _, err := f.rt.Start(ctx, runtime.Order{ID: "o"}, r); err == nil {
			t.Errorf("%s: start passed", name)
		}
	}
	real := New(Options{DataDir: t.TempDir()}, agentwire.New(agentwire.Options{}), proc.Local{}, nil)
	_, err := real.Start(ctx, runtime.Order{ID: "o"}, wire.LaunchRequest{Repo: f.repo, Name: "A-1", Agent: "fake", Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), `agent "fake"`) {
		t.Fatalf("fake without the test option: %v", err)
	}
}

func TestWorktreeCreateAndReuse(t *testing.T) {
	repo := gitRepo(t)
	root := t.TempDir()
	w := NewWorktrees(root, proc.Local{})
	ctx := context.Background()
	dir, err := w.Ensure(ctx, repo, "APP-2", "app-2")
	if err != nil || dir != filepath.Join(root, "app", "APP-2") {
		t.Fatal(dir, err)
	}
	if b := gitOut(t, dir, "branch", "--show-current"); b != "app-2" {
		t.Fatalf("branch %q", b)
	}
	os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x"), 0o600)
	again, err := w.Ensure(ctx, repo, "APP-2", "app-2")
	if err != nil || again != dir {
		t.Fatal(again, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work.txt")); err != nil {
		t.Fatal("reuse lost the work")
	}

	// An existing branch gets a worktree without -b.
	gitOut(t, repo, "branch", "app-3")
	dir3, err := w.Ensure(ctx, repo, "APP-3", "app-3")
	if err != nil || gitOut(t, dir3, "branch", "--show-current") != "app-3" {
		t.Fatal(dir3, err)
	}

	os.MkdirAll(filepath.Join(root, "app", "APP-4"), 0o700)
	if _, err := w.Ensure(ctx, repo, "APP-4", "app-4"); err == nil || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("plain folder: %v", err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSlots(t *testing.T) {
	s := NewSlots(1)
	ctx := context.Background()
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := s.Acquire(short); err == nil {
		t.Fatal("a second slot")
	}
	s.Release()
	if err := s.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClipAndTail(t *testing.T) {
	long := strings.Repeat("é", 600)
	if c := clip(long, 500); len([]rune(c)) != 500 || !strings.HasSuffix(c, "…") {
		t.Fatal(len([]rune(c)))
	}
	if c := tail(long+"end", 500); len([]rune(c)) != 500 || !strings.HasPrefix(c, "…") || !strings.HasSuffix(c, "end") {
		t.Fatal(c)
	}
	if clip("ok", 500) != "ok" || tail("ok", 500) != "ok" {
		t.Fatal("short text changed")
	}
}
