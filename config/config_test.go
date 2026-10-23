package config

import (
	"log/slog"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Setenv("OVERSIRE_RUNNER_HOME", "/tmp/r")
	var c Config
	if c.Sessions() != DefaultMaxSessions {
		t.Fatal(c.Sessions())
	}
	if d, err := c.Data(); err != nil || d != filepath.Join("/tmp/r", "data") {
		t.Fatal(d, err)
	}
	c = Config{MaxSessions: 5, DataDir: "/data"}
	if d, _ := c.Data(); c.Sessions() != 5 || d != "/data" {
		t.Fatal(c.Sessions(), d)
	}
}

func TestConfigKeepsSessionFields(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	if err := p.SaveConfig(Config{Hub: "h", Device: "d", MaxSessions: 3, DataDir: "/data"}); err != nil {
		t.Fatal(err)
	}
	c, ok, err := p.LoadConfig()
	if err != nil || !ok || c.MaxSessions != 3 || c.DataDir != "/data" {
		t.Fatal(c, ok, err)
	}
}

func TestLogLevel(t *testing.T) {
	for _, c := range []struct {
		env, cfg string
		want     slog.Level
		bad      bool
	}{
		{"", "", slog.LevelInfo, false},
		{"", "debug", slog.LevelDebug, false},
		{"warn", "debug", slog.LevelWarn, false},
		{"loud", "", slog.LevelInfo, true},
	} {
		cfg := Config{Log: &LogConfig{Level: c.cfg}}
		got, err := cfg.LogLevel(c.env)
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("env %q, config %q: got %v, %v", c.env, c.cfg, got, err)
		}
	}
}
