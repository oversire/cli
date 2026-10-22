package sessions

import (
	"path/filepath"
	"testing"

	"github.com/oversire/cli/wire"
)

func TestBookReport(t *testing.T) {
	b, _ := newBook(filepath.Join(t.TempDir(), "sessions.json"))
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: "oversire", ID: "local"}, ID: "app/oversire-1"}, ID: "s-1"}
	rs := wire.RunnerSession{ID: "s-1", Runner: "mbp", Session: ref, Order: "wo-1", Agent: "claude", Repo: "/src/app", Branch: "oversire-1",
		State: wire.SessionWaitingPermission, StartedAt: at(0), UpdatedAt: at(0),
		Permission: &wire.PermissionPrompt{ID: "p-1", Tool: "Bash", Text: "ls", ExpiresAt: at(60e9)}}
	b.Report(rs)
	s, ok := b.Get("s-1")
	if !ok || s.Runtime != wire.RuntimeOversire || s.Harness != "claude" || s.Workspace != "app/oversire-1" || s.Branch != "oversire-1" ||
		s.State != wire.PhaseWaiting || s.Reason != wire.ReasonPermission || len(s.Sources) != 1 || s.Sources[0] != wire.SessionSourceWire {
		t.Fatalf("%+v", s)
	}
	if tool, input := b.Tool("s-1"); tool != "Bash" || input != "ls" {
		t.Fatal(tool, input)
	}

	rs.State, rs.Permission, rs.UpdatedAt = wire.SessionWaitingInput, nil, at(1e9)
	rs.Question = &wire.RunnerQuestion{ID: "q-1", Round: 1, AskedAt: at(1e9)}
	b.Report(rs)
	if s, _ := b.Get("s-1"); s.State != wire.PhaseWaiting || s.Reason != wire.ReasonQuestion || s.Question != "q-1" {
		t.Fatalf("%+v", s)
	}

	rs.State, rs.Question, rs.Summary, rs.UpdatedAt = wire.SessionDone, nil, "All tests pass.", at(2e9)
	b.Report(rs)
	if s, _ := b.Get("s-1"); s.State != wire.PhaseDone || s.Reason != "" || s.Question != "" || s.LastMessage != "All tests pass." {
		t.Fatalf("%+v", s)
	}
}
