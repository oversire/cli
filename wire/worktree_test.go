package wire

import "testing"

func TestWorktreeBindingRoundTrip(t *testing.T) {
	roundTrip[WorktreeBinding](t, "worktree-binding")
	roundTrip[WorktreeBindingsPage](t, "worktree-bindings-page")
	roundTrip[WorktreesPage](t, "worktrees-page")
}

func TestWorktreeBindingDirJoinsARelativeGroup(t *testing.T) {
	if got := (WorktreeBinding{Root: "/w", Group: ".wt/x-1"}).Dir(); got != "/w/.wt/x-1" {
		t.Fatal(got)
	}
	if got := (WorktreeBinding{Root: "/w", Group: "/tmp/quick"}).Dir(); got != "/tmp/quick" {
		t.Fatal(got)
	}
}
