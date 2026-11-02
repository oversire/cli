package agentsecret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"syscall"
	"time"

	"github.com/oversire/cli/wire"
)

// Roster tells where home is (group.Store).
type Roster interface {
	Group
	HomeEndpoint() (base, token string, self bool)
}

// Resolver gives the values on this daemon when it is home (*Home).
type Resolver interface {
	Resolve(ctx context.Context, in wire.AgentSecretResolve) (wire.AgentSecretValues, error)
}

// Doer sends one HTTP request (*http.Client).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Env fills the secret env of a launch. On home it reads the store in process. Else it calls
// POST /v1/agent-secrets/resolve on home with the group credential of this daemon. The values stay in memory.
type Env struct {
	group Roster
	local Resolver
	http  Doer
}

// NewEnv makes the filler. local is nil on a daemon with no secret store. client nil: a client with a 15 s timeout.
func NewEnv(group Roster, local Resolver, client Doer) *Env {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Env{group: group, local: local, http: client}
}

// Fill checks the env keys and puts the values of r.EnvSecrets into a copy of r.Env. order names the start in the
// secret.resolved event. The result has no EnvSecrets.
func (e *Env) Fill(ctx context.Context, order string, r wire.LaunchRequest) (wire.LaunchRequest, error) {
	if err := r.CheckEnv(); err != nil {
		return r, err
	}
	if len(r.EnvSecrets) == 0 {
		return r, nil
	}
	me := e.group.Self().Machine
	in := wire.AgentSecretResolve{Order: order, Machine: me, Refs: r.EnvSecrets}
	var v wire.AgentSecretValues
	var err error
	g, ok := e.group.Snapshot()
	switch {
	case !ok:
		return r, &Error{http.StatusServiceUnavailable, me + " has no group. A secret needs a route to home."}
	case g.Cloud != nil:
		return r, &Error{http.StatusServiceUnavailable, "The cloud holds the group of " + me + ". A secret needs a home daemon."}
	case g.Home == me && e.local != nil:
		v, err = e.local.Resolve(ctx, in)
	case g.Home == me:
		return r, &Error{http.StatusServiceUnavailable, me + " (home) has no secret store."}
	default:
		v, err = e.remote(ctx, g.Home, r.AgentName(), in)
	}
	if err != nil {
		return r, err
	}
	env := maps.Clone(r.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, k := range slices.Sorted(maps.Keys(r.EnvSecrets)) {
		val, ok := v.Env[k]
		if !ok {
			return r, &Error{http.StatusNotFound, fmt.Sprintf("%s has no value for %s of %s. Type it again in Settings › Agents.", g.Home, k, r.EnvSecrets[k].Agent)}
		}
		env[k] = val
	}
	r.Env, r.EnvSecrets = env, nil
	return r, nil
}

// remote calls home. A home that does not answer is offline; a network with no path to home cannot reach it.
func (e *Env) remote(ctx context.Context, home, agent string, in wire.AgentSecretResolve) (wire.AgentSecretValues, error) {
	var none wire.AgentSecretValues
	first := slices.Sorted(maps.Keys(in.Refs))[0]
	offline := &Error{http.StatusServiceUnavailable, fmt.Sprintf("%s (home) is offline. %s needs the secret %s.", home, agent, first)}
	noRoute := &Error{http.StatusBadGateway, fmt.Sprintf("%s cannot reach %s (home). A secret needs a route to home.", in.Machine, home)}
	base, token, self := e.group.HomeEndpoint()
	if self || base == "" || token == "" {
		return none, noRoute
	}
	body, err := json.Marshal(in)
	if err != nil {
		return none, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/agent-secrets/resolve", bytes.NewReader(body))
	if err != nil {
		return none, noRoute
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.http.Do(req)
	if err != nil {
		var dns *net.DNSError
		if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.As(err, &dns) {
			return none, noRoute
		}
		return none, offline
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var b struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &b)
		switch {
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
			if b.Error != "" {
				return none, &Error{resp.StatusCode, b.Error}
			}
		case resp.StatusCode == http.StatusUnauthorized:
			return none, noRoute
		}
		return none, &Error{http.StatusBadGateway, fmt.Sprintf("%s (home) refused the secrets of %s: %d %s", home, agent, resp.StatusCode, b.Error)}
	}
	var v wire.AgentSecretValues
	if err := json.Unmarshal(data, &v); err != nil {
		return none, &Error{http.StatusBadGateway, home + " (home) gave an answer that is not valid."}
	}
	return v, nil
}
