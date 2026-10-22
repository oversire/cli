package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDigestMarkers(t *testing.T) {
	in, _ := filepath.Abs("../../contract/fixtures/session-digest/claude/long.jsonl")
	want, err := os.ReadFile("../../contract/fixtures/session-digest/long.json")
	if err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	c := &cli{out: out, home: t.TempDir()}
	if err := c.digest([]string{"--harness", "claude", "--session", in, "--markers"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if lines[0] != "OVERSIRE-DIGEST-BEGIN" || lines[len(lines)-1] != "OVERSIRE-DIGEST-END" {
		t.Fatalf("%q", out)
	}
	for _, l := range lines[1 : len(lines)-1] {
		if len(l) > 76 {
			t.Fatalf("line of %d chars", len(l))
		}
	}
	z, err := base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-1], ""))
	if err != nil {
		t.Fatal(err)
	}
	r, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r)
	var a, b any
	json.Unmarshal(want, &a)
	json.Unmarshal(body, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("got %s", body)
	}
}

func TestDigestErrors(t *testing.T) {
	out := &bytes.Buffer{}
	c := &cli{out: out, home: t.TempDir()}
	err := c.digest([]string{"--session", "abc", "--markers"})
	var e *exitError
	if !errors.As(err, &e) || e.code != 1 || !errors.Is(err, errSilent) || !strings.HasPrefix(out.String(), "OVERSIRE-DIGEST-ERROR no transcript") {
		t.Fatalf("%v %q", err, out)
	}
	if err := c.digest([]string{"--harness", "codex", "abc"}); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"--max-chars", "0", "abc"}, {"a", "b"}} {
		if err := c.digest(args); !errors.As(err, &e) || e.code != 2 {
			t.Errorf("%q: %v", args, err)
		}
	}
}
