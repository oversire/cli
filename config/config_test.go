package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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

func TestLoadConfigIgnoresAccount(t *testing.T) {
	p := Paths{Dir: t.TempDir()}
	if err := os.WriteFile(p.Config(), []byte(`{"hub":"h","account":"acc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok, err := p.LoadConfig()
	if err != nil || !ok || c.Hub != "h" {
		t.Fatal(c, ok, err)
	}
}

func TestWriteFile(t *testing.T) {
	for _, perm := range []os.FileMode{0o600, 0o644} {
		path := filepath.Join(t.TempDir(), "a", "b", "f.json")
		if err := WriteFile(path, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "x" {
			t.Fatal(string(got), err)
		}
		// Windows has no POSIX modes.
		if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != perm {
			t.Fatalf("mode %v want %v", fi.Mode().Perm(), perm)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatal("temp file left", err)
		}
	}
	path := filepath.Join(t.TempDir(), "j.json")
	if err := WriteJSON(path, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.HasSuffix(b, []byte("}\n")) {
		t.Fatalf("%q", b)
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

func TestShortHost(t *testing.T) {
	for in, want := range map[string]string{"mbp.local": "mbp", "mbp.tail8ed2.ts.net": "mbp", "mba": "mba", "": "", ".x": ".x"} {
		if got := shortHost(in); got != want {
			t.Errorf("shortHost(%q) = %q, want %q", in, got, want)
		}
	}
}
