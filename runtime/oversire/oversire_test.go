//go:build !windows

package oversire

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	"github.com/trebi-ai/agent-visor/holder"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
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
	rt    *Runtime
	rec   *recorder
	repo  string
	data  string
	visor *agentvisor.Visor
}

// TestMain runs the test binary as a visor holder when the visor starts one.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "hold" {
		os.Exit(runHolder(os.Args[2]))
	}
	os.Exit(m.Run())
}

func runHolder(id string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := holder.ReadConfig(os.Stdin, id)
	if err == nil {
		err = holder.Run(ctx, cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// shortHome is a visor home with a short path, for its sockets.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "ov")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// newVisor runs the fake harness with driver in the visor process. term is the TerminalCommand, or nil.
func newVisor(t *testing.T, home string, driver agentwire.Driver, term ...agentvisor.TerminalCommand) *agentvisor.Visor {
	t.Helper()
	o := agentvisor.Options{
		Home: home, Client: "oversire-test",
		Drivers: map[string]agentwire.Driver{string(agentwire.Fake): driver},
	}
	if len(term) > 0 {
		o.TerminalCommand = term[0]
	}
	v, err := agentvisor.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setup(t *testing.T, script string, opts Options) fixture {
	t.Helper()
	return setupWith(t, script, opts, func(v *agentvisor.Visor) Host { return v })
}

// setupWith lets a test wrap the visor.
func setupWith(t *testing.T, script string, opts Options, wrap func(*agentvisor.Visor) Host) fixture {
	t.Helper()
	return setupDriver(t, agentwire.FakeDriver{Script: script}, opts, wrap)
}

// setupDriver lets a test give the driver of the fake harness.
func setupDriver(t *testing.T, driver agentwire.Driver, opts Options, wrap func(*agentvisor.Visor) Host, term ...agentvisor.TerminalCommand) fixture {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the fake harness needs a POSIX sh")
	}
	f := fixture{rec: &recorder{}, repo: gitRepo(t)}
	f.visor = newVisor(t, shortHome(t), driver, term...)
	opts.Runner, opts.Fake = "r1", true
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	f.data = opts.DataDir
	f.rt = New(opts, wrap(f.visor), proc.Local{}, f.rec)
	if err := f.rt.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.rt.Close(ctx)
		_ = f.visor.Close(ctx)
	})
	return f
}

// again opens a second runtime on the visor of f, as after a restart of the daemon.
func (f fixture) again(t *testing.T, rec Reporter) *Runtime {
	t.Helper()
	rt := New(Options{Runner: "r1", DataDir: f.data, Fake: true}, f.visor, proc.Local{}, rec)
	if err := rt.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	return rt
}

// workdir gives the visor workdir of key.
func (f fixture) workdir(t *testing.T, key string) agentvisor.Workdir {
	t.Helper()
	wds, err := f.visor.Workdirs(context.Background(), agentvisor.Filter{Key: key})
	if err != nil || len(wds) != 1 {
		t.Fatalf("workdirs of %s: %v %v", key, wds, err)
	}
	return wds[0]
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
	w := f.workdir(t, "APP-2")
	if _, err := os.Stat(filepath.Join(w.Path, ".git")); err != nil || w.Kind != agentvisor.WorkdirWorktree || w.Branch != "app-2" {
		t.Fatalf("no worktree: %+v %v", w, err)
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
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatal("stop removed the worktree")
	}

	// A new runtime reads the session from the events of the visor.
	rec := &recorder{}
	again := f.again(t, rec)
	if lines, err := again.Read(ctx, ref, 2); err != nil || !slices.Equal(lines, []string{"> more", "turn 2"}) {
		t.Fatalf("again %q %v", lines, err)
	}
	if s, _ := rec.latest(ref.ID); s.State != wire.SessionDone || s.Summary != "turn 2" || s.Item == nil || s.Item.Key != "APP-2" || s.Session != ref || s.Order != "o1" {
		t.Fatalf("again %#v", s)
	}
	if _, err := again.Read(ctx, wire.SessionRef{ID: "../config"}, 2); err == nil {
		t.Fatal("a bad session id passed")
	}
}

func TestReadsOldTranscriptFile(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "sessions"), 0o700)
	os.WriteFile(filepath.Join(data, "sessions", "s-0a.log"), []byte("> go\nturn 1\n"), 0o600)
	rt := New(Options{DataDir: data}, nil, nil, nil)
	if lines, err := rt.Read(context.Background(), wire.SessionRef{ID: "s-0a"}, 1); err != nil || !slices.Equal(lines, []string{"turn 1"}) {
		t.Fatalf("lines %q %v", lines, err)
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
	real := New(Options{DataDir: t.TempDir()}, f.visor, proc.Local{}, nil)
	_, err := real.Start(ctx, runtime.Order{ID: "o"}, wire.LaunchRequest{Repo: f.repo, Name: "A-1", Agent: "fake", Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), `agent "fake"`) {
		t.Fatalf("fake without the test option: %v", err)
	}
}

