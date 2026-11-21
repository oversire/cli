package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

func TestHooksCommands(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.home = t.TempDir()
	if err := c.hooks(context.Background(), []string{"install", "--harness", "claude"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(b), `'/bin/oversire' hook claude`) {
		t.Fatalf("%v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(c.home, ".codex")); !os.IsNotExist(err) {
		t.Fatal("codex was touched")
	}
	out.Reset()
	if err := c.hooks(context.Background(), []string{"status", "--json"}); err != nil {
		t.Fatal(err)
	}
	var st []wire.HarnessHooks
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || len(st) != 2 || st[0].Hooks != wire.HooksInstalled || st[1].Hooks != wire.HooksMissing {
		t.Fatalf("%v %s", err, out)
	}
	if err := c.hooks(context.Background(), []string{"remove"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(c.home, ".claude", "settings.json")); string(b) != "{}\n" {
		t.Fatalf("%s", b)
	}
	if err := c.hooks(context.Background(), []string{"install", "--harness", "cursor"}); err == nil {
		t.Fatal("an unknown harness must fail")
	}
	if err := c.hooks(context.Background(), []string{"bogus"}); err == nil {
		t.Fatal("an unknown command must fail")
	}
}

func TestHookCommandSpools(t *testing.T) {
	c, out, _ := testCLI(t, `{"session_id":"s1","hook_event_name":"Stop","cwd":"/tmp"}`)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	data := t.TempDir()
	if err := c.paths.SaveConfig(config.Config{DataDir: data, Serve: &config.ServeConfig{Port: port}}); err != nil {
		t.Fatal(err)
	}
	if err := c.hook(context.Background(), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{}\n" {
		t.Fatalf("stdout %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(data, "spool", "hooks.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestHooksCIRedirectCommand(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.home = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := c.hooks(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run("ci-redirect"); got != "CI redirect: on\n" {
		t.Fatalf("default %q", got)
	}
	run("ci-redirect", "off")
	if got := run("status"); !strings.HasSuffix(got, "CI redirect: off\n") {
		t.Fatalf("status %q", got)
	}
	if got := run("ci-redirect", "on"); got != "CI redirect: on\n" {
		t.Fatalf("on %q", got)
	}
	if err := c.hooks(context.Background(), []string{"ci-redirect", "maybe"}); err == nil {
		t.Fatal("a bad value must fail")
	}
}

func TestHookCommandUsesConfig(t *testing.T) {
	payload := `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"gh run watch 9"},"cwd":"/tmp"}`
	for _, on := range []bool{true, false} {
		c, out, _ := testCLI(t, payload)
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		cfg := config.Config{DataDir: t.TempDir(), Serve: &config.ServeConfig{Port: port}, Hooks: &config.HooksConfig{CIRedirect: &on}}
		if err := c.paths.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if err := c.hook(context.Background(), []string{"claude"}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), `"permissionDecision":"deny"`); got != on {
			t.Fatalf("ciRedirect %v: stdout %q", on, out.String())
		}
		if !on && out.String() != "{}\n" {
			t.Fatalf("stdout %q", out.String())
		}
	}
	// A config with no hooks key: the default is on. The port is free, so the real daemon gets no call.
	c, out, _ := testCLI(t, payload)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := c.paths.SaveConfig(config.Config{DataDir: t.TempDir(), Serve: &config.ServeConfig{Port: port}}); err != nil {
		t.Fatal(err)
	}
	if err := c.hook(context.Background(), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "oversire ci wait 9") {
		t.Fatalf("stdout %q", out.String())
	}
}
