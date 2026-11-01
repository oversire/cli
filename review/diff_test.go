package review

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func TestParseDiff(t *testing.T) {
	long := "@@ -0,0 +1,3005 @@\n" + strings.Repeat("+x\n", 3005)
	tests := []struct {
		name      string
		out       string
		status    wire.FileStatus
		oldPath   string
		binary    bool
		truncated bool
		hunks     []wire.DiffHunk
	}{
		{
			name:   "modified",
			status: wire.FileModified,
			out: "diff --git a/lib.go b/lib.go\nindex 1..2 100644\n--- a/lib.go\n+++ b/lib.go\n" +
				"@@ -1,3 +1,3 @@ package lib\n package lib\n-func A() {}\n+func B() {}\n \n\\ No newline at end of file\n" +
				"@@ -10 +10,2 @@\n x\n+y\n",
			hunks: []wire.DiffHunk{
				{OldStart: 1, OldLines: 3, NewStart: 1, NewLines: 3, Header: "package lib", Lines: []wire.DiffLine{
					{Kind: wire.DiffCtx, Old: 1, New: 1, Text: "package lib"},
					{Kind: wire.DiffDel, Old: 2, Text: "func A() {}"},
					{Kind: wire.DiffAdd, New: 2, Text: "func B() {}"},
					{Kind: wire.DiffCtx, Old: 3, New: 3, Text: ""},
				}},
				{OldStart: 10, OldLines: 1, NewStart: 10, NewLines: 2, Lines: []wire.DiffLine{
					{Kind: wire.DiffCtx, Old: 10, New: 10, Text: "x"},
					{Kind: wire.DiffAdd, New: 11, Text: "y"},
				}},
			},
		},
		{
			name:   "added",
			status: wire.FileAdded,
			out:    "diff --git a/n.txt b/n.txt\nnew file mode 100644\n--- /dev/null\n+++ b/n.txt\n@@ -0,0 +1 @@\n+one\n",
			hunks: []wire.DiffHunk{{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: 1, Lines: []wire.DiffLine{
				{Kind: wire.DiffAdd, New: 1, Text: "one"},
			}}},
		},
		{
			name:   "deleted",
			status: wire.FileDeleted,
			out:    "diff --git a/n.txt b/n.txt\ndeleted file mode 100644\n--- a/n.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-one\n",
			hunks: []wire.DiffHunk{{OldStart: 1, OldLines: 1, NewStart: 0, NewLines: 0, Lines: []wire.DiffLine{
				{Kind: wire.DiffDel, Old: 1, Text: "one"},
			}}},
		},
		{
			name:    "renamed",
			status:  wire.FileRenamed,
			oldPath: "a.go",
			out:     "diff --git a/a.go b/b.go\nsimilarity index 100%\nrename from a.go\nrename to b.go\n",
			hunks:   []wire.DiffHunk{},
		},
		{
			name:   "binary",
			status: wire.FileModified,
			binary: true,
			out:    "diff --git a/i.png b/i.png\nindex 1..2 100644\nBinary files a/i.png and b/i.png differ\n",
			hunks:  []wire.DiffHunk{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := wire.FileDiff{Status: wire.FileModified}
			parseDiff(tc.out, &d)
			if d.Status != tc.status || d.OldPath != tc.oldPath || d.Binary != tc.binary || d.Truncated != tc.truncated {
				t.Fatalf("got %+v", d)
			}
			if !reflect.DeepEqual(d.Hunks, tc.hunks) {
				t.Fatalf("hunks\n%+v\n%+v", d.Hunks, tc.hunks)
			}
		})
	}
	t.Run("truncated", func(t *testing.T) {
		var d wire.FileDiff
		parseDiff(long, &d)
		if !d.Truncated || len(d.Hunks[0].Lines) != MaxDiffLines {
			t.Fatalf("truncated %v, %d lines", d.Truncated, len(d.Hunks[0].Lines))
		}
	})
}

