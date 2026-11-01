package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// Submit makes one round of all pending comments of key. Request changes sends one prompt to the session of the key:
// now when it is idle, at the end of its turn when it works, or in a new session in the same folder when it ended.
// Approve sends nothing. A failed delivery is in the round, not in the error.
func (e *Engine) Submit(ctx context.Context, key string, s wire.SubmitReview) (wire.ReviewRound, error) {
	if s.Verdict != wire.VerdictRequestChanges && s.Verdict != wire.VerdictApprove {
		return wire.ReviewRound{}, refuse(Invalid, "The verdict %q is not requestChanges or approve.", s.Verdict)
	}
	if len([]rune(s.Summary)) > MaxText {
		return wire.ReviewRound{}, refuse(Invalid, "The summary has more than %d characters.", MaxText)
	}
	kind, err := e.kindOf(ctx, key, s.Kind)
	if err != nil {
		return wire.ReviewRound{}, err
	}
	dir, err := e.folder(ctx, kind, key)
	if err != nil {
		return wire.ReviewRound{}, err
	}
	e.send.Lock()
	defer e.send.Unlock()
	e.mu.Lock()
	r := e.rec(kind, key)
	var batch, open []wire.ReviewComment
	for _, c := range r.Comments {
		switch c.State {
		case wire.CommentPending:
			batch = append(batch, c)
		case wire.CommentSubmitted:
			// A round that waits in the queue still goes. Repeat only a round that went, or that failed.
			if rd := e.roundOf(r, c.Round); rd != nil && (rd.DeliveredAt != nil || rd.Error != "") {
				open = append(open, c)
			}
		}
	}
	if s.Verdict == wire.VerdictRequestChanges && len(batch) == 0 && strings.TrimSpace(s.Summary) == "" {
		e.mu.Unlock()
		return wire.ReviewRound{}, refuse(Invalid, "The review has no comments and no summary. Add one first.")
	}
	n := len(r.Rounds) + 1
	for i := range r.Comments {
		if r.Comments[i].State == wire.CommentPending {
			r.Comments[i].State, r.Comments[i].Round = wire.CommentSubmitted, n
		}
	}
	round := wire.ReviewRound{N: n, Verdict: s.Verdict, Summary: strings.TrimSpace(s.Summary), Count: len(batch), SubmittedAt: e.now(), Delivery: wire.DeliveryNone}
	if s.Verdict == wire.VerdictApprove {
		r.Approved = kind == wire.ReviewPlan
	} else {
		round.Delivery = ""
		for i := range batch {
			batch[i].Round = n
		}
		e.setPrompt(r, n, prompt(kind, key, dir, round, batch, open))
	}
	r.Rounds = append(r.Rounds, round)
	e.changed(r, wire.EventReviewChanged)
	e.mu.Unlock()
	rowKind, summary := wire.KindReviewSubmitted, fmt.Sprintf("You asked for changes: %d comments", len(batch))
	if s.Verdict == wire.VerdictApprove {
		rowKind, summary = wire.KindReviewApproved, "You approved the "+string(kind)
	}
	if round.Summary != "" {
		summary += ". " + round.Summary
	}
	e.event(key, fmt.Sprintf("review:%s:%s:%d", kind, key, n), rowKind, wire.ActorHuman, summary,
		map[string]any{"kind": kind, "round": n, "comments": len(batch)})
	if s.Verdict == wire.VerdictRequestChanges {
		e.deliver(ctx, kind, key, n, s.Launch)
	}
	return e.round(kind, key, n), nil
}

// Fix sends the fixed prompt of a conflict to the session of the code review of key, with the delivery rules of Submit.
// It is a round with no comments. The pending comments stay in the batch.
func (e *Engine) Fix(ctx context.Context, key string, launch *wire.AgentLaunch) (wire.ReviewRound, error) {
	if !validKey(key) {
		return wire.ReviewRound{}, refuse(Invalid, "The key %q is not valid.", key)
	}
	g, ok := findGroup(e.allRoots(ctx), key)
	if !ok {
		return wire.ReviewRound{}, refuse(NotFound, "There is no group .wt/%s in the roots of this daemon.", strings.ToLower(key))
	}
	var repos []wire.ReviewRepo
	for _, name := range e.d.Order.Sort(g.Repos) {
		r, _, err := e.d.Git.repo(ctx, g, name)
		if err != nil {
			return wire.ReviewRound{}, err
		}
		if r.Ahead > 0 || r.Dirty {
			repos = append(repos, r)
		}
	}
	e.send.Lock()
	defer e.send.Unlock()
	e.mu.Lock()
	r := e.rec(wire.ReviewCode, key)
	n := len(r.Rounds) + 1
	r.Rounds = append(r.Rounds, wire.ReviewRound{N: n, Verdict: wire.VerdictRequestChanges, Summary: "Fix the merge conflicts.", SubmittedAt: e.now()})
	e.setPrompt(r, n, fixPrompt(key, g.Dir(), repos))
	e.changed(r, wire.EventReviewChanged)
	e.mu.Unlock()
	e.deliver(ctx, wire.ReviewCode, key, n, launch)
	return e.round(wire.ReviewCode, key, n), nil
}

