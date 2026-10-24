package proc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginPath(t *testing.T) {
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
	got := MergePath("/opt/nvm/bin:/usr/bin", "/usr/bin:/bin::/usr/sbin")
	if want := "/opt/nvm/bin:/usr/bin:/bin:/usr/sbin"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