func TestWorkdirKeyAndAdopt(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	w, err := f.rt.ensureWorkdir(ctx, f.repo, "APP-2", "app-2", "")
	if err != nil || w.Kind != agentvisor.WorkdirWorktree || gitOut(t, w.Path, "branch", "--show-current") != "app-2" {
		t.Fatal(w, err)
	}
	os.WriteFile(filepath.Join(w.Path, "work.txt"), []byte("x"), 0o600)
	again, err := f.rt.ensureWorkdir(ctx, f.repo, "APP-2", "app-2", "")
	if err != nil || again.ID != w.ID || again.Path != w.Path {
		t.Fatal(again, err)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "work.txt")); err != nil {
		t.Fatal("reuse lost the work")
	}

	// An existing branch gets a worktree without -b.
	gitOut(t, f.repo, "branch", "app-3")
	w3, err := f.rt.ensureWorkdir(ctx, f.repo, "APP-3", "app-3", "")
	if err != nil || gitOut(t, w3.Path, "branch", "--show-current") != "app-3" {
		t.Fatal(w3, err)
	}

	// The visor adopts a worktree from before agent-visor.
	old := filepath.Join(f.data, "worktrees", "app", "APP-4")
	gitOut(t, f.repo, "worktree", "add", "-b", "app-4", old)
	w4, err := f.rt.ensureWorkdir(ctx, f.repo, "APP-4", "app-4", "")
	real, _ := filepath.EvalSymlinks(old)
	if err != nil || w4.Path != real || w4.Kind != agentvisor.WorkdirWorktree || w4.Branch != "app-4" {
		t.Fatal(w4, err)
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

// startSpy keeps the last StartSpec.
type startSpy struct {
	*agentvisor.Visor
	mu   sync.Mutex
	spec agentvisor.StartSpec
}

func (s *startSpy) Start(ctx context.Context, spec agentvisor.StartSpec) (agentvisor.Session, error) {
	s.mu.Lock()
	s.spec = spec
	s.mu.Unlock()
	return s.Visor.Start(ctx, spec)
}

func TestStartPassesModelAndEffort(t *testing.T) {
	var spy *startSpy
	f := setupWith(t, chat, Options{}, func(v *agentvisor.Visor) Host {
		spy = &startSpy{Visor: v}
		return spy
	})
	ref, err := f.rt.Start(context.Background(), runtime.Order{ID: "o1"}, wire.LaunchRequest{Repo: f.repo, Name: "APP-9", Agent: "fake", Prompt: "go",
		Model: "claude-opus-5-5", Effort: "max", Profile: "claude-max"})
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	sp := spy.spec
	if sp.Model != "claude-opus-5-5" || sp.Effort != "max" || sp.Kind != agentvisor.KindWire || sp.Restart.Mode != agentvisor.RestartResume || sp.Labels[LabelSession] != ref.ID || sp.Labels[LabelRuntime] != ID || sp.Env["OVERSIRE_SESSION"] != ref.ID {
		t.Fatalf("start spec %#v", sp)
	}
}

// TestInPlaceUsesTheFolder starts a session in a folder that is not a git repo: no worktree, the folder is the working dir.
func TestInPlaceUsesTheFolder(t *testing.T) {
	f := setup(t, chat, Options{})
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	group := filepath.Join(dir, ".wt", "app-9")
	if err := os.MkdirAll(group, 0o700); err != nil {
		t.Fatal(err)
	}
	ref, err := f.rt.Start(ctx, runtime.Order{ID: "o1"}, wire.LaunchRequest{Repo: group, Name: "APP-9", InPlace: true, Agent: "fake", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	s := f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	if s.Repo != group || s.Branch != "" {
		t.Fatalf("session %#v", s)
	}
	vs, err := f.visor.Sessions(ctx, agentvisor.Filter{})
	if err != nil || len(vs) != 1 || vs[0].WorkingDir != group {
		t.Fatalf("visor sessions %#v %v", vs, err)
	}
	if out := gitOut(t, f.repo, "worktree", "list"); strings.Count(out, "\n") > 0 {
		t.Fatalf("a worktree: %s", out)
	}
}

func TestStartPassesTheEnvAndOversireSessionWins(t *testing.T) {
	var spy *startSpy
	f := setupWith(t, chat, Options{}, func(v *agentvisor.Visor) Host {
		spy = &startSpy{Visor: v}
		return spy
	})
	env := map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-\"q\"\nv", "OVERSIRE_SESSION": "not-this"}
	ref, err := f.rt.Start(context.Background(), runtime.Order{ID: "o1"}, wire.LaunchRequest{Repo: f.repo, Name: "APP-10", Agent: "fake", Prompt: "go", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if sp := spy.spec; sp.Env["ANTHROPIC_AUTH_TOKEN"] != env["ANTHROPIC_AUTH_TOKEN"] || sp.Env["OVERSIRE_SESSION"] != ref.ID {
		t.Fatalf("env %v", sp.Env)
	}
	if env["OVERSIRE_SESSION"] != "not-this" {
		t.Fatal("the spec changed the env map of the request")
	}
}