// Run sends the queued rounds: on each change in the book and each 30 s.
func (e *Engine) Run(ctx context.Context) error {
	ids, cancel := e.d.Sessions.Watch()
	defer cancel()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		e.flush(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ids:
		case <-t.C:
		}
	}
}

// flush sends the oldest queued round of each review whose session is idle, or starts a new session when it ended.
func (e *Engine) flush(ctx context.Context) {
	type due struct {
		kind wire.ReviewKind
		key  string
		n    int
	}
	var all []due
	e.mu.Lock()
	for _, r := range e.recs {
		if n := waiting(r, len(r.Rounds)+1); n > 0 {
			all = append(all, due{r.Kind, r.Key, n})
		}
	}
	e.mu.Unlock()
	for _, d := range all {
		e.send.Lock()
		e.deliver(ctx, d.kind, d.key, d.n, nil)
		e.send.Unlock()
	}
}

// waiting gives the oldest round before n that waits for the end of a turn, else 0.
func waiting(r *record, n int) int {
	for _, x := range r.Rounds {
		if x.N < n && x.Delivery == wire.DeliveryQueued && x.DeliveredAt == nil && x.Error == "" {
			return x.N
		}
	}
	return 0
}

// deliver sends the prompt of round n, keeps it queued, or starts a new session with it. Call it with e.send held.
func (e *Engine) deliver(ctx context.Context, kind wire.ReviewKind, key string, n int, launch *wire.AgentLaunch) {
	e.mu.Lock()
	r := e.rec(kind, key)
	rd := e.roundOf(r, n)
	if rd == nil || rd.DeliveredAt != nil || rd.Error != "" {
		e.mu.Unlock()
		return
	}
	if waiting(r, n) > 0 {
		e.mark(r, n, func(x *wire.ReviewRound) { x.Delivery = wire.DeliveryQueued })
		e.mu.Unlock()
		return
	}
	text := r.Prompts[n]
	e.mu.Unlock()

	dir, err := e.folder(ctx, kind, key)
	if err != nil {
		e.fail(kind, key, n, wire.DeliveryNewSession, err)
		return
	}
	ses, rs := e.sessionOf(kind, key, dir)
	if ses.ID != "" {
		e.mu.Lock()
		if r := e.rec(kind, key); e.remember(r, ses) {
			e.save(r)
		}
		e.mu.Unlock()
	}
	switch {
	case rs.State == wire.ReviewSessionWorking:
		e.mu.Lock()
		e.mark(e.rec(kind, key), n, func(x *wire.ReviewRound) { x.Delivery = wire.DeliveryQueued })
		e.mu.Unlock()
	case rs.State == wire.ReviewSessionIdle:
		mode := wire.DeliverySent
		e.mu.Lock()
		if x := e.roundOf(e.rec(kind, key), n); x != nil && x.Delivery == wire.DeliveryQueued {
			mode = wire.DeliveryQueued // a queued round keeps its delivery and gets deliveredAt
		}
		e.mu.Unlock()
		if !rs.CanSend {
			e.fail(kind, key, n, mode, fmt.Errorf("OverSire cannot type into the %s session %s.", ses.Runtime, ses.ID))
			return
		}
		if err := e.d.Sender.Deliver(ctx, ses, text); err != nil {
			e.fail(kind, key, n, mode, err)
			return
		}
		e.delivered(kind, key, n, mode, ses.ID)
	default:
		id, err := e.launch(ctx, kind, key, dir, n, text, launch)
		if err != nil {
			e.fail(kind, key, n, wire.DeliveryNewSession, err)
			return
		}
		e.delivered(kind, key, n, wire.DeliveryNewSession, id)
	}
}

