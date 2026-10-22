// Package ask sends a question to an agent session and waits for the reply of its turn (POST /v1/sessions/{id}/ask).
package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	// ScreenLines is the number of screen lines in a screenTail.
	ScreenLines = 40
	maxBody     = 64 << 10
)

// Book is the session book (sessions.Book).
type Book interface {
	Get(id string) (wire.DaemonSession, bool)
	Watch() (<-chan string, func())
}

// Screen reads and types into the Orca terminal of a session (orca.Screen).
type Screen interface {
	Read(ctx context.Context, terminal string, lines int) ([]string, error)
	Send(ctx context.Context, terminal, text string, enter bool) error
}

// Runtimes runs a sessionSend or sessionRead order in its runtime (runtime.Registry).
type Runtimes interface {
	Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error)
}

// Asker sends the text of an ask to a session and waits for the end of the turn.
// The turn ends when the book shows the session out of the working state after the send.
type Asker struct {
	book     Book
	screen   Screen
	runtimes Runtimes
	clock    func() time.Time

	// Poll is the check of the book between two notifications: the watch channel can drop ids.
	Poll time.Duration
	// Settle is the wait for a new last message after the turn end. A hook can give it after the state.
	Settle time.Duration
	// unit is one second of timeoutSec. Tests make it short.
	unit time.Duration
}

// NewAsker needs a book. screen and runtimes can be nil: then the sessions of that kind are unsupported.
func NewAsker(book Book, screen Screen, runtimes Runtimes, clock func() time.Time) *Asker {
	if clock == nil {
		clock = time.Now
	}
	return &Asker{book: book, screen: screen, runtimes: runtimes, clock: clock,
		Poll: time.Second, Settle: 2 * time.Second, unit: time.Second}
}

func (k *Asker) Routes() []server.Route {
	return []server.Route{{Pattern: "POST /v1/sessions/{id}/ask", Access: server.Member, Handle: k.serve}}
}

func (k *Asker) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	var a wire.SessionAsk
	if err == nil {
		err = wire.Decode(body, &a)
	}
	if err == nil {
		err = a.Check()
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, wire.SessionAskReply{Error: wire.AskBadRequest, Message: err.Error()})
		return
	}
	code, reply := k.Ask(r.Context(), r.PathValue("id"), a)
	writeJSON(w, code, reply)
}

// Ask sends a.Prompt() to the session id and waits for the turn end. code is the HTTP status of the reply.
func (k *Asker) Ask(ctx context.Context, id string, a wire.SessionAsk) (code int, reply wire.SessionAskReply) {
	deadline := k.clock().Add(a.Timeout() / time.Second * k.unit)
	before, ok := k.book.Get(id)
	if !ok {
		return http.StatusNotFound, wire.SessionAskReply{Error: wire.AskNoSession, Message: "The daemon has no session " + id + "."}
	}
	if !k.canSend(before) {
		return http.StatusConflict, wire.SessionAskReply{Error: wire.AskUnsupported,
			Message: fmt.Sprintf("OverSire cannot type into the %s session %s.", before.Runtime, id)}
	}
	if before.State == wire.PhaseWorking && !steers(before) {
		return http.StatusConflict, wire.SessionAskReply{Error: wire.AskBusy, ScreenTail: k.screenTail(ctx, before),
			Message: "The agent is in a turn. Ask again when the turn ends."}
	}

	changes, stop := k.book.Watch()
	defer stop()
	sentAt := k.clock()
	if err := k.send(ctx, before, a.Prompt()); err != nil {
		return http.StatusBadGateway, wire.SessionAskReply{Error: wire.AskSendFailed, Message: err.Error()}
	}

	t := turn{before: before, sentAt: sentAt}
	timer := time.NewTimer(max(deadline.Sub(k.clock()), 0))
	defer timer.Stop()
	poll := time.NewTicker(k.Poll)
	defer poll.Stop()
	for {
		s, ok := k.book.Get(id)
		if !ok {
			return http.StatusGone, wire.SessionAskReply{Error: wire.AskGone, Message: "The session " + id + " went away."}
		}
		if t.ended(s) {
			if s.LastMessage == before.LastMessage {
				s = k.settle(ctx, s, deadline)
			}
			at := s.StateSince
			if at.Before(sentAt) {
				at = k.clock()
			}
			return http.StatusOK, wire.SessionAskReply{Reply: clip(s.LastMessage, wire.MaxAskReply), TurnEndedAt: &at,
				ScreenTail: k.screenTail(ctx, s)}
		}
		select {
		case <-ctx.Done():
			return http.StatusGatewayTimeout, wire.SessionAskReply{Error: wire.AskTimeout, Message: "The caller went away."}
		case <-timer.C:
			return http.StatusGatewayTimeout, wire.SessionAskReply{Error: wire.AskTimeout, ScreenTail: k.screenTail(ctx, s),
				Message: fmt.Sprintf("The agent did not end its turn in %d s.", a.Timeout()/time.Second)}
		case <-changes:
		case <-poll.C:
		}
	}
}

