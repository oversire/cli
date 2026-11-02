package wire

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestCheckEnvKey(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "_X", "a1"} {
		if err := CheckEnvKey(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"", "1A", "A-B", "A B", "OVERSIRE_SESSION", "AGENT_VISOR_PORT", "ORCA_PANE_KEY", strings.Repeat("A", 201)} {
		if err := CheckEnvKey(k); err == nil {
			t.Errorf("%q: no error", k)
		}
	}
}

func TestCheckEnv(t *testing.T) {
	ok := LaunchRequest{Env: map[string]string{"A": "x"}, EnvSecrets: map[string]AgentSecretRef{"B": {Agent: "claude", Scope: AgentSecretShared}}}
	if err := ok.CheckEnv(); err != nil {
		t.Fatal(err)
	}
	if got := ok.EnvKeys(); strings.Join(got, ",") != "A,B" {
		t.Fatal(got)
	}
	bad := []LaunchRequest{
		{Env: map[string]string{"OVERSIRE_X": "1"}},
		{Env: map[string]string{"A": strings.Repeat("x", MaxEnvValue+1)}},
		{Env: map[string]string{"A": "1"}, EnvSecrets: map[string]AgentSecretRef{"A": {Agent: "c", Scope: "shared"}}},
		{EnvSecrets: map[string]AgentSecretRef{"A": {Agent: "c"}}},
	}
	for i, r := range bad {
		err := r.CheckEnv()
		if err == nil {
			t.Errorf("%d: no error", i)
		} else if strings.Contains(err.Error(), "xxx") {
			t.Errorf("%d: the error holds the value", i)
		}
	}
}

func TestLaunchRequestLogsNoValue(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	r := LaunchRequest{Repo: "/r", Agent: "claude", Prompt: "do it", Env: map[string]string{"TOKEN": "s3cr3t-value"},
		EnvSecrets: map[string]AgentSecretRef{"KEY": {Agent: "claude", Scope: "shared"}}}
	log.Info("launch", "req", r, "set", AgentSecretSet{Value: "s3cr3t-value"}, "values", AgentSecretValues{Env: map[string]string{"K": "s3cr3t-value"}})
	out := buf.String()
	if strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "do it") || !strings.Contains(out, `"envKeys":["KEY","TOKEN"]`) {
		t.Fatal(out)
	}
}