// launch starts a new session in dir with the prompt: the agent of launch, else the agent of the last session of the key.
func (e *Engine) launch(ctx context.Context, kind wire.ReviewKind, key, dir string, n int, text string, launch *wire.AgentLaunch) (string, error) {
	e.mu.Lock()
	r := e.rec(kind, key)
	rt := ""
	var a *wire.AgentLaunch
	if r.Agent != nil {
		rt, a = r.Agent.Runtime, &r.Agent.Launch
	}
	if launch != nil {
		a = launch
	}
	var l wire.AgentLaunch
	if a != nil {
		l = *a
	}
	e.mu.Unlock()
	if l.CLI == "" {
		return "", errors.New("The session of " + key + " ended, and no agent is known for a new session. Give the agent in the submit.")
	}
	if e.d.Launcher == nil || e.d.Picker == nil {
		return "", errors.New("This daemon cannot start a session.")
	}
	rt, err := e.d.Picker.Pick(ctx, rt)
	if err != nil {
		return "", err
	}
	start := "work:" + key
	if kind == wire.ReviewPlan {
		start = "plan:" + key
	}
	req := wire.LaunchRequest{Repo: dir, InPlace: true, Name: key, Agent: l.CLI, Command: l.Command, Model: l.Model, Effort: l.Effort, Profile: l.Profile,
		Title: fmt.Sprintf("%s · review %d", key, n), Key: start, Prompt: text}
	o := runtime.Order{ID: "review-" + strings.ToLower(key) + "-" + randomHex(4), ExpiresAt: e.d.Clock().Add(launchTTL)}
	st, err := e.d.Launcher.Launch(ctx, rt, o, req)
	if err != nil {
		return "", err
	}
	sid := st.PaneKey
	if sid == "" {
		sid = st.Session.ID
	}
	e.mu.Lock()
	r = e.rec(kind, key)
	r.Agent, r.Session = &agentMemo{Runtime: rt, Launch: l}, sid
	e.mu.Unlock()
	return sid, nil
}

func (e *Engine) delivered(kind wire.ReviewKind, key string, n int, mode wire.Delivery, session string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.sent[id(kind, key)] = e.d.Clock()
	e.mark(e.rec(kind, key), n, func(x *wire.ReviewRound) { x.Delivery, x.Session, x.DeliveredAt = mode, session, &now })
}

func (e *Engine) fail(kind wire.ReviewKind, key string, n int, mode wire.Delivery, err error) {
	e.Log.Warn("review delivery", "key", key, "kind", kind, "round", n, "err", err)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mark(e.rec(kind, key), n, func(x *wire.ReviewRound) { x.Delivery, x.Error = mode, err.Error() })
}

// mark changes round n of r and tells the change. Call it with e.mu held.
func (e *Engine) mark(r *record, n int, f func(*wire.ReviewRound)) {
	if x := e.roundOf(r, n); x != nil {
		f(x)
		e.changed(r, wire.EventReviewChanged)
	}
}

func (e *Engine) roundOf(r *record, n int) *wire.ReviewRound {
	for i := range r.Rounds {
		if r.Rounds[i].N == n {
			return &r.Rounds[i]
		}
	}
	return nil
}

// round gives a copy of round n.
func (e *Engine) round(kind wire.ReviewKind, key string, n int) wire.ReviewRound {
	e.mu.Lock()
	defer e.mu.Unlock()
	if x := e.roundOf(e.rec(kind, key), n); x != nil {
		return *x
	}
	return wire.ReviewRound{}
}

// setPrompt keeps the prompt of round n. Call it with e.mu held.
func (e *Engine) setPrompt(r *record, n int, text string) {
	if r.Prompts == nil {
		r.Prompts = map[int]string{}
	}
	r.Prompts[n] = text
}

// folder is the place of the sessions of key: the group folder of a code review, the folder of a plan.
func (e *Engine) folder(ctx context.Context, kind wire.ReviewKind, key string) (string, error) {
	if kind == wire.ReviewPlan {
		e.mu.Lock()
		defer e.mu.Unlock()
		if r := e.recs[id(kind, key)]; r != nil && r.Root != "" {
			return r.Root, nil
		}
		return "", refuse(NotFound, "%s has no plan.", key)
	}
	g, ok := findGroup(e.allRoots(ctx), key)
	if !ok {
		return "", refuse(NotFound, "There is no group .wt/%s in the roots of this daemon.", strings.ToLower(key))
	}
	return g.Dir(), nil
}
