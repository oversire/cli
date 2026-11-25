package orders

import (
	"log/slog"
	"net/http"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
)

const maxClaimBody = 64 << 10

// Claims serves POST /v1/sessions/claim (loopback only): it gives a start key to a live session
// that the daemon did not start, so a keyed start reuses it.
type Claims struct {
	Log *slog.Logger

	runner  *Runner
	onClaim func(c sessions.Claim, role string) // nil: no row
}

func NewClaims(r *Runner, onClaim func(c sessions.Claim, role string)) *Claims {
	return &Claims{Log: slog.Default(), runner: r, onClaim: onClaim}
}

func (c *Claims) Routes() []server.Route {
	return []server.Route{{Pattern: "POST /v1/sessions/claim", Access: server.LoopbackOnly, Handle: c.post}}
}

func (c *Claims) post(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if !server.Decode(w, r, maxClaimBody, "claim: ", &req) {
		return
	}
	claim, err := c.runner.Claim(r.Context(), req)
	if err != nil {
		server.Fail(w, err)
		return
	}
	if claim.Changed {
		c.Log.Info("session claimed", "session", claim.Session.ID, "key", claim.Session.Key, "previous", claim.Previous, "freed", claim.Freed)
		if c.onClaim != nil {
			c.onClaim(claim, req.Role)
		}
	}
	server.WriteJSON(w, http.StatusOK, claim)
}
