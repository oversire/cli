package worker

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func fold(t *testing.T, f Fold, r Read) string {
	t.Helper()
	var b bytes.Buffer
	if err := f.Write(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func toolOf(seq int, name, input string, status wire.ToolStatus, summary string) wire.SessionTurn {
	return wire.SessionTurn{Seq: seq, Role: wire.RoleTool, At: t0, Tool: &wire.TurnTool{Name: name, Status: status, Input: input, Summary: summary}}
}

func TestFoldFoldsRepeatedReads(t *testing.T) {
	var turns []wire.SessionTurn
	for i := 1; i <= 12; i++ {
		turns = append(turns, toolOf(i, "Read", fmt.Sprintf("file%d.go", i), wire.ToolDone, "x"))
	}
	turns = append(turns, assistant(13), toolOf(14, "Read", "last.go", wire.ToolDone, ""), toolOf(15, "Bash", "go test", wire.ToolDone, ""))
	out := fold(t, Fold{}, Read{Turns: turns})
	if !strings.Contains(out, "✓ 12 reads\n") || strings.Contains(out, "file1.go") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "✓ Read last.go\n") || !strings.Contains(out, "✓ Bash go test\n") {
		t.Fatalf("single read and bash:\n%s", out)
	}
	// A failed read breaks the run.
	broken := []wire.SessionTurn{toolOf(1, "Read", "a", wire.ToolDone, ""), toolOf(2, "Read", "b", wire.ToolFailed, "no such file"),
		toolOf(3, "Bash", "cat c.go", wire.ToolDone, ""), toolOf(4, "Bash", "rg foo", wire.ToolDone, "")}
	out = fold(t, Fold{}, Read{Turns: broken})
	if !strings.Contains(out, "✓ Read a\n") || !strings.Contains(out, "✗ Read b\n") || !strings.Contains(out, "✓ 2 reads\n") {
		t.Fatalf("%s", out)
	}
}

func TestFoldAddsTheTailOfAFailedTool(t *testing.T) {
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	turns := []wire.SessionTurn{toolOf(1, "Bash", "go test ./...", wire.ToolFailed, strings.Join(lines, "\n")+"\n")}
	out := fold(t, Fold{TailLines: 3}, Read{Turns: turns})
	want := "✗ Bash go test ./...\n    line 18\n    line 19\n    line 20\n"
	if !strings.HasSuffix(out, want) || strings.Contains(out, "line 17") {
		t.Fatalf("%q", out)
	}
	if out = fold(t, Fold{}, Read{Turns: turns}); !strings.Contains(out, "line 13\n") || strings.Contains(out, "line 12\n") {
		t.Fatalf("default tail:\n%s", out)
	}
	// A passing tool has no tail.
	turns[0].Tool.Status = wire.ToolDone
	if out = fold(t, Fold{}, Read{Turns: turns}); strings.Contains(out, "line 20") {
		t.Fatalf("%s", out)
	}
}

func TestFoldLabelsUserTurns(t *testing.T) {
	user := func(seq int, o wire.TurnOrigin, text string) wire.SessionTurn {
		return wire.SessionTurn{Seq: seq, Role: wire.RoleUser, At: t0, Text: text, Origin: o}
	}
	out := fold(t, Fold{}, Read{Turns: []wire.SessionTurn{
		user(1, wire.OriginUser, "a"), user(2, "", "b"), user(3, wire.OriginOverseer, "c"), user(4, wire.OriginSystem, "d"),
	}})
	for _, want := range []string{"[you] a\n", "[you] b\n", "[overseer] c\n", "[daemon] d\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestFoldClipsAgentText(t *testing.T) {
	long := strings.Repeat("é", 50)
	out := fold(t, Fold{MaxText: 20}, Read{Turns: []wire.SessionTurn{{Seq: 1, Role: wire.RoleAssistant, Text: long}, {Seq: 2, Role: wire.RoleAssistant, Text: "short"}}})
	if !strings.Contains(out, strings.Repeat("é", 20)+"\n[cut: 30 more characters]\n") || strings.Contains(out, strings.Repeat("é", 21)) {
		t.Fatalf("%q", out)
	}
	if !strings.Contains(out, "short\n") {
		t.Fatalf("%q", out)
	}
}

func TestFoldWritesTheHeader(t *testing.T) {
	h := Header{Session: "s1", Key: "work:OVERSIRE-12", Harness: "codex", Model: "gpt-5", State: wire.PhaseWaiting, Reason: wire.ReasonPermission,
		Since: t0, First: 4, Last: 9, Tokens: 1200, Request: "permission Bash npm test",
		Repos: []wire.ReviewRepo{{Name: "cli", Added: 10, Removed: 2, Files: 3}}}
	f := Fold{now: func() time.Time { return t0.Add(3 * time.Minute) }}
	var b bytes.Buffer
	if err := f.WriteHeader(&b, h); err != nil {
		t.Fatal(err)
	}
	want := "work:OVERSIRE-12 | codex gpt-5 | waiting (permission) for 3m | turns 4-9 | 1200 tokens\nOpen request: permission Bash npm test\ncli +10 -2, 3 files\n"
	if b.String() != want {
		t.Fatalf("%q", b.String())
	}
	h.Tokens, h.Request, h.Repos, h.First, h.Key = 0, "", nil, 0, ""
	b.Reset()
	f.WriteHeader(&b, h)
	if b.String() != "s1 | codex gpt-5 | waiting (permission) for 3m | no new turns\n" {
		t.Fatalf("%q", b.String())
	}
}

func TestWriteBriefKeepsEverySection(t *testing.T) {
	var out strings.Builder
	if err := (Fold{}).WriteBrief(&out, Brief{Header: Header{Key: "work:X-1", State: wire.PhaseWorking}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, h := range []string{"## Plan\nnone", "## Decisions\nnone", "## Last report\nnone", "## Failed tools\nnone", "## Open request\nnone", "## Diff\nnone"} {
		if !strings.Contains(got, h) {
			t.Fatalf("missing %q in:\n%s", h, got)
		}
	}
}