func TestFileDiff(t *testing.T) {
	var d wire.FileDiff
	fileDiff([]byte("a\r\nb\n"), &d)
	want := []wire.DiffHunk{{NewStart: 1, NewLines: 2, Lines: []wire.DiffLine{{Kind: wire.DiffAdd, New: 1, Text: "a"}, {Kind: wire.DiffAdd, New: 2, Text: "b"}}}}
	if d.Binary || !reflect.DeepEqual(d.Hunks, want) {
		t.Fatalf("%+v", d)
	}
	d = wire.FileDiff{}
	fileDiff([]byte("PNG\x00\x01"), &d)
	if !d.Binary || len(d.Hunks) != 0 {
		t.Fatalf("%+v", d)
	}
	d = wire.FileDiff{}
	fileDiff([]byte(strings.Repeat("x\n", MaxDiffLines+1)), &d)
	if !d.Truncated || len(d.Hunks[0].Lines) != MaxDiffLines || d.Hunks[0].NewLines != MaxDiffLines+1 {
		t.Fatalf("truncated %v", d.Truncated)
	}
}

func TestExcerpt(t *testing.T) {
	d := wire.FileDiff{Hunks: []wire.DiffHunk{{Lines: []wire.DiffLine{
		{Kind: wire.DiffCtx, Old: 1, New: 1, Text: "a"},
		{Kind: wire.DiffDel, Old: 2, Text: "b"},
		{Kind: wire.DiffAdd, New: 2, Text: "B"},
		{Kind: wire.DiffAdd, New: 3, Text: "C"},
	}}}}
	for _, tc := range []struct {
		side     string
		from, to int
		want     string
	}{
		{"new", 1, 2, "a\nB"},
		{"", 2, 3, "B\nC"},
		{"old", 1, 2, "a\nb"},
		{"new", 9, 9, ""},
	} {
		if got := excerpt(d, tc.side, tc.from, tc.to); got != tc.want {
			t.Errorf("%s %d-%d: %q, want %q", tc.side, tc.from, tc.to, got, tc.want)
		}
	}
}

func TestParseNameStatus(t *testing.T) {
	out := "M\x00lib.go\x00A\x00new.go\x00D\x00old.go\x00R087\x00a.go\x00b.go\x00C100\x00x.go\x00y.go\x00T\x00link\x00"
	got := parseNameStatus("app", out)
	want := []wire.ReviewFile{
		{Repo: "app", Path: "lib.go", Status: wire.FileModified},
		{Repo: "app", Path: "new.go", Status: wire.FileAdded},
		{Repo: "app", Path: "old.go", Status: wire.FileDeleted},
		{Repo: "app", Path: "b.go", OldPath: "a.go", Status: wire.FileRenamed},
		{Repo: "app", Path: "y.go", Status: wire.FileAdded},
		{Repo: "app", Path: "link", Status: wire.FileModified},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n%+v\n%+v", got, want)
	}
}

func TestParseLog(t *testing.T) {
	log := "b2\x1f2026-11-01T10:00:00+01:00\x1fSecond\x1fBody\n\nItem: app-3, APP-4\n\x1e\n" +
		"a1\x1f2026-11-01T08:00:00Z\x1fFirst\x1fItem: APP-1\x1e\n"
	commits, newest := parseLog(log)
	want := []wire.ReviewCommit{{SHA: "b2", Subject: "Second", Items: []string{"APP-3", "APP-4"}}, {SHA: "a1", Subject: "First", Items: []string{"APP-1"}}}
	if !reflect.DeepEqual(commits, want) {
		t.Fatalf("%+v", commits)
	}
	if !newest.Equal(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatal(newest)
	}
}

func TestOrderSort(t *testing.T) {
	got := Order{"agent-binder", "oversire", "cloud", "cli"}.Sort([]string{"cli", "zeta", "oversire", "alpha", "agent-binder"})
	if fmt.Sprint(got) != "[agent-binder oversire cli alpha zeta]" {
		t.Fatal(got)
	}
	if got := Order(nil).Sort([]string{"b", "a"}); fmt.Sprint(got) != "[a b]" {
		t.Fatal(got)
	}
}

func TestValidKey(t *testing.T) {
	for k, ok := range map[string]bool{"OVERSIRE-365": true, "APP_1": true, "app-1": false, "": false, "-A": false, "A/B": false} {
		if validKey(k) != ok {
			t.Errorf("%q", k)
		}
	}
}
