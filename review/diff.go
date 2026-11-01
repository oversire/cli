package review

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/oversire/cli/wire"
)

// MaxDiffLines is the number of diff lines in a FileDiff. A longer diff is cut and has truncated set.
const MaxDiffLines = 3000

var hunkHead = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// parseDiff reads the output of git diff for one file into d: the hunks, binary, and the status from the file header.
func parseDiff(out string, d *wire.FileDiff) {
	d.Hunks = []wire.DiffHunk{}
	var h *wire.DiffHunk
	oldLine, newLine, n := 0, 0, 0
	for _, l := range strings.Split(out, "\n") {
		if h == nil || strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "diff --git ") {
			switch {
			case strings.HasPrefix(l, "@@"):
				m := hunkHead.FindStringSubmatch(l)
				if m == nil {
					continue
				}
				d.Hunks = append(d.Hunks, wire.DiffHunk{OldStart: atoi(m[1]), OldLines: count(m[2]), NewStart: atoi(m[3]), NewLines: count(m[4]),
					Header: m[5], Lines: []wire.DiffLine{}})
				h = &d.Hunks[len(d.Hunks)-1]
				oldLine, newLine = h.OldStart, h.NewStart
			case strings.HasPrefix(l, "new file mode"):
				d.Status = wire.FileAdded
			case strings.HasPrefix(l, "deleted file mode"):
				d.Status = wire.FileDeleted
			case strings.HasPrefix(l, "rename from "):
				d.Status, d.OldPath = wire.FileRenamed, strings.TrimPrefix(l, "rename from ")
			case strings.HasPrefix(l, "Binary files ") || l == "GIT binary patch":
				d.Binary = true
			}
			continue
		}
		var dl wire.DiffLine
		switch {
		case strings.HasPrefix(l, "+"):
			dl = wire.DiffLine{Kind: wire.DiffAdd, New: newLine, Text: l[1:]}
			newLine++
		case strings.HasPrefix(l, "-"):
			dl = wire.DiffLine{Kind: wire.DiffDel, Old: oldLine, Text: l[1:]}
			oldLine++
		case strings.HasPrefix(l, " "):
			dl = wire.DiffLine{Kind: wire.DiffCtx, Old: oldLine, New: newLine, Text: l[1:]}
			oldLine++
			newLine++
		default:
			// "\ No newline at end of file", or the empty last line.
			continue
		}
		if n == MaxDiffLines {
			d.Truncated = true
			break
		}
		h.Lines = append(h.Lines, dl)
		n++
	}
	if d.Binary {
		d.Hunks = []wire.DiffHunk{}
	}
}

// fileDiff is the diff of a new file with the content data: one hunk of added lines.
func fileDiff(data []byte, d *wire.FileDiff) {
	d.Hunks = []wire.DiffHunk{}
	if isBinary(data) {
		d.Binary = true
		return
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if text == "" {
		return
	}
	all := strings.Split(text, "\n")
	h := wire.DiffHunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: len(all), Lines: []wire.DiffLine{}}
	for i, l := range all {
		if i == MaxDiffLines {
			d.Truncated = true
			break
		}
		h.Lines = append(h.Lines, wire.DiffLine{Kind: wire.DiffAdd, New: i + 1, Text: l})
	}
	d.Hunks = append(d.Hunks, h)
}

// isBinary is true when the first 8000 bytes have a NUL byte, as git decides.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// lineCount is the number of lines of a text.
func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// excerpt gives the diff lines of d from..to on side (old or new), one line each.
func excerpt(d wire.FileDiff, side string, from, to int) string {
	var out []string
	for _, h := range d.Hunks {
		for _, l := range h.Lines {
			n := l.New
			if side == "old" {
				n = l.Old
			}
			if n >= from && n <= to && (side == "old" && l.Kind != wire.DiffAdd || side != "old" && l.Kind != wire.DiffDel) {
				out = append(out, l.Text)
			}
		}
	}
	return strings.Join(out, "\n")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// count is the line count of a hunk range. git leaves out ",1".
func count(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}
