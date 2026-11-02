package agentsecret

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	collection = "agents"
	maxBody    = 64 << 10
	maxName    = 200
)

// Error is a refusal with the HTTP status of the resolve route. Text names the machine and the key, never a value.
type Error struct {
	Status int
	Text   string
}

func (e *Error) Error() string { return e.Text }

// Group gives the roster of this machine (group.Store).
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

// Agents reads a record of the home store (store.OpLog).
type Agents interface {
	Record(collection, id string) (json.RawMessage, bool, error)
}

// Events takes the event rows (capture.Writer).
type Events interface {
	AddFor(s wire.DaemonSession, d wire.EventDraft)
}

// Home serves the secrets on the home daemon: the /v1/agent-secrets routes and the resolve of a launch on this machine.
type Home struct {
	Log *slog.Logger

	store  *Store
	group  Group
	agents Agents
	events Events
	clock  func() time.Time
}

func NewHome(store *Store, group Group, agents Agents, events Events, clock func() time.Time) *Home {
	if clock == nil {
		clock = time.Now
	}
	return &Home{Log: slog.Default(), store: store, group: group, agents: agents, events: events, clock: clock}
}

// Routes gives the secret routes. A daemon that is not home answers 409 with the home name.
func (h *Home) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/agent-secrets", Access: server.Member, Handle: h.home(h.apps(h.list))},
		{Pattern: "PUT /v1/agent-secrets/{scope}/{agent}/{key}", Access: server.Member, Handle: h.home(h.apps(h.set))},
		{Pattern: "DELETE /v1/agent-secrets/{scope}/{agent}/{key}", Access: server.Member, Handle: h.home(h.apps(h.remove))},
		{Pattern: "POST /v1/agent-secrets/resolve", Access: server.Member, Handle: h.home(h.resolve)},
	}
}

// notHome is the 409 body, as in the store routes.
type notHome struct {
	Error string           `json:"error"`
	Home  string           `json:"home,omitempty"`
	Cloud *wire.GroupCloud `json:"cloud,omitempty"`
}

func (h *Home) home(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, ok := h.group.Snapshot()
		switch {
		case !ok:
			writeJSON(w, http.StatusConflict, notHome{Error: "this machine has no group"})
		case g.Cloud != nil:
			writeJSON(w, http.StatusConflict, notHome{Error: "the cloud is home", Cloud: g.Cloud})
		case g.Home != h.group.Self().Machine:
			writeJSON(w, http.StatusConflict, notHome{Error: g.Home + " is home", Home: g.Home})
		default:
			next(w, r)
		}
	}
}

// apps refuses a daemon credential: only an app sets and lists the secrets.
func (h *Home) apps(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, ok := server.CallerFrom(r.Context()); ok && c.Kind == string(wire.DeviceDaemon) {
			writeError(w, http.StatusForbidden, "only an app sets the secrets")
			return
		}
		next(w, r)
	}
}

func (h *Home) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, wire.AgentSecretList{Secrets: h.store.List()})
}

// path reads and checks {scope}, {agent} and {key}.
func path(r *http.Request) (scope, agent, key string, err error) {
	scope, agent, key = r.PathValue("scope"), r.PathValue("agent"), r.PathValue("key")
	if !validName(scope) {
		return "", "", "", fmt.Errorf("The scope %q is not valid.", scope)
	}
	if !validName(agent) {
		return "", "", "", fmt.Errorf("The agent name %q is not valid.", agent)
	}
	return scope, agent, key, wire.CheckEnvKey(key)
}

// validName is a scope or an agent name: 1 to 200 characters, no slash and no control character.
func validName(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= maxName && !strings.ContainsFunc(s, func(r rune) bool { return r == '/' || unicode.IsControl(r) })
}

