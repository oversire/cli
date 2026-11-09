package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

// The roles of the actor of a Change. The event writer turns a role into an actor.
const (
	ActorOverseer = "overseer" // the Claude session that oversees the worker
	ActorSystem   = "system"   // the daemon
	ActorWorker   = "worker"   // the agent of the worker
)

const (
	maxDoneNote = 300 // characters of the note of a done report
	orderTTL    = 2 * time.Minute
)

// Change is a lifecycle step of a worker. Kind is the event kind. Actor is a role. Session is the session of the actor.
type Change struct {
	Kind    string
	Record  Record
	Actor   string
	Session string
	At      time.Time
	Payload map[string]any
}

// StartRequest is the body of POST /v1/workers.
type StartRequest struct {
	Key      string        `json:"key"`
	Harness  string        `json:"harness"`
	Model    string        `json:"model,omitempty"`
	Dir      string        `json:"dir"`
	Plan     string        `json:"plan,omitempty"`
	Overseer string        `json:"overseer"`
	Budget   time.Duration `json:"budget,omitempty"`
}

// Started is the answer of POST /v1/workers.
type Started struct {
	Record Record `json:"record"`
	Reused bool   `json:"reused"`
}

// Answer is the body of POST /v1/workers/{id}/answer. It answers a permission or a question.
type Answer struct {
	Permission string `json:"permission,omitempty"`
	Allow      *bool  `json:"allow,omitempty"`
	Question   string `json:"question,omitempty"`
	Text       string `json:"text,omitempty"`
}

// order makes a new order id for each call, because a runner gives the old result for an id that it saw.
func (e *Engine) order(op string) runtime.Order {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return runtime.Order{ID: "worker-" + op + "-" + hex.EncodeToString(b), ExpiresAt: e.now().Add(orderTTL)}
}

// mapErr gives the refusals of the runtime as the kinds of this package.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrNotFound):
		return refuse(NotFound, "%v", err)
	case errors.Is(err, runtime.ErrConflict), errors.Is(err, runtime.ErrUnsupported):
		return refuse(Conflict, "%v", err)
	}
	return err
}

func refOf(s wire.DaemonSession) wire.SessionRef {
	return wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: s.Runtime, ID: "local"}, ID: s.Workspace}, ID: s.ID}
}

// update loads the record of ses, applies fn, saves it and gives the copy.
func (e *Engine) update(ses wire.DaemonSession, fn func(*Record)) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, err := e.store.Load(ses.ID)
	if err != nil {
		return Record{}, err
	}
	rec.Session = ses.ID
	if ses.Key != "" {
		rec.Key = ses.Key
	}
	if rec.Overseer == "" {
		rec.Overseer = ses.Overseer
	}
	fn(&rec)
	rec.UpdatedAt = e.now().UTC()
	return rec, e.store.Save(rec)
}

func (e *Engine) record(ses wire.DaemonSession) (Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.Load(ses.ID)
}

// Get gives the record of a worker.
func (e *Engine) Get(ref string) (Record, error) {
	ses, err := e.session(ref)
	if err != nil {
		return Record{}, err
	}
	return e.record(ses)
}

// Start starts a worker session for the item, or gives the live one. The bool is true for the live one.
func (e *Engine) Start(ctx context.Context, r StartRequest) (Record, bool, error) {
	key := strings.ToUpper(strings.TrimSpace(r.Key))
	switch {
	case key == "":
		return Record{}, false, refuse(Invalid, "Give the item key.")
	case r.Harness == "":
		return Record{}, false, refuse(Invalid, "Give the harness.")
	case r.Overseer == "":
		return Record{}, false, refuse(Invalid, "Give the session of the overseer.")
	case !filepath.IsAbs(r.Dir):
		return Record{}, false, refuse(Invalid, "The folder is an absolute path.")
	case r.Budget < 0:
		return Record{}, false, refuse(Invalid, "The budget is 0 or more.")
	}
	dir := filepath.Clean(r.Dir)
	started, err := e.orders.Launch(ctx, wire.RuntimeOversire, e.order("start"), wire.LaunchRequest{
		Key: "work:" + key, Item: &wire.ItemRef{Key: key}, InPlace: true, Repo: dir,
		Agent: r.Harness, Model: r.Model, Prompt: Prompt{Key: key, Plan: r.Plan}.String(), Overseer: r.Overseer,
	})
	if err != nil {
		return Record{}, false, mapErr(err)
	}
	now := e.now().UTC()
	e.mu.Lock()
	rec, err := e.store.Load(started.Session.ID)
	if err == nil {
		if rec.Session == "" {
			rec = Record{Session: started.Session.ID, Key: "work:" + key, Harness: r.Harness, Model: r.Model, Dir: dir, Plan: r.Plan,
				State: StateWorking, Budget: cmpBudget(r.Budget), StartedAt: now}
		}
		rec.Overseer, rec.LastAction, rec.UpdatedAt = r.Overseer, now, now
		err = e.store.Save(rec)
	}
	e.mu.Unlock()
	if err != nil {
		return Record{}, false, err
	}
	e.notify(Change{Kind: wire.KindWorkerStarted, Record: rec, Actor: ActorOverseer, Session: r.Overseer, At: now,
		Payload: map[string]any{"session": rec.Session, "harness": rec.Harness, "model": rec.Model, "reused": started.Reused}})
	return rec, started.Reused, nil
}

