package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/review"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

type noRepos struct{}

func (noRepos) Repos(context.Context) wire.DaemonReposPage { return wire.DaemonReposPage{} }

// reviewDaemon runs a real server with the review engine on a free loopback port, and points the config of c at it.
// The daemon knows no roots: the hand-off names them.
func reviewDaemon(t *testing.T, c *cli) (*sessions.Book, <-chan wire.ReviewEvent) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	off := false
	cfg := config.ServeConfig{Port: port, Tailscale: &off}
	if err := c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &cfg}); err != nil {
		t.Fatal(err)
	}
	events := eventlog.New(10)
	book := sessions.NewBook("mbp", "", events, nil)
	info := func() wire.DaemonInfo {
		return wire.DaemonInfo{Machine: "mbp", Version: "test", Contract: wire.DaemonContract}
	}
	srv := server.NewServer(cfg, info, nil, events, book, nil)
	srv.Log = c.log
	engine := review.NewEngine(review.Deps{Store: review.NewStore(filepath.Join(t.TempDir(), "reviews")), Git: review.NewGit(proc.Local{}),
		Repos: noRepos{}, Sessions: book, Launcher: &bookLauncher{book: book}, Picker: onePicker{}, Machine: "mbp",
		Notify: func(ev wire.ReviewEvent) { srv.Notify(false, ev) }})
	engine.Log = c.log
	srv.Handle(review.NewRoutes(engine).Routes()...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var conn *websocket.Conn
	for i := 0; ; i++ {
		dctx, dcancel := context.WithTimeout(ctx, time.Second)
		conn, _, err = websocket.Dial(dctx, "ws://127.0.0.1:"+strconv.Itoa(port)+"/v1/connect", nil)
		dcancel()
		if err == nil {
			break
		}
		if i == 50 {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := make(chan wire.ReviewEvent, 64)
	go func() {
		defer close(out)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var ev wire.ReviewEvent
			if json.Unmarshal(data, &ev) == nil && ev.Type == "review" {
				out <- ev
			}
		}
	}()
	t.Cleanup(func() { conn.CloseNow() })
	return book, out
}

// gitGroup makes <root>/app on main and the group <root>/.wt/app-1 with one commit.
func gitGroup(t *testing.T) (root, wt string) {
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
	root, _ = filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(root, "app")
	wt = filepath.Join(root, ".wt", "app-1", "app")
	run := func(dir string, args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run(root, "init", "-q", "-b", "main", app)
	run(app, "commit", "-q", "--allow-empty", "-m", "init")
	run(app, "worktree", "add", "-q", "-b", "app-1", wt, "main")
	os.WriteFile(filepath.Join(wt, "n.go"), []byte("package app\n\nfunc N() {}\n"), 0o644)
	run(wt, "add", "-A")
	run(wt, "commit", "-q", "-m", "Add N")
	return root, wt
}

func awaitReview(t *testing.T, frames <-chan wire.ReviewEvent, event string) wire.ReviewEvent {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-frames:
			if !ok {
				t.Fatal("the socket closed")
			}
			if ev.Event == event {
				return ev
			}
		case <-timeout:
			t.Fatal("no frame", event)
		}
	}
}

func TestReviewReadyAndReply(t *testing.T) {
	c, out, _ := testCLI(t, "")
	_, frames := reviewDaemon(t, c)
	root, wt := gitGroup(t)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := c.dispatch(ctx, append([]string{"review"}, args...)); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(out.String())
	}

	// In the worktree, the group gives the key and the root.
	c.wd = wt
	if s := run("ready", "--note", "Adds N."); s != "Review of APP-1 is ready. The user reviews it in OverSire. Do not merge." {
		t.Fatal(s)
	}
	ev := awaitReview(t, frames, wire.EventReviewReady)
	if ev != (wire.ReviewEvent{Type: "review", Event: wire.EventReviewReady, Key: "APP-1", Kind: wire.ReviewCode, Machine: "mbp"}) {
		t.Fatalf("%+v", ev)
	}
	if s := run("ls"); !strings.Contains(s, "APP-1") || !strings.Contains(s, "Add N") {
		t.Fatal(s)
	}

	// The user comments and submits. The session ended and no agent is known: the round keeps the error.
	d, _ := c.daemonClient()
	var cm wire.ReviewComment
	if err := d.call(ctx, "POST", "/v1/reviews/APP-1/comments", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "Rename N."}, &cm); err != nil {
		t.Fatal(err)
	}
	var round wire.ReviewRound
	if err := d.call(ctx, "POST", "/v1/reviews/APP-1/submit", wire.SubmitReview{Verdict: wire.VerdictRequestChanges, Launch: &wire.AgentLaunch{CLI: "claude"}}, &round); err != nil {
		t.Fatal(err)
	}
	if round.Delivery != wire.DeliveryNewSession || round.Session == "" {
		t.Fatalf("%+v", round)
	}

	// --key wins over the working folder: from the root, with --kind code.
	c.wd = root
	s := run("ready", "--key", "APP-1", "--kind", "code", "--note", "Renamed.")
	if !strings.Contains(s, "1 comments have no answer") || !strings.Contains(s, "oversire review reply --key APP-1 "+cm.ID) {
		t.Fatal(s)
	}
	// From a folder outside the group, the reply finds the review of the comment.
	c.wd = t.TempDir()
	if s := run("reply", cm.ID, "Renamed", "to", "New."); s != "Comment "+cm.ID+" of APP-1: answered." {
		t.Fatal(s)
	}
	var det wire.ReviewDetail
	d.call(ctx, "GET", "/v1/reviews/APP-1", nil, &det)
	if det.Comments[0].Reply != "Renamed to New." || det.Comments[0].State != wire.CommentAnswered {
		t.Fatalf("%+v", det.Comments)
	}
	if err := c.dispatch(ctx, []string{"review", "reply", "--key", "APP-1", "c_00000000", "x"}); err == nil || !strings.Contains(err.Error(), "no comment c_00000000") {
		t.Fatal(err)
	}
}

