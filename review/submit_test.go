package review

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

var update = flag.Bool("update", false, "write the golden files in testdata")

// golden compares text with testdata/name. The root and the comment ids are placeholders in the file.
func golden(t *testing.T, name, text string, repl ...string) {
	t.Helper()
	text = strings.NewReplacer(repl...).Replace(text)
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if text != string(want) {
		t.Fatalf("%s:\n%s\nwant:\n%s", name, text, want)
	}
}

func session(id string, state wire.SessionPhase, since time.Time, cwd string) wire.DaemonSession {
	return wire.DaemonSession{ID: id, Runtime: wire.RuntimeOrca, Harness: "claude", Terminal: "t-" + id, Cwd: cwd, State: state,
		Reason: wire.ReasonTurnEnd, StateSince: since, Model: "opus", Profile: "work"}
}

func TestSubmitIdle(t *testing.T) {
	w, r, g := codeRig(t)
	ctx := context.Background()
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	c1, _ := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "Rename N to New."})
	c2, _ := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "lib.go", Line: 3, Side: "old"}, Text: "Why does A go?"})
	round, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges, Summary: "Two small things."})
	if err != nil {
		t.Fatal(err)
	}
	if round.N != 1 || round.Count != 2 || round.Delivery != wire.DeliverySent || round.Session != "s1" || round.DeliveredAt == nil || round.Error != "" {
		t.Fatalf("%+v", round)
	}
	sent := r.sender.all()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "s1: ") {
		t.Fatal(sent)
	}
	golden(t, "code_round1.txt", strings.TrimPrefix(sent[0], "s1: "), w.root, "<root>", c1.ID, "c_1", c2.ID, "c_2")

	d, _ := r.Get(ctx, "APP-1", "")
	if d.Comments[0].State != wire.CommentSubmitted || d.Comments[0].Round != 1 || len(d.Rounds) != 1 {
		t.Fatalf("%+v", d)
	}
	// The session got the prompt: it counts as working until the book shows a newer state.
	if s := list(t, r)[0]; s.State != wire.ReviewWaiting {
		t.Fatal(s.State)
	}
	// Nothing to send and no summary: refused.
	if _, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges}); !kindIs(err, Invalid) {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: "maybe"}); !kindIs(err, Invalid) {
		t.Fatal(err)
	}
}

func TestSubmitWorkingQueuesTwoBatches(t *testing.T) {
	_, r, g := codeRig(t)
	ctx := context.Background()
	t0 := r.clock.now()
	r.book.put(session("s1", wire.PhaseWorking, t0, filepath.Join(g, "app")))
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "One."})
	r1, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if err != nil || r1.Delivery != wire.DeliveryQueued || r1.DeliveredAt != nil {
		t.Fatalf("%v %+v", err, r1)
	}
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 5}, Text: "Two."})
	r2, _ := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if r2.N != 2 || r2.Delivery != wire.DeliveryQueued {
		t.Fatalf("%+v", r2)
	}
	r.flush(ctx)
	if len(r.sender.all()) != 0 {
		t.Fatal(r.sender.all())
	}

	// The turn ends: the first batch goes, the second waits behind it.
	r.clock.add(time.Minute)
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	r.flush(ctx)
	sent := r.sender.all()
	if len(sent) != 1 || !strings.Contains(sent[0], "round 1:") || !strings.Contains(sent[0], "One.") {
		t.Fatal(sent)
	}
	d, _ := r.Get(ctx, "APP-1", "")
	if d.Rounds[0].Delivery != wire.DeliveryQueued || d.Rounds[0].DeliveredAt == nil || d.Rounds[1].DeliveredAt != nil {
		t.Fatalf("%+v", d.Rounds)
	}
	// The book still shows the old turn end: no second prompt yet.
	r.flush(ctx)
	if len(r.sender.all()) != 1 {
		t.Fatal(r.sender.all())
	}
	r.clock.add(time.Minute)
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	r.flush(ctx)
	sent = r.sender.all()
	if len(sent) != 2 || !strings.Contains(sent[1], "round 2:") || !strings.Contains(sent[1], "Two.") {
		t.Fatal(sent)
	}
}

func TestSubmitEndedStartsSession(t *testing.T) {
	w, r, g := codeRig(t)
	ctx := context.Background()
	// The session of the group was seen once, then it went away.
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	list(t, r)
	r.book.mu.Lock()
	r.book.sessions = nil
	r.book.mu.Unlock()

	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "One."})
	round, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if err != nil || round.Delivery != wire.DeliveryNewSession || round.Session != "new1" || round.DeliveredAt == nil {
		t.Fatalf("%v %+v", err, round)
	}
	req := r.launch.reqs[0]
	if r.launch.rts[0] != wire.RuntimeOrca || req.Repo != g || !req.InPlace || req.Key != "work:APP-1" || req.Name != "APP-1" ||
		req.Agent != "claude" || req.Model != "opus" || req.Profile != "work" || !strings.Contains(req.Prompt, "Work in the group folder "+g) {
		t.Fatalf("%s %+v", r.launch.rts[0], req)
	}
	_ = w

	// The launch of the submit wins over the agent of the last session.
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 5}, Text: "Two."})
	round, _ = r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges, Launch: &wire.AgentLaunch{CLI: "codex", Profile: "p"}})
	if round.Session != "new2" || r.launch.reqs[1].Agent != "codex" {
		t.Fatalf("%+v %+v", round, r.launch.reqs[1])
	}
}

