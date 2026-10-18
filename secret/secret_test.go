package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const token = "f00dfeed0000000000000000000000000000000000000000000000000000beef"

// fakeKeys keeps one secret and records each call.
type fakeKeys struct {
	calls [][]string
	stdin []string
	value string
	fail  bool
}

func (f *fakeKeys) Run(_ context.Context, exe string, args []string, stdin []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{exe}, args...))
	f.stdin = append(f.stdin, string(stdin))
	if f.fail {
		return nil, []byte("no keyring"), errors.New("exit status 1")
	}
	switch {
	case len(args) == 1 && args[0] == "-i":
		if i := strings.Index(string(stdin), "-w \""); i >= 0 {
			f.value = strings.TrimSuffix(strings.TrimSpace(string(stdin)[i+4:]), "\"")
		}
	case args[0] == "store":
		f.value = string(stdin)
	case args[0] == "find-generic-password" || args[0] == "lookup":
		if f.value == "" {
			return nil, nil, errors.New("exit status 44")
		}
		return []byte(f.value + "\n"), nil, nil
	}
	return nil, nil, nil
}

func (f *fakeKeys) argvHasToken(t *testing.T) {
	t.Helper()
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), token) {
			t.Fatalf("argv holds the token: %q", c)
		}
	}
}

func found(string) (string, error)   { return "/usr/bin/secret-tool", nil }
func missing(string) (string, error) { return "", errors.New("not found") }

func TestKeychain(t *testing.T) {
	f := &fakeKeys{}
	file := filepath.Join(t.TempDir(), "token")
	s := New(f, "darwin", file, missing)
	ctx := context.Background()
	if err := s.Set(ctx, "dev1", token); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.stdin[0], `add-generic-password -U -s oversire-runner -a "dev1" -w "`+token) {
		t.Fatalf("stdin %q", f.stdin[0])
	}
	got, err := s.Get(ctx, "dev1")
	if err != nil || got != token {
		t.Fatal(got, err)
	}
	f.argvHasToken(t)
	if _, err := os.Stat(file); err == nil {
		t.Fatal("a token file exists next to the keychain")
	}
}

func TestSecretTool(t *testing.T) {
	f := &fakeKeys{}
	s := New(f, "linux", filepath.Join(t.TempDir(), "token"), found)
	ctx := context.Background()
	if err := s.Set(ctx, "dev1", token); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "dev1"); got != token {
		t.Fatal(got)
	}
	if f.stdin[0] != token {
		t.Fatalf("stdin %q", f.stdin[0])
	}
	f.argvHasToken(t)
}

func TestFileFallback(t *testing.T) {
	for _, c := range []struct {
		goos string
		run  *fakeKeys
		look func(string) (string, error)
	}{
		{"windows", &fakeKeys{}, missing},
		{"linux", &fakeKeys{}, missing},
		{"linux", &fakeKeys{fail: true}, found},
	} {
		file := filepath.Join(t.TempDir(), "token")
		s := New(c.run, c.goos, file, c.look)
		ctx := context.Background()
		if err := s.Set(ctx, "dev1", token); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Get(ctx, "dev1"); err != nil || got != token {
			t.Fatal(c.goos, got, err)
		}
		if st, _ := os.Stat(file); c.goos != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode())
		}
		c.run.argvHasToken(t)
		s.Delete(ctx, "dev1")
		if _, err := s.Get(ctx, "dev1"); err == nil {
			t.Fatal("token after delete")
		}
	}
}

func TestBadToken(t *testing.T) {
	s := New(&fakeKeys{}, "darwin", filepath.Join(t.TempDir(), "token"), missing)
	if err := s.Set(context.Background(), "d", "a\"\n-w x"); err == nil {
		t.Fatal("a token with a quote and a newline passed")
	}
}
