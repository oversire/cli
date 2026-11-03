package agentsecret

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSecurity is /usr/bin/security for the darwin backend. It keeps each item by account and records each argv.
type fakeSecurity struct {
	mu    sync.Mutex
	items map[string]string
	argv  []string
	stdin []string
}

var addCmd = regexp.MustCompile(`-a "((?:[^"\\]|\\.)*)" -w "([^"]*)"`)

func (f *fakeSecurity) Run(_ context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.items == nil {
		f.items = map[string]string{}
	}
	f.argv = append(f.argv, exe+" "+strings.Join(args, " "))
	f.stdin = append(f.stdin, string(stdin))
	switch {
	case len(args) == 1 && args[0] == "-i":
		m := addCmd.FindStringSubmatch(string(stdin))
		acct := strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1])
		f.items[acct] = m[2]
	case args[0] == "find-generic-password":
		if v, ok := f.items[args[4]]; ok {
			return []byte(v + "\n"), nil, nil
		}
		return nil, []byte("not found"), os.ErrNotExist
	case args[0] == "delete-generic-password":
		delete(f.items, args[4])
	}
	return nil, nil, nil
}

func noLook(string) (string, error) { return "", os.ErrNotExist }

const tricky = "a \"quoted\" value\nwith a new line\\ and $HOME 'and' `ticks`"

func TestStoreRoundTripsQuotesAndNewLines(t *testing.T) {
	for _, goos := range []string{"", "darwin"} {
		t.Run("goos="+goos, func(t *testing.T) {
			dir := t.TempDir()
			sec := &fakeSecurity{}
			s, err := NewStore(NewKeychain(sec, goos, filepath.Join(dir, "agent-secrets"), noLook), filepath.Join(dir, "agent-secrets.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Set(context.Background(), "shared", "claude", "ANTHROPIC_AUTH_TOKEN", tricky); err != nil {
				t.Fatal(err)
			}
			if v, ok := s.Value(context.Background(), "shared", "claude", "ANTHROPIC_AUTH_TOKEN"); !ok || v != tricky {
				t.Fatalf("value = %q, %v", v, ok)
			}
			for _, a := range sec.argv {
				if strings.Contains(a, "quoted") || strings.Contains(a, "new line") {
					t.Fatalf("the value is in argv: %s", a)
				}
			}
			index, _ := os.ReadFile(filepath.Join(dir, "agent-secrets.json"))
			if strings.Contains(string(index), "quoted") {
				t.Fatalf("the index holds the value: %s", index)
			}
			// Windows has no Unix file modes.
			if fi, err := os.Stat(filepath.Join(dir, "agent-secrets.json")); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
				t.Fatalf("index mode = %v, %v", fi.Mode(), err)
			}
			// A new Store reads the index again.
			s2, err := NewStore(NewKeychain(sec, goos, filepath.Join(dir, "agent-secrets"), noLook), filepath.Join(dir, "agent-secrets.json"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if v, ok := s2.Value(context.Background(), "shared", "claude", "ANTHROPIC_AUTH_TOKEN"); !ok || v != tricky {
				t.Fatalf("value after reload = %q, %v", v, ok)
			}
		})
	}
}

func TestStoreDeleteAndDropAgent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s, err := NewStore(NewKeychain(nil, "", dir, noLook), filepath.Join(dir, "index.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{"A", "B"} {
		if _, err := s.Set(ctx, "shared", "claude", k, "v"+k); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Set(ctx, "mba", "claude", "A", "local"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Delete(ctx, "shared", "claude", "A"); !ok || err != nil {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if ok, _ := s.Delete(ctx, "shared", "claude", "A"); ok {
		t.Fatal("a second delete found the key")
	}
	if _, ok := s.Value(ctx, "shared", "claude", "A"); ok {
		t.Fatal("the deleted value is still there")
	}
	gone, err := s.DropAgent(ctx, "shared", "claude")
	if err != nil || len(gone) != 1 || gone[0].Key != "B" {
		t.Fatalf("drop = %v, %v", gone, err)
	}
	if l := s.List(); len(l) != 1 || l[0].Scope != "mba" || !l[0].SetAt.Equal(now) {
		t.Fatalf("list = %+v", l)
	}
}
