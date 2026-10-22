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
