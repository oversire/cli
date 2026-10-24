package proc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// unix needs a POSIX shell and /bin tools.
func unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
}

func TestLoginPath(t *testing.T) {
	unix(t)
	sh := filepath.Join(t.TempDir(), "sh")
	// A noisy rc file, then the command of -c.
	script := "#!/bin/sh\necho 'welcome __OVERSIRE_PATH__'\nPATH=/opt/nvm/bin:/usr/bin\neval \"$4\"\necho bye\n"
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := LoginPath(context.Background(), sh); got != "/opt/nvm/bin:/usr/bin" {
		t.Fatalf("got %q", got)
	}
	if got := LoginPath(context.Background(), "/no/such/shell"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMergePath(t *testing.T) {
	list := func(s string) string { return strings.ReplaceAll(s, ":", string(filepath.ListSeparator)) }
	got := MergePath(list("/opt/nvm/bin:/usr/bin"), list("/usr/bin:/bin::/usr/sbin"))
	if want := list("/opt/nvm/bin:/usr/bin:/bin:/usr/sbin"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
