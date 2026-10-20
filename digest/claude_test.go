package digest

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oversire/runner/wire"
)

func sameJSON(t *testing.T, name string, want []byte, got any) {
	t.Helper()
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(want, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s: digest differs\nwant %s\ngot  %s", name, want, out)
	}
}

func TestClaudeFixtures(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("..", "contract", "fixtures", "session-digest", "claude", "*.jsonl"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".jsonl")
		want, err := os.ReadFile(filepath.Join("..", "contract", "fixtures", "session-digest", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadClaudeFile(in, nil, wire.DigestFromFile)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sameJSON(t, name, want, got)
		var decoded wire.SessionDigest
		if err := wire.Decode(want, &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sameJSON(t, name+" round trip", want, decoded)
	}
}

func TestClaudeBudget(t *testing.T) {
	in := filepath.Join("..", "contract", "fixtures", "session-digest", "claude", "long.jsonl")
	b := wire.NewHistoryBudget(0, 1, 5)
	d, err := ReadClaudeFile(in, &b, wire.DigestFromRunner)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Earlier) != 0 || len(d.Last) != 1 || !d.Truncated || len([]rune(d.Last[0].Text)) > 5 || d.Source != "runner" {
		t.Fatalf("%+v", d)
	}
	b = wire.NewHistoryBudget(0, 0, 5)
	if d, _ = ReadClaudeFile(in, &b, "file"); len(d.Last) != 0 || d.Earlier == nil || d.Last == nil {
		t.Fatalf("%+v", d)
	}
}

func TestClaudeLines(t *testing.T) {
	long := strings.Repeat("é", 3<<20) // a line of several MB
	lines := []string{
		`not json`,
		`null`,
		`[1,2]`,
		`{"type":"user","isSidechain":true,"message":{"content":"side"}}`,
		`{"type":"user","message":{"content":"  hello  "}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + long + `"},{"type":"tool_use"}]}}`,
		`{"type":"user","message":{"content":"Caveat: the messages below"}}`,
		`{"type":"user","message":{"content":"<pasted text>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":" done "}]}}`,
	}
	d, err := ReadClaude(strings.NewReader(strings.Join(lines, "\n")), "stem", nil, "file")
	if err != nil {
		t.Fatal(err)
	}
	if d.SessionID != "stem" || d.Turns != 2 || d.Last[0].Prompt != "hello" || d.Last[0].Tools != 1 ||
		!d.Truncated || len([]rune(d.Last[0].Text)) != wire.DefaultMaxChars || d.Last[1].Text != "done" {
		t.Fatalf("%d %q %+v", d.Turns, d.SessionID, d.Last[1])
	}
}

func TestClaudeFind(t *testing.T) {
	root := t.TempDir()
	for i, dir := range []string{"-a", "-b"} {
		p := filepath.Join(root, dir, "abc.jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i-5) * time.Hour)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	c := NewClaudeReader(root)
	if p, err := c.Find("abc"); err != nil || filepath.Base(filepath.Dir(p)) != "-b" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"nope", "", "*", "../x"} {
		if _, err := c.Find(bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := ForHarness("codex", root); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatal(err)
	}
}
