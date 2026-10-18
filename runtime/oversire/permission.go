package oversire

import (
	"context"
	"fmt"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/runner/wire"
)

// expiredReason goes to the agent with a deny at the expiry time.
const expiredReason = "No answer came before the expiry time."

type openPermission struct {
	prompt wire.PermissionPrompt
	timer  *time.Timer
}

// ask shows a permission request. With no answer before the expiry time, the runner denies it.
// Call it with s.mu held.
func (s *session) ask(p *agentwire.Permission) {
	tool := p.Tool
	if tool == "" {
		tool = string(p.Kind)
	}
	text := p.Question
	if text == "" {
		text = p.Input
	}
	timeout := s.rt.opts.PermissionTimeout
	op := &openPermission{prompt: wire.PermissionPrompt{ID: p.ID, Tool: tool, Text: clip(text, maxText), ExpiresAt: s.rt.now().Add(timeout).UTC()}}
	op.timer = time.AfterFunc(timeout, func() { _ = s.answer(context.Background(), p.ID, false, expiredReason) })
	s.permissions = append(s.permissions, op)
	s.log.add("? " + tool + ": " + op.prompt.Text)
	s.showPermission()
}

// answer gives the decision for the open prompt id to the agent.
func (s *session) answer(ctx context.Context, id string, allow bool, reason string) error {
	s.mu.Lock()
	i := -1
	for n, p := range s.permissions {
		if p.prompt.ID == id {
			i = n
		}
	}
	if i < 0 {
		open := len(s.permissions)
		s.mu.Unlock()
		if open == 0 {
			return fmt.Errorf("The session %s has no open permission prompt.", s.id)
		}
		return fmt.Errorf("The session %s has no open permission prompt %s.", s.id, id)
	}
	s.permissions[i].timer.Stop()
	s.permissions = append(s.permissions[:i], s.permissions[i+1:]...)
	aw := s.aw
	word := "deny"
	if allow {
		word = "allow"
	}
	if reason != "" {
		word += " (" + reason + ")"
	}
	s.log.add("= " + id + " " + word)
	s.showPermission()
	s.mu.Unlock()
	return aw.AnswerPermission(ctx, id, agentwire.Decision{Allow: allow, Message: reason})
}

// showPermission shows the first open prompt, else goes back to working. Call it with s.mu held.
func (s *session) showPermission() {
	if len(s.permissions) == 0 {
		s.info.Permission = nil
		s.change(wire.SessionWorking)
		return
	}
	p := s.permissions[0].prompt
	s.info.Permission = &p
	s.change(wire.SessionWaitingPermission)
}