// turn tells the end of the turn that the ask started.
type turn struct {
	before  wire.DaemonSession
	sentAt  time.Time
	working bool
}

// ended is true when the session left the working state after the send. A state that changed after the send
// with a new last message also counts: a short turn can start and end between two checks.
func (t *turn) ended(s wire.DaemonSession) bool {
	if s.State == wire.PhaseWorking || s.State == wire.PhaseStarting {
		t.working = true
		return false
	}
	return t.working || !s.StateSince.Before(t.sentAt) && s.LastMessage != t.before.LastMessage
}

// settle waits up to Settle, and not past the deadline, for a new last message: the turn end can come before it.
func (k *Asker) settle(ctx context.Context, s wire.DaemonSession, deadline time.Time) wire.DaemonSession {
	end := k.clock().Add(k.Settle)
	if deadline.Before(end) {
		end = deadline
	}
	before := s.LastMessage
	for s.LastMessage == before && k.clock().Before(end) {
		select {
		case <-ctx.Done():
			return s
		case <-time.After(50 * time.Millisecond):
		}
		if n, ok := k.book.Get(s.ID); ok {
			s = n
		}
	}
	return s
}

// steers is true when the runtime takes a message during a turn. The oversire runtime adds it to the turn.
func steers(s wire.DaemonSession) bool { return s.Runtime == wire.RuntimeOversire }

func (k *Asker) canSend(s wire.DaemonSession) bool {
	switch s.Runtime {
	case wire.RuntimeOversire:
		return k.runtimes != nil
	case wire.RuntimeOrca:
		return k.screen != nil && s.Terminal != ""
	}
	return false
}

// send types the text into the Orca terminal, or gives it to the oversire runtime as a sessionSend order.
func (k *Asker) send(ctx context.Context, s wire.DaemonSession, text string) error {
	if s.Runtime == wire.RuntimeOrca {
		return k.screen.Send(ctx, s.Terminal, text, true)
	}
	_, err := k.runtimes.Do(ctx, k.order(s, wire.SendPayload{Session: ref(s), Text: text}))
	return err
}

// screenTail gives the last screen lines of an Orca terminal, or the transcript tail of an oversire session. A failed read gives "".
func (k *Asker) screenTail(ctx context.Context, s wire.DaemonSession) string {
	var lines []string
	switch {
	case s.Runtime == wire.RuntimeOrca && k.screen != nil && s.Terminal != "":
		lines, _ = k.screen.Read(ctx, s.Terminal, ScreenLines)
	case s.Runtime == wire.RuntimeOversire && k.runtimes != nil:
		if res, err := k.runtimes.Do(ctx, k.order(s, wire.ReadPayload{Session: ref(s), Lines: ScreenLines})); err == nil && res != nil {
			lines = res.Lines
		}
	}
	return tail(strings.TrimRight(strings.Join(lines, "\n"), "\n \t"), wire.MaxAskScreenTail)
}

func (k *Asker) order(s wire.DaemonSession, p wire.Payload) wire.WorkOrder {
	now := k.clock()
	return wire.WorkOrder{ID: "ask-" + s.ID, Runtime: s.Runtime, CreatedAt: now, State: wire.StateQueued, Payload: p}
}

func ref(s wire.DaemonSession) wire.SessionRef {
	return wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: s.Runtime}, ID: s.Workspace}, ID: s.ID}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
