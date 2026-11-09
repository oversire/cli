package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/ticket"
)

// TestSkillCommandsExist fails when the skill body names an oversire command that the CLI does not have.
func TestSkillCommandsExist(t *testing.T) {
	cmd := regexp.MustCompile("`oversire ([a-z]+)(?: ([a-z]+))?")
	s := ticket.NewSkill()
	n := 0
	fs.WalkDir(s.Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(s.Files(), p)
		for _, m := range cmd.FindAllStringSubmatch(string(b), -1) {
			n++
			if !usageHas(m[1], m[2]) {
				t.Errorf("%s names %q, which is not in the usage", p, m[0])
			}
		}
		return nil
	})
	if n < 5 {
		t.Fatalf("only %d commands found", n)
	}
}

func usageHas(cmd, sub string) bool {
	for _, line := range strings.Split(usage, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "oversire "+cmd+" ")
		if !ok {
			continue
		}
		if sub == "" {
			return true
		}
		words := strings.FieldsFunc(strings.Fields(rest)[0], func(r rune) bool { return r == '|' })
		for _, w := range words {
			if w == sub {
				return true
			}
		}
	}
	return false
}

func ticketTest(t *testing.T) (*cli, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return &cli{out: out, home: t.TempDir(), wd: "/w", paths: config.Paths{Dir: t.TempDir()}}, out
}

// putTranscript puts a Claude transcript of session id under the fake home.
func putTranscript(t *testing.T, home, id string) {
	t.Helper()
	b, err := os.ReadFile("../../contract/fixtures/session-digest/claude/long.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "projects", "-w", id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, b, 0o600)
}

func TestTicketWorkPrintsEarlierDigest(t *testing.T) {
	c, out := ticketTest(t)
	putTranscript(t, c.home, "s1")
	if err := c.ticket([]string{"new", "oversire-7", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.ticket([]string{"work", "OVERSIRE-7", "--session", "s2"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OVERSIRE-7: this session (claude s2) is bound (work)", "Earlier sessions:", "new   claude s1",
		"Digest of session s1", "### Prompt"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	got, _ := c.ledger().Sessions("OVERSIRE-7")
	if len(got) != 2 {
		t.Fatal(got)
	}
}

func TestTicketWorkTranscriptElsewhere(t *testing.T) {
	c, out := ticketTest(t)
	c.ledger().Add(ticket.SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Machine: "far", Role: "new"})
	if err := c.ticket([]string{"work", "A-1", "--session", "s2"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ssh far oversire digest --harness claude --session s1") {
		t.Fatal(out)
	}
}

func TestTicketDoneUsesBoundKey(t *testing.T) {
	c, out := ticketTest(t)
	putTranscript(t, c.home, "s1")
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	dir := t.TempDir()
	if err := c.ticket([]string{"done", "--session", "s1", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "claude-session-s1.jsonl.gz") + "\tclaude-session-s1.jsonl.gz\n"
	if out.String() != want {
		t.Fatalf("%q", out)
	}
	if items, _ := c.ledger().Items("s1"); len(items) != 0 {
		t.Fatal(items)
	}
}

func TestTicketDoneNoKeyTwoItems(t *testing.T) {
	c, _ := ticketTest(t)
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	c.ticket([]string{"new", "B-2", "--session", "s1"})
	err := c.ticket([]string{"done", "--session", "s1"})
	var e *exitError
	if !errors.As(err, &e) || !strings.Contains(err.Error(), "A-1, B-2") {
		t.Fatal(err)
	}
}

func TestTicketSkillBody(t *testing.T) {
	c, out := ticketTest(t)
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	if err := c.ticket([]string{"skill", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Tickets of this session: A-1") {
		t.Fatal(out)
	}
	out.Reset()
	if err := c.ticket([]string{"skill", "spec"}); err != nil || !strings.Contains(out.String(), "## Changes") {
		t.Fatal(err, out)
	}
	if err := c.ticket([]string{"skill", "enrich"}); err == nil {
		t.Fatal("no error for enrich")
	}
	if err := c.ticket([]string{"work"}); err == nil {
		t.Fatal("no error without a key")
	}
}

func TestTicketPlanManyKeys(t *testing.T) {
	c, out := ticketTest(t)
	if err := c.ticket([]string{"plan", "a-1", "B-2", "a-1", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if items, _ := c.ledger().Items("s1"); !slices.Equal(items, []string{"A-1", "B-2"}) {
		t.Fatal(items)
	}
	if n := strings.Count(out.String(), "is bound (plan)"); n != 2 || strings.Contains(out.String(), "Digest of session") {
		t.Fatal(out)
	}
	if !strings.Contains(out.String(), "B-2: no earlier sessions on this machine") {
		t.Fatal(out)
	}
}

func TestTicketPlanBadKeyBindsNone(t *testing.T) {
	c, _ := ticketTest(t)
	var e *exitError
	if err := c.ticket([]string{"plan", "A-1", "nokey", "--session", "s1"}); !errors.As(err, &e) || e.code != 2 {
		t.Fatal(err)
	}
	if items, _ := c.ledger().Items("s1"); len(items) != 0 {
		t.Fatal(items)
	}
}

func TestTicketPlanTooManyKeys(t *testing.T) {
	c, _ := ticketTest(t)
	args := []string{"plan", "--session", "s1"}
	for i := 1; i <= 21; i++ {
		args = append(args, fmt.Sprintf("A-%d", i))
	}
	var e *exitError
	if err := c.ticket(args); !errors.As(err, &e) || e.code != 2 || !strings.Contains(err.Error(), "at most 20") {
		t.Fatal(err)
	}
}

func TestTicketWorkOneKey(t *testing.T) {
	c, _ := ticketTest(t)
	for _, verb := range []string{"work", "new"} {
		var e *exitError
		if err := c.ticket([]string{verb, "A-1", "B-2", "--session", "s1"}); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%s: %v", verb, err)
		}
	}
}