func (h *Home) set(w http.ResponseWriter, r *http.Request) {
	scope, agent, key, err := path(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	var in wire.AgentSecretSet
	if err == nil {
		err = wire.Decode(body, &in)
	}
	if err != nil || in.Value == "" || utf8.RuneCountInString(in.Value) > wire.MaxEnvValue {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("The body must be {value} with 1 to %d characters.", wire.MaxEnvValue))
		return
	}
	row, err := h.store.Set(r.Context(), scope, agent, key, in.Value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.changed(r, scope, agent, key, "set")
	writeJSON(w, http.StatusOK, row)
}

func (h *Home) remove(w http.ResponseWriter, r *http.Request) {
	scope, agent, key, err := path(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ok, err := h.store.Delete(r.Context(), scope, agent, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ok {
		h.changed(r, scope, agent, key, "removed")
	}
	w.WriteHeader(http.StatusNoContent)
}

// changed writes secret.set. via is the app and its machine.
func (h *Home) changed(r *http.Request, scope, agent, key, action string) {
	machine := h.group.Self().Machine
	if c, ok := server.CallerFrom(r.Context()); ok && c.Machine != "" {
		machine = c.Machine
	}
	now := h.clock()
	summary := "Secret " + key + " of " + agent + " set"
	if action == "removed" {
		summary = "Secret " + key + " of " + agent + " removed"
	}
	h.add(wire.DaemonSession{}, wire.EventDraft{ID: "secret:" + account(scope, agent, key) + ":" + strconv.FormatInt(now.UnixMilli(), 10),
		At: now, Kind: "secret.set", ActorType: wire.ActorHuman, Via: "app@" + machine, Summary: summary,
		Payload: mustJSON(map[string]string{"agent": agent, "scope": scope, "key": key, "action": action})})
}

// resolve answers the values to a daemon of the group over Tailscale, for its own machine. It refuses loopback
// (an app on home) and app credentials: no app reads a value back.
func (h *Home) resolve(w http.ResponseWriter, r *http.Request) {
	c, ok := server.CallerFrom(r.Context())
	if !ok || c.Loopback || c.Kind != string(wire.DeviceDaemon) {
		writeError(w, http.StatusForbidden, "only a daemon of the group reads the secrets")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	var in wire.AgentSecretResolve
	if err == nil {
		err = wire.Decode(body, &in)
	}
	if err == nil {
		err = wire.LaunchRequest{EnvSecrets: in.Refs}.CheckEnv()
	}
	if err != nil || len(in.Refs) == 0 || in.Machine == "" {
		writeError(w, http.StatusBadRequest, "The body must be {machine, refs} with valid env keys.")
		return
	}
	if in.Machine != c.Machine {
		writeError(w, http.StatusForbidden, "a daemon reads the secrets only for its own machine")
		return
	}
	v, err := h.Resolve(r.Context(), in)
	if err != nil {
		code := http.StatusInternalServerError
		if e, ok := err.(*Error); ok {
			code = e.Status
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Resolve checks the Runs on list of each shared agent against in.Machine and gives the values.
// The env for all agents and a secret of one Mac have no Runs on check. It writes secret.resolved with the keys only.
func (h *Home) Resolve(ctx context.Context, in wire.AgentSecretResolve) (wire.AgentSecretValues, error) {
	home := h.group.Self().Machine
	keys := slices.Sorted(maps.Keys(in.Refs))
	agent := wire.AllAgents
	for _, k := range keys {
		ref := in.Refs[k]
		if ref.Agent != wire.AllAgents && agent == wire.AllAgents {
			agent = ref.Agent
		}
		if ref.Scope != wire.AgentSecretShared || ref.Agent == wire.AllAgents {
			continue
		}
		runsOn, err := h.runsOn(ref.Agent)
		if err != nil {
			return wire.AgentSecretValues{}, err
		}
		if len(runsOn) > 0 && !slices.Contains(runsOn, in.Machine) {
			return wire.AgentSecretValues{}, &Error{http.StatusForbidden,
				fmt.Sprintf("%s cannot run on %s. Its Runs on list is %s.", ref.Agent, in.Machine, strings.Join(runsOn, ", "))}
		}
	}
	out := wire.AgentSecretValues{Env: map[string]string{}}
	for _, k := range keys {
		ref := in.Refs[k]
		v, ok := h.store.Value(ctx, ref.Scope, ref.Agent, k)
		if !ok {
			name := ref.Agent
			if name == wire.AllAgents {
				name = "all agents"
			}
			return wire.AgentSecretValues{}, &Error{http.StatusNotFound,
				fmt.Sprintf("%s has no value for %s of %s. Type it again in Settings › Agents.", home, k, name)}
		}
		out.Env[k] = v
	}
	now := h.clock()
	noun := "secrets"
	if len(keys) == 1 {
		noun = "secret"
	}
	pl := map[string]any{"agent": agent, "keys": keys, "machine": in.Machine}
	if in.Order != "" {
		pl["order"] = in.Order
	}
	h.add(wire.DaemonSession{Key: in.Order}, wire.EventDraft{ID: "secret:resolve:" + cmpOr(in.Order, in.Machine) + ":" + strconv.FormatInt(now.UnixMilli(), 10),
		At: now, Kind: "secret.resolved", ActorType: wire.ActorSystem, Actor: "agent-secrets", Via: "daemon",
		Summary: fmt.Sprintf("%s got %d %s of %s", in.Machine, len(keys), noun, agent), Payload: mustJSON(pl)})
	h.Log.Info("agent secrets resolved", "machine", in.Machine, "order", in.Order, "keys", keys)
	return out, nil
}

// runsOn reads the Runs on list of a shared agent. A missing agent has none.
func (h *Home) runsOn(agent string) ([]string, error) {
	if h.agents == nil {
		return nil, nil
	}
	rec, ok, err := h.agents.Record(collection, agent)
	if err != nil || !ok {
		return nil, err
	}
	var def struct {
		RunsOn []string `json:"runsOn"`
	}
	if err := json.Unmarshal(rec, &def); err != nil {
		return nil, fmt.Errorf("The record of the agent %s is not valid.", agent)
	}
	return def.RunsOn, nil
}

// Forget removes the shared secrets of each agent that the home store deleted (store.OpLog.OnApply).
func (h *Home) Forget(accepted []wire.StoreOp) {
	for _, op := range accepted {
		if op.Collection != collection || op.Op != wire.StoreDelete {
			continue
		}
		gone, err := h.store.DropAgent(context.Background(), wire.AgentSecretShared, op.ID)
		if err != nil {
			h.Log.Warn("agent secrets: forget", "agent", op.ID, "err", err)
		}
		if len(gone) > 0 {
			h.Log.Info("agent secrets: the agent is gone, its secrets too", "agent", op.ID, "keys", len(gone))
		}
	}
}

func (h *Home) add(s wire.DaemonSession, d wire.EventDraft) {
	if h.events != nil {
		h.events.AddFor(s, d)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
