//go:build !windows

package oversire

import (
	"context"
	"fmt"
	"slices"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/wire"
)

// expiredReason goes to the agent with a deny at the expiry time.
const expiredReason = "No answer came before the expiry time."

type openPermission struct {
	prompt wire.PermissionPrompt
	timer  *time.Timer
}

// ask shows a permission request. With no answer before the expiry time, the runner denies it.
// The expiry time starts at the request, also after a restart. Call it with s.mu held.
func (s *session) ask(p *agentwire.Permission, at time.Time) {
	tool := p.Tool
	if tool == "" {
		tool = string(p.Kind)
	}
	text := p.Question
	if text == "" {
		text = p.Input
	}
	op := &openPermission{prompt: wire.PermissionPrompt{ID: p.ID, Tool: tool, Text: clip(text, maxText), ExpiresAt: at.Add(s.rt.opts.PermissionTimeout).UTC()}}
	s.permissions = append(s.permissions, op)
	s.log.add("? " + tool + ": " + op.prompt.Text)
	if s.live {
		s.arm(op)
	}
	s.show()
}

// arm starts the expiry timer of p. Call it with s.mu held.
func (s *session) arm(p *openPermission) {
	if p.timer != nil {
		return
	}
	id := p.prompt.ID
	p.timer = time.AfterFunc(max(p.prompt.ExpiresAt.Sub(s.rt.now()), 0), func() {
		_ = s.answer(context.Background(), id, false, expiredReason)
	})
}

// answer gives the decision for the open prompt id to the agent.
func (s *session) answer(ctx context.Context, id string, allow bool, reason string) error {
	s.mu.Lock()
	i := slices.IndexFunc(s.permissions, func(p *openPermission) bool { return p.prompt.ID == id })
	if i < 0 {
		open := len(s.permissions)
		s.mu.Unlock()
		if open == 0 {
			return fmt.Errorf("The session %s has no open permission prompt.", s.id)
		}
		return fmt.Errorf("The session %s has no open permission prompt %s.", s.id, id)
	}
	s.removePermission(i)
	word := "deny"
	if allow {
		word = "allow"
	}
	if reason != "" {
		word += " (" + reason + ")"
	}
	s.log.add("= " + id + " " + word)
	s.show()
	vid := s.vid
	s.mu.Unlock()
	return s.rt.host.Answer(ctx, vid, id, agentvisor.Answer{Allow: allow, Message: reason})
}

// closeRequest drops a request that someone answered in another place, for example in the visor CLI.
// Call it with s.mu held.
func (s *session) closeRequest(id string) {
	if i := slices.IndexFunc(s.permissions, func(p *openPermission) bool { return p.prompt.ID == id }); i >= 0 {
		s.removePermission(i)
		s.log.add("= " + id + " answered")
		s.show()
		return
	}
	if s.question != nil && s.question.question.ID == id {
		s.log.add("= " + id + " answered")
		s.clearQuestion()
	}
}

// removePermission drops the open prompt at i. Call it with s.mu held.
func (s *session) removePermission(i int) {
	if t := s.permissions[i].timer; t != nil {
		t.Stop()
	}
	s.permissions = slices.Delete(s.permissions, i, i+1)
}

// dropRequests drops the open prompts and the open question. Call it with s.mu held.
func (s *session) dropRequests() {
	for len(s.permissions) > 0 {
		s.removePermission(0)
	}
	s.info.Permission = nil
	s.question = nil
	s.info.Question = nil
}
