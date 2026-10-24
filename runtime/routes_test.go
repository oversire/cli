package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// catalog is a fake with every optional part. down makes it unreachable and its lists fail.
type catalog struct {
	fake
	down     bool
	removed  string
	rm       [2]bool
	revealed wire.SessionRef
}

func (c *catalog) ID() string { return "cat" }
func (c *catalog) Probe(context.Context) wire.DaemonRuntime {
	d := wire.DaemonRuntime{Name: "Cat", Installed: true, Reachable: !c.down, Version: "1.0"}
	if c.down {
		d.Error = "Cat is not running."
	}
	return d
}
func (c *catalog) Repos(context.Context) ([]wire.DaemonRepo, error) {
	if c.down {
		return nil, errors.New("down")
	}
	return []wire.DaemonRepo{{ID: "r1", Runtime: "cat", Name: "towertail", Path: "/r/towertail"}}, nil
}
func (c *catalog) Workspaces(context.Context) ([]wire.DaemonWorkspace, error) {
	if c.down {
		return nil, errors.New("Cat is not running.")
	}
	return []wire.DaemonWorkspace{{ID: "w1", Runtime: "cat", Path: "/w/1", Kind: wire.KindWorktree}}, nil
}
func (c *catalog) RemoveWorkspace(_ context.Context, id string, force, runHooks bool) (wire.WorkspaceRemoval, error) {
	if id != "repo::/w/1" {
		return wire.WorkspaceRemoval{}, ErrNotFound
	}
	c.removed, c.rm = id, [2]bool{force, runHooks}
	return wire.WorkspaceRemoval{Warning: "kept the branch"}, nil
}
func (c *catalog) Reveal(_ context.Context, s wire.SessionRef) error {
	c.revealed = s
	return nil
}

