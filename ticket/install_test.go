package ticket

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWritesClaudeAndCodex(t *testing.T) {
	home := t.TempDir()
	i := NewInstaller(home)
	got, err := i.Install("claude", "codex")
	if err != nil || len(got) != 2 || !got[0].Changed || !got[1].Changed {
		t.Fatal(got, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "oversire", "SKILL.md"))
	if !strings.Contains(string(b), "!`oversire ticket skill`") || !strings.Contains(string(b), "$ARGUMENTS") {
		t.Fatal(string(b))
	}
	b, _ = os.ReadFile(filepath.Join(home, ".codex", "skills", "oversire", "SKILL.md"))
	if !strings.Contains(string(b), "run `oversire ticket skill`") {
		t.Fatal(string(b))
	}
	again, _ := i.Install("claude")
	if again[0].Changed {
		t.Fatal("a second install wrote the same file")
	}
	if _, err := i.Install("cursor"); err == nil {
		t.Fatal("no error for an unknown harness")
	}
}

func TestRefreshOnlyOldFiles(t *testing.T) {
	home := t.TempDir()
	i := NewInstaller(home)
	if got, err := i.Refresh(); err != nil || len(got) != 0 {
		t.Fatal("refresh made a file", got, err)
	}
	p := filepath.Join(home, ".claude", "skills", "oversire", "SKILL.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("old "+mark+"\n"), 0o644)
	got, err := i.Refresh()
	if err != nil || len(got) != 1 || !got[0].Changed {
		t.Fatal(got, err)
	}
	if b, _ := os.ReadFile(p); string(b) != thin["claude"] {
		t.Fatal(string(b))
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); err == nil {
		t.Fatal("refresh made the codex file")
	}
}

func TestInstallKeepsForeignFile(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "skills", "oversire", "SKILL.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("mine"), 0o644)
	if _, err := NewInstaller(home).Install(); err == nil {
		t.Fatal("no error")
	}
	if b, _ := os.ReadFile(p); string(b) != "mine" {
		t.Fatal("changed a foreign file")
	}
}

func TestInstallListsDelegate(t *testing.T) {
	home := t.TempDir()
	if _, err := NewInstaller(home).Install("claude"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "oversire", "SKILL.md"))
	for _, want := range []string{"argument-hint: new|plan|work|delegate|", "Bash(oversire worker:*)", "Bash(oversire ticket:*)"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in\n%s", want, b)
		}
	}
}
