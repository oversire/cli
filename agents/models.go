// Package agents serves facts about the agent CLIs of the machine.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// probeTimeout holds a model probe that starts a CLI. The agent-wire cache answers the next calls.
const probeTimeout = 30 * time.Second

// Lister lists the models of a harness (agentwire.Runtime, with its cache).
type Lister interface {
	Models(ctx context.Context, h agentwire.Harness, q agentwire.ModelQuery) ([]agentwire.ModelInfo, error)
}

// API serves GET /v1/agents/{agent}/models.
type API struct {
	Log *slog.Logger

	lister Lister
}

func NewAPI(l Lister) *API {
	return &API{Log: slog.Default(), lister: l}
}

func (a *API) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/agents/{agent}/models", Access: server.Member, Handle: a.models},
	}
}

// models gives an empty list and a reason when the CLI cannot list. The app then uses its own list.
func (a *API) models(w http.ResponseWriter, r *http.Request) {
	agent := r.PathValue("agent")
	h := agentwire.Harness(agent)
	if h == agentwire.Fake || h == agentwire.Native || !slices.Contains(agentwire.Harnesses(), h) {
		writeError(w, http.StatusNotFound, "The agent "+agent+" is not known.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	list, err := a.lister.Models(ctx, h, agentwire.ModelQuery{})
	out := wire.AgentModels{Agent: agent, Models: []wire.AgentModel{}, Source: wire.ModelSourceWire}
	switch {
	case errors.Is(err, agentwire.ErrUnsupported):
		out.Source, out.Reason = wire.ModelSourceNone, agent+" cannot list its models."
	case err != nil:
		a.Log.Warn("agent models", "agent", agent, "err", err)
		out.Source, out.Reason = wire.ModelSourceNone, err.Error()
	}
	for _, m := range list {
		out.Models = append(out.Models, wire.AgentModel{ID: m.ID, Name: m.Name, Description: m.Description, IsDefault: m.Default})
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