func newAPI(t *testing.T, rts ...Runtime) (*httptest.Server, string) {
	dir := t.TempDir()
	api := NewAPI(NewRegistry(rts...), dir, time.Now)
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, dir
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestAPIRuntimes(t *testing.T) {
	srv, _ := newAPI(t, &fake{}, &catalog{down: true})
	var page wire.DaemonRuntimesPage
	if code := call(t, srv, "GET", "/v1/runtimes", nil, &page); code != 200 || len(page.Runtimes) != 2 {
		t.Fatal(code, page)
	}
	cat, plain := page.Runtimes[0], page.Runtimes[1]
	if cat.ID != "cat" || cat.Reachable || cat.Error == "" || len(cat.Capabilities) != 0 {
		t.Fatalf("%+v", cat)
	}
	if !plain.Reachable || slices.Contains(plain.Capabilities, CapReveal) || !slices.Contains(plain.Capabilities, CapHostFiles) {
		t.Fatalf("%+v", plain)
	}

	srv, _ = newAPI(t, &catalog{})
	_ = call(t, srv, "GET", "/v1/runtimes", nil, &page)
	for _, c := range []string{CapWorkspaces, CapReveal, CapRemoveWorkspace} {
		if !slices.Contains(page.Runtimes[0].Capabilities, c) {
			t.Fatal(c, page.Runtimes[0].Capabilities)
		}
	}
}

type retrier struct {
	catalog
	tries int
}

func (r *retrier) RetryStream(context.Context) { r.tries++ }
func (r *retrier) StreamState() wire.RuntimeStream {
	if r.tries == 0 {
		return wire.RuntimeStream{State: wire.StreamTextOnly, Reason: "no pairing"}
	}
	return wire.RuntimeStream{State: wire.StreamLive}
}

func TestAPIRetry(t *testing.T) {
	rt := &retrier{}
	srv, _ := newAPI(t, rt, &fake{})
	var d wire.DaemonRuntime
	if code := call(t, srv, "POST", "/v1/runtimes/cat/retry", nil, &d); code != 200 || rt.tries != 1 || d.ID != "cat" || d.Stream == nil || d.Stream.State != wire.StreamLive {
		t.Fatal(code, rt.tries, d)
	}
	var plain wire.DaemonRuntime
	if code := call(t, srv, "POST", "/v1/runtimes/"+(&fake{}).ID()+"/retry", nil, &plain); code != 200 || !plain.Reachable {
		t.Fatal("a runtime with no retry gives its state", code, plain)
	}
	if code := call(t, srv, "POST", "/v1/runtimes/nope/retry", nil, nil); code != 404 {
		t.Fatal(code)
	}
}

type pairer struct {
	retrier
	link string
}

func (p *pairer) PairStream(_ context.Context, link string) error {
	if link != "good" {
		return errors.New("bad link")
	}
	p.link, p.tries = link, 1
	return nil
}
func (p *pairer) UnpairStream(context.Context) { p.link, p.tries = "", 0 }

func TestAPIPair(t *testing.T) {
	rt := &pairer{}
	srv, _ := newAPI(t, rt, &fake{})
	var d wire.DaemonRuntime
	if code := call(t, srv, "POST", "/v1/runtimes/cat/pair", wire.RuntimePair{Link: "good"}, &d); code != 200 || rt.link != "good" || d.Stream.State != wire.StreamLive {
		t.Fatal(code, d)
	}
	if code := call(t, srv, "POST", "/v1/runtimes/cat/pair", wire.RuntimePair{Link: "bad"}, nil); code != 400 {
		t.Fatal(code)
	}
	if code := call(t, srv, "POST", "/v1/runtimes/cat/pair", nil, nil); code != 400 {
		t.Fatal("no link", code)
	}
	if code := call(t, srv, "DELETE", "/v1/runtimes/cat/pair", nil, &d); code != 200 || rt.link != "" || d.Stream.State != wire.StreamTextOnly {
		t.Fatal(code, d)
	}
	if code := call(t, srv, "POST", "/v1/runtimes/"+(&fake{}).ID()+"/pair", wire.RuntimePair{Link: "good"}, nil); code != 501 {
		t.Fatal("a runtime with no pairing", code)
	}
	if code := call(t, srv, "DELETE", "/v1/runtimes/nope/pair", nil, nil); code != 404 {
		t.Fatal(code)
	}
}

func TestAPIRepos(t *testing.T) {
	srv, _ := newAPI(t, &fake{}, &catalog{})
	var page wire.DaemonReposPage
	if code := call(t, srv, "GET", "/v1/repos", nil, &page); code != 200 || len(page.Repos) != 1 || page.Repos[0].Name != "towertail" {
		t.Fatal(code, page)
	}
	srv, _ = newAPI(t, &catalog{down: true})
	if code := call(t, srv, "GET", "/v1/repos", nil, &page); code != 200 || page.Repos == nil || len(page.Repos) != 0 {
		t.Fatal(code, page)
	}
}

func TestAPIWorkspaces(t *testing.T) {
	srv, _ := newAPI(t, &catalog{})
	var page wire.DaemonWorkspacesPage
	if code := call(t, srv, "GET", "/v1/workspaces", nil, &page); code != 200 || len(page.Workspaces) != 1 || page.Errors != nil {
		t.Fatal(code, page)
	}
	srv, _ = newAPI(t, &catalog{down: true})
	page = wire.DaemonWorkspacesPage{}
	if code := call(t, srv, "GET", "/v1/workspaces", nil, &page); code != 200 || len(page.Workspaces) != 0 || page.Errors["cat"] != "Cat is not running." {
		t.Fatal(code, page)
	}
}

func TestAPIRemoveWorkspace(t *testing.T) {
	c := &catalog{}
	srv, _ := newAPI(t, c, &fake{})
	var res wire.WorkspaceRemoval
	// An Orca worktree id has "/" in it. The app escapes it as %2F.
	path := "/v1/workspaces/repo::%2Fw%2F1"
	if code := call(t, srv, "DELETE", path, wire.WorkspaceRemove{Runtime: "cat", Force: true}, &res); code != 200 || res.Warning != "kept the branch" {
		t.Fatal(code, res)
	}
	if c.removed != "repo::/w/1" || c.rm != [2]bool{true, false} {
		t.Fatal(c.removed, c.rm)
	}
	cases := []struct {
		path string
		body any
		code int
	}{
		{"/v1/workspaces/nope", wire.WorkspaceRemove{Runtime: "cat"}, 404},
		{path, wire.WorkspaceRemove{Runtime: "orca"}, 404},
		{path, wire.WorkspaceRemove{Runtime: "fake"}, 501},
		{path, map[string]any{"runtime": "cat", "purge": true}, 400},
	}
	for _, tc := range cases {
		if code := call(t, srv, "DELETE", tc.path, tc.body, nil); code != tc.code {
			t.Fatal(tc, code)
		}
	}
}

func TestAPIReveal(t *testing.T) {
	for _, r := range NewAPI(NewRegistry(), "", time.Now).Routes() {
		if (r.Pattern == "POST /v1/reveal") != (r.Access == server.LoopbackOnly) {
			t.Fatal(r.Pattern, r.Access)
		}
	}
	c := &catalog{}
	srv, _ := newAPI(t, c, &fake{})
	s := wire.SessionRef{ID: "t-1", Workspace: wire.WorkspaceRef{ID: "w1", Account: wire.AccountID{Provider: "cat", ID: "local"}}}
	if code := call(t, srv, "POST", "/v1/reveal", s, nil); code != 204 || c.revealed != s {
		t.Fatal(code, c.revealed)
	}
	s.Workspace.Account.Provider = "fake"
	if code := call(t, srv, "POST", "/v1/reveal", s, nil); code != 501 {
		t.Fatal(code)
	}
	s.Workspace.Account.Provider = "orca"
	if code := call(t, srv, "POST", "/v1/reveal", s, nil); code != 404 {
		t.Fatal(code)
	}
}

func TestAPIFiles(t *testing.T) {
	srv, dir := newAPI(t, &fake{})
	old := filepath.Join(dir, "old")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	week := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(old, week, week)

	var res wire.WorkspaceFilesResult
	body := wire.WorkspaceFiles{Files: []wire.WorkspaceFile{{Name: "shot.png", Data: []byte{1, 2}}, {Name: "notes.txt", Data: []byte("hi")}}}
	if code := call(t, srv, "POST", "/v1/files", body, &res); code != 200 || len(res.Paths) != 2 {
		t.Fatal(code, res)
	}
	if !strings.HasPrefix(res.Paths[0], dir+string(filepath.Separator)) || filepath.Base(res.Paths[1]) != "notes.txt" || filepath.Dir(res.Paths[0]) != filepath.Dir(res.Paths[1]) {
		t.Fatal(res.Paths)
	}
	if b, err := os.ReadFile(res.Paths[1]); err != nil || string(b) != "hi" {
		t.Fatal(string(b), err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old folder stays:", err)
	}

	many := make([]wire.WorkspaceFile, MaxFiles+1)
	for i := range many {
		many[i] = wire.WorkspaceFile{Name: string(rune('a'+i)) + ".txt"}
	}
	bad := [][]wire.WorkspaceFile{
		{},
		many,
		{{Name: "../x"}},
		{{Name: "a/b"}},
		{{Name: ".env"}},
		{{Name: ".."}},
		{{Name: "a"}, {Name: "a"}},
		{{Name: "big", Data: make([]byte, MaxFileBytes+1)}},
	}
	for _, fs := range bad {
		if code := call(t, srv, "POST", "/v1/files", wire.WorkspaceFiles{Files: fs}, nil); code != 400 {
			t.Fatal(len(fs), code)
		}
	}
}

func TestAPILogsAWorkspacesErrorWhenItChanges(t *testing.T) {
	var buf bytes.Buffer
	a := NewAPI(NewRegistry(), t.TempDir(), time.Now)
	a.Log = slog.New(slog.NewTextHandler(&buf, nil))
	down := map[string]string{"orca": "Orca is not running."}
	a.logErrors(down)
	a.logErrors(down)
	a.logErrors(map[string]string{"orca": "timeout"})
	a.logErrors(nil)
	a.logErrors(nil)
	got := buf.String()
	if strings.Count(got, "level=WARN") != 2 || strings.Count(got, "work again") != 1 || !strings.Contains(got, `err="Orca is not running."`) {
		t.Fatal(got)
	}
}
