package capture

import (
	"strconv"
	"time"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// ClaimRows writes the session.claimed row when a session gets a start key.
type ClaimRows struct{ w *Writer }

func NewClaimRows(w *Writer) *ClaimRows { return &ClaimRows{w: w} }

// OnClaim writes one row for a claim that changed the key.
func (r *ClaimRows) OnClaim(c sessions.Claim, role string) {
	s, now := c.Session, time.Now()
	summary := "Session holds " + s.Key
	if role != "" {
		summary += " (" + role + ")"
	}
	pl := map[string]any{"key": s.Key, "runtime": s.Runtime}
	if role != "" {
		pl["role"] = role
	}
	if c.Previous != "" {
		pl["previous"] = c.Previous
	}
	if len(c.Freed) > 0 {
		pl["freed"] = c.Freed
	}
	r.w.AddFor(s, wire.EventDraft{
		ID:   ID("claim", s.ID, s.Key, strconv.FormatInt(now.UnixMilli(), 10)),
		Kind: wire.KindSessionClaimed, ActorType: wire.ActorAgent, Via: "cli", At: now,
		Summary: summary, Payload: payload(pl),
	})
}