func cmpBudget(b time.Duration) time.Duration {
	if b == 0 {
		return DefaultBudget
	}
	return b
}

// Done records the report of the worker: done waits for a check, blocked waits for help.
func (e *Engine) Done(ctx context.Context, ref string, verdict, note string) (Record, error) {
	var state State
	switch verdict {
	case "done":
		state = StateAwaitingCheck
	case "blocked":
		state = StateBlocked
	default:
		return Record{}, refuse(Invalid, "The verdict is done or blocked.")
	}
	ses, err := e.session(ref)
	if err != nil {
		return Record{}, err
	}
	note = clip(strings.TrimSpace(note), maxDoneNote)
	rec, err := e.update(ses, func(r *Record) { r.State, r.Verdict, r.Note = state, verdict, note })
	if err != nil {
		return Record{}, err
	}
	e.notify(Change{Kind: wire.KindWorkerDone, Record: rec, Actor: ActorWorker, Session: ses.ID, At: e.now().UTC(),
		Payload: map[string]any{"verdict": verdict, "note": note}})
	return rec, nil
}

// Send gives text to the worker as a turn of the overseer. The worker is working again.
func (e *Engine) Send(ctx context.Context, ref, text string) error {
	if strings.TrimSpace(text) == "" {
		return refuse(Invalid, "Give the text.")
	}
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	if err := e.orders.Send(ctx, ses.Runtime, e.order("send"), wire.SendPayload{Session: refOf(ses), Text: text, Origin: wire.OriginOverseer}); err != nil {
		return mapErr(err)
	}
	return e.acted(ses, func(r *Record) { r.State = StateWorking }, wire.KindWorkerNudged, map[string]any{"how": "send"})
}

// Answer answers the open permission or question of the worker.
func (e *Engine) Answer(ctx context.Context, ref string, a Answer) error {
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	p := wire.SendPayload{Session: refOf(ses), Origin: wire.OriginOverseer}
	payload := map[string]any{}
	switch {
	case a.Permission != "" && a.Question == "":
		if a.Allow == nil {
			return refuse(Invalid, "Give allow or deny.")
		}
		p.Permission = &wire.PermissionAnswer{ID: a.Permission, Allow: *a.Allow}
		payload["request"], payload["allow"] = "permission", *a.Allow
	case a.Question != "" && a.Permission == "":
		if strings.TrimSpace(a.Text) == "" {
			return refuse(Invalid, "Give the text of the answer.")
		}
		if strings.HasPrefix(a.Question, escalationPrefix) {
			return refuse(Conflict, "The question %s asks the user. Only the user answers it.", a.Question)
		}
		insp, ok := e.inspect(ctx, ses)
		if !ok || insp.Session.Question == nil || insp.Session.Question.ID != a.Question || len(insp.Session.Question.Items) == 0 {
			return refuse(Conflict, "The worker has no open question %s.", a.Question)
		}
		p.Answer = &wire.QuestionAnswer{ID: a.Question, Items: []wire.QuestionAnswerItem{{ID: insp.Session.Question.Items[0].ID, Choices: []string{}, Other: a.Text}}}
		payload["request"] = "question"
	default:
		return refuse(Invalid, "Answer one permission or one question.")
	}
	if err := e.orders.Send(ctx, ses.Runtime, e.order("answer"), p); err != nil {
		return mapErr(err)
	}
	return e.acted(ses, func(*Record) {}, wire.KindWorkerAnswered, payload)
}

