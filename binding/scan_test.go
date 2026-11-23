package binding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/gitwatch"
	"github.com/oversire/cli/wire"
)

type fakeList []gitwatch.Worktree

func (f fakeList) Worktrees() []gitwatch.Worktree { return f }

// fakeGit answers status with status[dir] and rev-list with ahead[dir]. A dir in fail gives an error.
type fakeGit struct {
	status, ahead map[string]string
	fail          string
	calls         int
}

func (g *fakeGit) Run(_ context.Context, dir string, args ...string) (string, error) {
	g.calls++
	if dir == g.fail {
		return "", errors.New("not a git repository")
	}
	if args[0] == "status" {
		return g.status[dir], nil
	}
	if a, ok := g.ahead[dir]; ok {
		return a, nil
	}
	return "0", nil
}

type fakeSessions []wire.DaemonSession

func (f fakeSessions) All() []wire.DaemonSession { return f }

func wt(path, branch string) gitwatch.Worktree {
	return gitwatch.Worktree{Path: path, Branch: branch, Repo: "/r/app", Base: "main"}
}

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestScannerChangedOnly(t *testing.T) {
	g := &fakeGit{status: map[string]string{"/w/dirty": " M a.go"}, fail: "/w/broken"}
	s := NewScanner(fakeList{wt("/w/clean", "a"), wt("/w/dirty", "b"), wt("/w/broken", "c")}, g, fakeSessions{}, clockAt(time.Unix(100, 0)))
	got := s.Worktrees(context.Background(), true, "")
	if len(got.Worktrees) != 1 || got.Worktrees[0].Path != "/w/dirty" || got.Worktrees[0].Uncommitted != 1 {
		t.Fatalf("%+v", got)
	}
	if all := s.Worktrees(context.Background(), false, ""); len(all.Worktrees) != 2 {
		t.Fatalf("a git error must skip only that worktree: %+v", all)
	}
}

func TestScannerAheadAndCache(t *testing.T) {
	now := time.Unix(100, 0)
	g := &fakeGit{ahead: map[string]string{"/w/a": "2"}}
	s := NewScanner(fakeList{wt("/w/a", "a")}, g, fakeSessions{}, func() time.Time { return now })
	if got := s.Worktrees(context.Background(), true, ""); len(got.Worktrees) != 1 || got.Worktrees[0].Ahead != 2 {
		t.Fatalf("%+v", got)
	}
	calls := g.calls
	now = now.Add(10 * time.Second)
	s.Worktrees(context.Background(), false, "")
	if g.calls != calls {
		t.Fatal("a read inside 30 s must use the cache")
	}
	now = now.Add(30 * time.Second)
	s.Worktrees(context.Background(), false, "")
	if g.calls == calls {
		t.Fatal("a read after 30 s must read again")
	}
}

func TestScannerGuessOrder(t *testing.T) {
	cases := []struct {
		name     string
		wt       gitwatch.Worktree
		sessions fakeSessions
		want     *wire.WorktreeGuess
	}{
		{"prompt wins", wt("/w/x-9", "x-8"), fakeSessions{{ID: "s1", Cwd: "/w/x-9/sub", Prompt: "Fix X-2 now"}}, &wire.WorktreeGuess{Key: "X-2", From: "prompt"}},
		{"session key", wt("/w/quick", ""), fakeSessions{{ID: "s1", Cwd: "/w/quick", Key: "X-5"}}, &wire.WorktreeGuess{Key: "X-5", From: "prompt"}},
		{"two keys fall back to the path", wt("/w/x-9", "x-8"), fakeSessions{{ID: "s1", Cwd: "/w/x-9", Prompt: "X-2 and X-3"}}, &wire.WorktreeGuess{Key: "X-9", From: "cwd"}},
		{"two keys fall back to the branch", wt("/w/quick-fix", "x-8-menu"), fakeSessions{{ID: "s1", Cwd: "/w/quick-fix", Prompt: "X-2 and X-3"}}, &wire.WorktreeGuess{Key: "X-8", From: "branch"}},
		{"group folder", wt("/root/.wt/x-4/app", "feature"), nil, &wire.WorktreeGuess{Key: "X-4", From: "cwd"}},
		{"far parent is no guess", wt("/tmp/claude-501/a/quick", "quick"), nil, nil},
		{"session elsewhere", wt("/w/quick", ""), fakeSessions{{ID: "s1", Cwd: "/w/quick2", Key: "X-5"}}, nil},
	}
	for _, c := range cases {
		_, got := guess(c.wt, c.sessions)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestScannerNoPromptText(t *testing.T) {
	s := NewScanner(fakeList{wt("/w/a", "a")}, &fakeGit{status: map[string]string{"/w/a": "?? n"}},
		fakeSessions{{ID: "s1", Cwd: "/w/a", Prompt: "Secret plan for X-2"}}, clockAt(time.Unix(100, 0)))
	b, _ := json.Marshal(s.Worktrees(context.Background(), false, ""))
	if strings.Contains(string(b), "Secret") || strings.Contains(string(b), "prompt\":\"") {
		t.Fatalf("the answer has prompt text: %s", b)
	}
	if !strings.Contains(string(b), `"session":"s1"`) {
		t.Fatalf("no session: %s", b)
	}
}

func TestWorktreesRoute(t *testing.T) {
	s := NewScanner(fakeList{wt("/w/a", "a"), wt("/v/b", "b")}, &fakeGit{}, fakeSessions{}, clockAt(time.Unix(100, 0)))
	get := func(q string) (int, wire.WorktreesPage) {
		w := httptest.NewRecorder()
		s.serve(w, httptest.NewRequest(http.MethodGet, "/v1/worktrees?"+q, nil))
		var p wire.WorktreesPage
		json.Unmarshal(w.Body.Bytes(), &p)
		return w.Code, p
	}
	if code, p := get(""); code != 200 || len(p.Worktrees) != 2 {
		t.Fatalf("all: %d %+v", code, p)
	}
	if code, p := get("dir=/w"); code != 200 || len(p.Worktrees) != 1 || p.Worktrees[0].Path != "/w/a" {
		t.Fatalf("dir: %d %+v", code, p)
	}
	if code, p := get("changed=1"); code != 200 || p.Worktrees == nil || len(p.Worktrees) != 0 {
		t.Fatalf("changed: %d %+v", code, p)
	}
	if code, _ := get("dir=rel"); code != 400 {
		t.Fatalf("a relative dir: %d", code)
	}
}
