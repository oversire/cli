package ticket

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines int) (string, []byte) {
	t.Helper()
	var b bytes.Buffer
	r := rand.New(rand.NewSource(1))
	for i := range lines {
		noise := make([]byte, 600)
		for j := range noise {
			noise[j] = byte('a' + r.Intn(26))
		}
		fmt.Fprintf(&b, `{"n":%d,"text":%q}`+"\n", i, noise)
	}
	p := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(p, b.Bytes(), 0o600)
	return p, b.Bytes()
}

func gunzip(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPackSmallOneFile(t *testing.T) {
	src, want := writeTranscript(t, 10)
	dir := t.TempDir()
	got, err := Packer{Max: MaxAttachment}.Pack(src, dir, "claude-session-s1")
	if err != nil || len(got) != 1 || filepath.Base(got[0]) != "claude-session-s1.jsonl.gz" {
		t.Fatal(got, err)
	}
	if !bytes.Equal(gunzip(t, got[0]), want) {
		t.Fatal("content differs")
	}
}

func TestPackSplitsAtLineEnds(t *testing.T) {
	src, want := writeTranscript(t, 6000) // about 3.6 MB of random text, about 2.2 MB gzipped
	dir := t.TempDir()
	max := int64(1 << 20)
	got, err := Packer{Max: max}.Pack(src, dir, "claude-session-s1")
	if err != nil || len(got) < 3 {
		t.Fatal(got, err)
	}
	var all []byte
	for i, p := range got {
		if filepath.Base(p) != fmt.Sprintf("claude-session-s1.part%d.jsonl.gz", i+1) {
			t.Fatal(p)
		}
		if st, _ := os.Stat(p); st.Size() > max {
			t.Fatalf("%s has %d bytes", p, st.Size())
		}
		b := gunzip(t, p)
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if !json.Valid(sc.Bytes()) {
				t.Fatalf("%s: a bad line %.40q", p, sc.Text())
			}
		}
		all = append(all, b...)
	}
	if !bytes.Equal(all, want) {
		t.Fatal("the parts do not join to the source")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(left) != 0 || strings.Contains(strings.Join(got, ""), ".tmp") {
		t.Fatal(left)
	}
}