// Interrupt stops the current turn of the worker. The session stays open.
func (e *Engine) Interrupt(ctx context.Context, ref string) error {
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	if err := e.interrupt(ctx, ses); err != nil {
		return err
	}
	return e.acted(ses, func(*Record) {}, wire.KindWorkerNudged, map[string]any{"how": "interrupt"})
}

func (e *Engine) interrupt(ctx context.Context, ses wire.DaemonSession) error {
	ctx, cancel := context.WithCancel(ctx) // ends the live feed of this call
	defer cancel()
	src, err := e.chats.Chat(ctx, ses)
	if errors.Is(err, term.ErrNoView) {
		return refuse(Conflict, "The session %s has no turns.", ses.ID)
	}
	if err != nil {
		return err
	}
	return mapErr(src.Interrupt(ctx))
}

// acted marks an action of the overseer and tells it.
func (e *Engine) acted(ses wire.DaemonSession, fn func(*Record), kind string, payload map[string]any) error {
	now := e.now().UTC()
	rec, err := e.update(ses, func(r *Record) { r.LastAction = now; fn(r) })
	if err != nil {
		return err
	}
	e.notify(Change{Kind: kind, Record: rec, Actor: ActorOverseer, Session: rec.Overseer, At: now, Payload: payload})
	return nil
}

// Stop ends the session of the worker.
func (e *Engine) Stop(ctx context.Context, ref string) error {
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	if err := e.orders.Stop(ctx, ses.Runtime, e.order("stop"), refOf(ses)); err != nil {
		return mapErr(err)
	}
	e.mu.Lock()
	delete(e.posted, ses.ID)
	e.mu.Unlock()
	_, err = e.update(ses, func(r *Record) { r.State, r.Escalation = StateStopped, nil })
	return err
}

// Escalate asks the user for the worker. With a question, the user answers it and the answer goes to the worker.
// With no question, the open ask of the worker goes to the user.
func (e *Engine) Escalate(ctx context.Context, ref, question string, reason EscalationReason) error {
	switch reason {
	case ReasonOverseer, ReasonFallback, ReasonBudget:
	default:
		return refuse(Invalid, "The reason is overseer, fallback or budget.")
	}
	ses, err := e.session(ref)
	if err != nil {
		return err
	}
	rec, err := e.record(ses)
	if err != nil {
		return err
	}
	if rec.Escalation != nil {
		return refuse(Conflict, "The worker %s has an open escalation.", ses.ID)
	}
	return e.escalate(ctx, ses, strings.TrimSpace(question), reason)
}

func (e *Engine) escalate(ctx context.Context, ses wire.DaemonSession, question string, reason EscalationReason) error {
	now := e.now().UTC()
	esc := &Escalation{Question: question, Reason: reason, At: now}
	if question != "" {
		esc.ID = fmt.Sprintf("%s%d", escalationPrefix, now.UnixMilli())
	}
	if err := e.watch.Escalate(ctx, ses, esc.runtimeQuestion()); err != nil {
		return mapErr(err)
	}
	rec, err := e.update(ses, func(r *Record) { r.Escalation = esc })
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.posted[ses.ID] = true
	e.mu.Unlock()
	actor, session := ActorOverseer, rec.Overseer
	if reason != ReasonOverseer {
		actor = ActorSystem
	}
	e.notify(Change{Kind: wire.KindWorkerEscalated, Record: rec, Actor: actor, Session: session, At: now,
		Payload: map[string]any{"reason": string(reason), "question": question}})
	return nil
}

// runtimeQuestion is the question for the runtime, or nil to show the open ask of the worker.
func (x Escalation) runtimeQuestion() *wire.RunnerQuestion {
	if x.ID == "" {
		return nil
	}
	return &wire.RunnerQuestion{ID: x.ID, AskedAt: x.At,
		Items: []wire.RunnerQuestionItem{{ID: "q", Text: x.Question, Kind: wire.QuestionFree, Options: []wire.QuestionOption{}, AllowsOther: true}}}
}
