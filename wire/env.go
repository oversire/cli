package wire

import (
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// AgentSecretShared is the scope of the secrets of a shared agent.
	AgentSecretShared = "shared"
	// AllAgents is the agent name of the env for all agents.
	AllAgents = "*"

	MaxEnvVars  = 64
	MaxEnvValue = 8192
	maxEnvKey   = 200
)

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckEnvKey refuses a key that is not a valid env name, or that OverSire, agent-visor or Orca sets.
func CheckEnvKey(key string) error {
	switch {
	case !envKey.MatchString(key) || len(key) > maxEnvKey:
		return fmt.Errorf("The env key %q is not valid.", key)
	case strings.HasPrefix(key, "OVERSIRE_"), strings.HasPrefix(key, "AGENT_VISOR_"), key == "ORCA_PANE_KEY":
		return fmt.Errorf("The env key %s is reserved.", key)
	}
	return nil
}

// CheckEnv checks the keys of Env and EnvSecrets, and the size of each value. An error never holds a value.
func (r LaunchRequest) CheckEnv() error {
	if len(r.Env) > MaxEnvVars || len(r.EnvSecrets) > MaxEnvVars {
		return fmt.Errorf("A launch has at most %d env keys.", MaxEnvVars)
	}
	for k, v := range r.Env {
		if err := CheckEnvKey(k); err != nil {
			return err
		}
		if utf8.RuneCountInString(v) > MaxEnvValue {
			return fmt.Errorf("The value of the env key %s is longer than %d characters.", k, MaxEnvValue)
		}
	}
	for k, ref := range r.EnvSecrets {
		if err := CheckEnvKey(k); err != nil {
			return err
		}
		if _, ok := r.Env[k]; ok {
			return fmt.Errorf("The env key %s is plain and secret.", k)
		}
		if ref.Agent == "" || ref.Scope == "" {
			return fmt.Errorf("The secret %s needs an agent and a scope.", k)
		}
	}
	return nil
}

// EnvKeys gives the keys of Env and EnvSecrets, sorted.
func (r LaunchRequest) EnvKeys() []string {
	keys := slices.Collect(maps.Keys(r.Env))
	for k := range r.EnvSecrets {
		if _, ok := r.Env[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// AgentName is the OverSire agent name: Profile, else the base CLI.
func (r LaunchRequest) AgentName() string {
	if r.Profile != "" {
		return r.Profile
	}
	return r.Agent
}

// LogValue gives the env keys and never a value or the prompt.
func (r LaunchRequest) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("repo", r.Repo), slog.String("agent", r.Agent)}
	for _, a := range []struct{ k, v string }{{"key", r.Key}, {"profile", r.Profile}, {"model", r.Model}} {
		if a.v != "" {
			attrs = append(attrs, slog.String(a.k, a.v))
		}
	}
	if keys := r.EnvKeys(); len(keys) > 0 {
		attrs = append(attrs, slog.Any("envKeys", keys))
	}
	return slog.GroupValue(attrs...)
}

// AgentSecretSet is the body of PUT /v1/agent-secrets/{scope}/{agent}/{key}.
type AgentSecretSet struct {
	Value string `json:"value"`
}

// LogValue hides the value.
func (AgentSecretSet) LogValue() slog.Value { return slog.StringValue("[secret]") }

// AgentSecretKey is one secret on home, with no value.
type AgentSecretKey struct {
	Scope string    `json:"scope"`
	Agent string    `json:"agent"`
	Key   string    `json:"key"`
	SetAt time.Time `json:"setAt"`
}

// AgentSecretList is the answer of GET /v1/agent-secrets.
type AgentSecretList struct {
	Secrets []AgentSecretKey `json:"secrets"`
}

// AgentSecretResolve is the body of POST /v1/agent-secrets/resolve.
type AgentSecretResolve struct {
	Order   string                    `json:"order,omitempty"`
	Machine string                    `json:"machine"`
	Refs    map[string]AgentSecretRef `json:"refs"`
}

// AgentSecretValues is the answer of POST /v1/agent-secrets/resolve: env key to value.
type AgentSecretValues struct {
	Env map[string]string `json:"env"`
}

// LogValue gives the keys only.
func (v AgentSecretValues) LogValue() slog.Value {
	return slog.AnyValue(slices.Sorted(maps.Keys(v.Env)))
}