func TestSubmitFailures(t *testing.T) {
	_, r, g := codeRig(t)
	ctx := context.Background()
	// No session and no agent: the round keeps the error, the call works.
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 3}, Text: "One."})
	round, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if err != nil || round.Delivery != wire.DeliveryNewSession || round.Error == "" || round.DeliveredAt != nil {
		t.Fatalf("%v %+v", err, round)
	}
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	r.sender.err = errors.New("the terminal is gone")
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 5}, Text: "Two."})
	round, err = r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if err != nil || round.Delivery != wire.DeliverySent || round.Error != "the terminal is gone" {
		t.Fatalf("%v %+v", err, round)
	}
	r.sender.err, r.sender.no = nil, true
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Repo: "app", Path: "n.go", Line: 1}, Text: "Three."})
	round, _ = r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictRequestChanges})
	if !strings.Contains(round.Error, "cannot type") {
		t.Fatalf("%+v", round)
	}
	// Approve sends nothing.
	round, _ = r.Submit(ctx, "APP-1", wire.SubmitReview{Verdict: wire.VerdictApprove})
	if round.Delivery != wire.DeliveryNone || round.Count != 0 {
		t.Fatalf("%+v", round)
	}
}

func TestSubmitPlan(t *testing.T) {
	r := newRig(t)
	root := t.TempDir()
	ctx := context.Background()
	r.PutPlan("APP-1", wire.PlanDoc{Root: root, Blocks: []wire.PlanBlock{{ID: "goal", Heading: "Goal", Markdown: "x"}, {ID: "steps", Heading: "Steps", Markdown: "y"}}})
	r.book.put(wire.DaemonSession{ID: "p1", Runtime: wire.RuntimeOversire, Harness: "claude", Key: "plan:APP-1", Cwd: root,
		State: wire.PhaseWaiting, Reason: wire.ReasonTurnEnd, StateSince: r.clock.now()})
	c, _ := r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Block: "steps"}, Text: "Split step 2."})
	round, err := r.Submit(ctx, "APP-1", wire.SubmitReview{Kind: wire.ReviewPlan, Verdict: wire.VerdictRequestChanges, Summary: "Close."})
	if err != nil || round.Delivery != wire.DeliverySent || round.Session != "p1" {
		t.Fatalf("%v %+v", err, round)
	}
	golden(t, "plan_round1.txt", strings.TrimPrefix(r.sender.all()[0], "p1: "), root, "<root>", c.ID, "c_1")

	// The plan session ended: a new one starts in the folder of the plan with the key plan:APP-1.
	r.book.put(wire.DaemonSession{ID: "p1", Runtime: wire.RuntimeOversire, Harness: "claude", Key: "plan:APP-1", State: wire.PhaseDone, StateSince: r.clock.now()})
	r.AddComment(ctx, "APP-1", wire.CommentDraft{Anchor: wire.Anchor{Block: "goal"}, Text: "Say why."})
	round, _ = r.Submit(ctx, "APP-1", wire.SubmitReview{Kind: wire.ReviewPlan, Verdict: wire.VerdictRequestChanges})
	if round.Delivery != wire.DeliveryNewSession || r.launch.reqs[0].Key != "plan:APP-1" || r.launch.reqs[0].Repo != root || !r.launch.reqs[0].InPlace {
		t.Fatalf("%+v %+v", round, r.launch.reqs)
	}
}

func TestFixPrompt(t *testing.T) {
	w := newWS(t)
	app := w.repo("app")
	g := w.group("app-1", "app")
	w.commit(filepath.Join(g, "app"), "Mine", map[string]string{"lib.go": "package lib\n\nfunc Mine() {}\n"})
	w.commit(app, "Theirs", map[string]string{"lib.go": "package lib\n\nfunc Theirs() {}\n"})
	r := newRig(t, w.root)
	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	round, err := r.Fix(context.Background(), "APP-1", nil)
	if err != nil || round.Delivery != wire.DeliverySent || round.Count != 0 {
		t.Fatalf("%v %+v", err, round)
	}
	golden(t, "fix.txt", strings.TrimPrefix(r.sender.all()[0], "s1: "), w.root, "<root>")
}
