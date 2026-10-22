package hooks

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oversire/cli/wire"
)

var update = flag.Bool("update", false, "write the golden files")

const testExe = "/usr/local/bin/oversire"

func command(h string) string { return Command(testExe, h) }

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	full := filepath.Join("testdata", path)
	if *update {
		if err := os.WriteFile(full, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs:\n%s", path, got)
	}
}

func TestCommand(t *testing.T) {
	want := `: oversire-hook=1; if [ -x '/usr/local/bin/oversire' ]; then '/usr/local/bin/oversire' hook claude; else cat >/dev/null; fi`
	if got := Command(testExe, "claude"); got != want {
		t.Fatalf("got %s", got)
	}
	if got := Command("/Users/o'neil/bin/oversire", "codex"); !strings.Contains(got, `'/Users/o'\''neil/bin/oversire'`) {
		t.Fatalf("quote: %s", got)
	}
}

func TestClaudeInstallRemove(t *testing.T) {
	orig := read(t, "claude/settings.json")
	c := Claude{}
	if s := c.Status(orig, nil); s.Hooks != wire.HooksMissing {
		t.Fatalf("status before: %s", s.Hooks)
	}
	got, err := c.Install(orig, command)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "claude/settings.installed.json", got)
	again, err := c.Install(got, command)
	if err != nil || !bytes.Equal(again, got) {
		t.Fatalf("second install changed the file: %v", err)
	}
	if s := c.Status(got, nil); s.Hooks != wire.HooksInstalled {
		t.Fatalf("status: %s", s.Hooks)
	}
	back, err := c.Remove(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, orig) {
		t.Fatalf("remove did not give the original bytes:\n%s", back)
	}
}

func TestClaudeOutdated(t *testing.T) {
	installed := read(t, "claude/settings.installed.json")
	old := bytes.ReplaceAll(installed, []byte("oversire-hook=1;"), []byte("oversire-hook=0;"))
	old = bytes.ReplaceAll(old, []byte("/usr/local/bin/oversire"), []byte("/opt/old/oversire"))
	if s := (Claude{}).Status(old, nil); s.Hooks != wire.HooksOutdated {
		t.Fatalf("status: %s", s.Hooks)
	}
	got, err := Claude{}.Install(old, command)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installed) {
		t.Fatalf("the outdated entries were not replaced:\n%s", got)
	}
}

func TestClaudeEmptyConfig(t *testing.T) {
	got, err := Claude{}.Install(nil, command)
	if err != nil {
		t.Fatal(err)
	}
	if s := (Claude{}).Status(got, nil); s.Hooks != wire.HooksInstalled {
		t.Fatalf("status: %s", s.Hooks)
	}
	back, err := Claude{}.Remove(got)
	if err != nil || string(back) != "{}\n" {
		t.Fatalf("remove: %q %v", back, err)
	}
}

func TestCodexInstallRemove(t *testing.T) {
	orig := read(t, "codex/hooks.json")
	c := Codex{}
	got, err := c.Install(orig, command)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "codex/hooks.installed.json", got)
	if again, _ := c.Install(got, command); !bytes.Equal(again, got) {
		t.Fatal("second install changed the file")
	}
	back, err := c.Remove(got)
	if err != nil || !bytes.Equal(back, orig) {
		t.Fatalf("remove: %v\n%s", err, back)
	}

	if s := c.Status(got, nil); s.Hooks != wire.HooksUntrusted {
		t.Fatalf("no trust: %s", s.Hooks)
	}
	if s := c.Status(got, read(t, "codex/config.untrusted.toml")); s.Hooks != wire.HooksUntrusted {
		t.Fatalf("partial trust: %s", s.Hooks)
	}
	if s := c.Status(got, read(t, "codex/config.trusted.toml")); s.Hooks != wire.HooksInstalled {
		t.Fatalf("trusted: %s", s.Hooks)
	}
	if s := c.Status(orig, read(t, "codex/config.trusted.toml")); s.Hooks != wire.HooksMissing {
		t.Fatalf("not installed: %s", s.Hooks)
	}
}

func TestSnake(t *testing.T) {
	for in, want := range map[string]string{"PreToolUse": "pre_tool_use", "Stop": "stop", "SessionStart": "session_start"} {
		if got := snake(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

type memFS map[string][]byte

func (m memFS) ReadFile(p string) ([]byte, error) {
	b, ok := m[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

func (m memFS) WriteFile(p string, b []byte) error {
	m[p] = b
	return nil
}

func TestInstaller(t *testing.T) {
	fs := memFS{filepath.Join("/h", ".claude", "settings.json"): read(t, "claude/settings.json")}
	i := NewInstaller("/h", testExe).WithFS(fs)
	st := i.Status()
	if len(st) != 2 || st[0] != (wire.HarnessHooks{Name: "claude", Hooks: wire.HooksMissing}) || st[1].Hooks != wire.HooksMissing {
		t.Fatalf("status: %+v", st)
	}
	st, err := i.Install("claude")
	if err != nil {
		t.Fatal(err)
	}
	if st[0].Hooks != wire.HooksInstalled || st[1].Hooks != wire.HooksMissing {
		t.Fatalf("after install: %+v", st)
	}
	if _, ok := fs[filepath.Join("/h", ".codex", "hooks.json")]; ok {
		t.Fatal("codex was written")
	}
	if _, err := i.Install("cursor"); err == nil {
		t.Fatal("an unknown harness must fail")
	}
	if _, err := i.Install(); err != nil {
		t.Fatal(err)
	}
	if i.Status()[1].Hooks != wire.HooksUntrusted {
		t.Fatalf("codex: %+v", i.Status())
	}
	if _, ok := fs[filepath.Join("/h", ".codex", "config.toml")]; ok {
		t.Fatal("the installer wrote the Codex trust")
	}
	if err := i.Remove(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fs[filepath.Join("/h", ".claude", "settings.json")], read(t, "claude/settings.json")) {
		t.Fatal("remove did not restore the claude config")
	}
	if string(fs[filepath.Join("/h", ".codex", "hooks.json")]) != "{}\n" {
		t.Fatalf("codex: %s", fs[filepath.Join("/h", ".codex", "hooks.json")])
	}
}
