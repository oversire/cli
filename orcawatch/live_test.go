package orcawatch

import (
	"context"
	"os"
	"testing"

	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/sessions"
)

// TestLive polls the Orca of this machine once. It runs only with OVERSIRE_LIVE=1. The calls only read.
func TestLive(t *testing.T) {
	if os.Getenv("OVERSIRE_LIVE") != "1" {
		t.Skip("OVERSIRE_LIVE is not 1")
	}
	exe, ok := orca.FindCLI()
	if !ok {
		t.Skip("no Orca CLI")
	}
	book := sessions.NewBook("live", "", nil, nil)
	w := NewWatcher(orca.New(exe, proc.Local{}), book, nil)
	w.poll(context.Background())
	if w.Health() != HealthOK {
		t.Fatalf("health: %s", w.Health())
	}
	for _, s := range book.All() {
		t.Logf("%s %s %s %s %s", s.ID, s.State, s.Reason, s.Terminal, s.Repo)
	}
	if len(book.All()) == 0 {
		t.Fatal("no sessions")
	}
}