func TestReviewReadyPlan(t *testing.T) {
	c, out, _ := testCLI(t, "")
	book, frames := reviewDaemon(t, c)
	root := t.TempDir()
	plan := "# Review gate\n\nOne gate for all work.\n\n## Goal\n\nShip it.\n\n```\n## not a heading\n```\n\n## Sub-items\n\n- APP-3: Engine\n- [ ] Routes\n"
	os.MkdirAll(filepath.Join(root, ".oversire", "plans"), 0o755)
	os.WriteFile(filepath.Join(root, ".oversire", "plans", "APP-2.md"), []byte(plan), 0o644)
	c.wd = root
	ctx := context.Background()
	if err := c.dispatch(ctx, []string{"review", "ready", "--key", "APP-2", "--kind", "plan", "--plan", ".oversire/plans/APP-2.md", "--note", "First plan."}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "Plan of APP-2: version 1, 3 blocks.") || !strings.Contains(s, "Review of the plan of APP-2 is ready.") {
		t.Fatal(s)
	}
	awaitReview(t, frames, wire.EventReviewReady)
	d, _ := c.daemonClient()
	var det wire.ReviewDetail
	if err := d.call(ctx, "GET", "/v1/reviews/APP-2?kind=plan", nil, &det); err != nil {
		t.Fatal(err)
	}
	p := det.Plan
	if p.Title != "Review gate" || p.Root != root || len(p.Blocks) != 3 || p.Blocks[0].ID != "summary" || p.Blocks[2].ID != "sub-items" ||
		!strings.Contains(p.Blocks[1].Markdown, "## not a heading") || det.Summary.Note != "First plan." {
		t.Fatalf("%+v", p)
	}
	if !reflect.DeepEqual(p.Subitems, []wire.PlanSubitem{{Key: "APP-3", Title: "Engine"}, {Title: "Routes"}}) {
		t.Fatalf("%+v", p.Subitems)
	}

	// The user asks for changes. The plan session ended, so a new one starts with the key plan:APP-2 in the root.
	var cm wire.ReviewComment
	d.call(ctx, "POST", "/v1/reviews/APP-2/comments", wire.CommentDraft{Kind: wire.ReviewPlan, Anchor: wire.Anchor{Block: "goal"}, Text: "Say why."}, &cm)
	var round wire.ReviewRound
	if err := d.call(ctx, "POST", "/v1/reviews/APP-2/submit", wire.SubmitReview{Kind: wire.ReviewPlan, Verdict: wire.VerdictRequestChanges,
		Launch: &wire.AgentLaunch{CLI: "claude"}}, &round); err != nil {
		t.Fatal(err)
	}
	s, ok := book.Get(round.Session)
	if !ok || s.Key != "plan:APP-2" || !strings.Contains(s.Prompt, "Change the plan in "+root) {
		t.Fatalf("%+v", s)
	}
}

func TestReviewUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	c.wd = t.TempDir()
	for _, args := range [][]string{
		{"review"},
		{"review", "merge"},
		{"review", "ready", "--kind", "plan"},
		{"review", "ready", "--plan", "p.md", "--kind", "code"},
		{"review", "ready"},
		{"review", "reply", "c_1"},
	} {
		var e *exitError
		if err := c.dispatch(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestGroupOf(t *testing.T) {
	for dir, want := range map[string][2]string{
		"/w/.wt/app-1":           {"/w", "app-1"},
		"/w/.wt/app-1/cli/cmd/x": {"/w", "app-1"},
		"/w/cli":                 {"", ""},
		"/":                      {"", ""},
	} {
		root, name, ok := groupOf(dir)
		if root != want[0] || name != want[1] || ok != (want[1] != "") {
			t.Errorf("%s: %s %s %v", dir, root, name, ok)
		}
	}
}

func TestParsePlan(t *testing.T) {
	p := parsePlan("APP-1", "/w", "## Goal\nA\n## Goal\nB\n## ???\nC\n")
	var ids []string
	for _, b := range p.Blocks {
		ids = append(ids, b.ID)
	}
	if p.Title != "" || strings.Join(ids, " ") != "goal goal-2 block" || p.Version != 1 || p.Key != "APP-1" || p.Root != "/w" {
		t.Fatalf("%+v", p)
	}
}
