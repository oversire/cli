package worker

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oversire/cli/wire"
)

// The default limits of a Fold.
const (
	DefaultMaxText   = 4000
	DefaultTailLines = 8
)

// readTools are the tools that only read. A run of them in sequence becomes one line.
var readTools = map[string]bool{"read": true, "grep": true, "glob": true, "ls": true, "list": true, "view": true, "find": true, "search": true}

// readCommands are the first words of a shell command that only reads.
var readCommands = map[string]bool{"cat": true, "ls": true, "rg": true, "grep": true, "head": true, "tail": true, "find": true, "wc": true, "nl": true}

// Fold renders a Read as short text for an overseer: the header, the agent text up to MaxText,
// one line for each tool call, and the tail of a failed tool.
type Fold struct {
	MaxText   int // characters of one text; 0 is DefaultMaxText
	TailLines int // lines of a failed tool; 0 is DefaultTailLines

	now func() time.Time // the clock for the elapsed time; nil is time.Now
}

// WriteHeader writes the header of a worker.
func (f Fold) WriteHeader(w io.Writer, h Header) error {
	var b strings.Builder
	f.header(&b, h)
	_, err := io.WriteString(w, b.String())
	return err
}

// Write writes the header, a blank line and the turns of r.
func (f Fold) Write(w io.Writer, r Read) error {
	var b strings.Builder
	f.header(&b, r.Header)
	b.WriteByte('\n')
	f.turns(&b, r.Turns)
	_, err := io.WriteString(w, b.String())
	return err
}

func (f Fold) limits() (maxText, tail int) {
	maxText, tail = f.MaxText, f.TailLines
	if maxText <= 0 {
		maxText = DefaultMaxText
	}
	if tail <= 0 {
		tail = DefaultTailLines
	}
	return maxText, tail
}

func (f Fold) header(b *strings.Builder, h Header) {
	name := h.Key
	if name == "" {
		name = h.Session
	}
	agent := strings.TrimSpace(h.Harness + " " + h.Model)
	state := string(h.State)
	if h.Reason != "" {
		state += " (" + string(h.Reason) + ")"
	}
	if !h.Since.IsZero() {
		now := time.Now
		if f.now != nil {
			now = f.now
		}
		state += " for " + elapsed(now().Sub(h.Since))
	}
	parts := []string{name}
	if agent != "" {
		parts = append(parts, agent)
	}
	parts = append(parts, state)
	if h.First > 0 {
		parts = append(parts, fmt.Sprintf("turns %d-%d", h.First, h.Last))
	} else {
		parts = append(parts, "no new turns")
	}
	if h.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("%d tokens", h.Tokens))
	}
	b.WriteString(strings.Join(parts, " | "))
	b.WriteByte('\n')
	if h.Request != "" {
		fmt.Fprintf(b, "Open request: %s\n", h.Request)
	}
	for _, r := range h.Repos {
		fmt.Fprintf(b, "%s +%d -%d, %d files\n", r.Name, r.Added, r.Removed, r.Files)
	}
}

func elapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func (f Fold) turns(b *strings.Builder, turns []wire.SessionTurn) {
	maxText, tail := f.limits()
	for i := 0; i < len(turns); i++ {
		t := turns[i]
		switch {
		case t.Tool != nil && isRead(t) && t.Tool.Status == wire.ToolDone:
			n := 1
			for i+n < len(turns) && isRead(turns[i+n]) && turns[i+n].Tool.Status == wire.ToolDone {
				n++
			}
			if n == 1 {
				f.tool(b, t, tail)
				continue
			}
			fmt.Fprintf(b, "✓ %d reads\n", n)
			i += n - 1
		case t.Tool != nil:
			f.tool(b, t, tail)
		case t.Role == wire.RoleUser:
			fmt.Fprintf(b, "%s %s\n", userLabel(t.Origin), cut(t.Text, maxText))
		case t.Role == wire.RoleSystem:
			fmt.Fprintf(b, "[system] %s\n", cut(t.Text, maxText))
		default:
			fmt.Fprintf(b, "%s\n", cut(t.Text, maxText))
		}
	}
}

func userLabel(o wire.TurnOrigin) string {
	switch o {
	case wire.OriginOverseer:
		return "[overseer]"
	case wire.OriginSystem:
		return "[daemon]"
	}
	return "[you]"
}

func (f Fold) tool(b *strings.Builder, t wire.SessionTurn, tail int) {
	mark := "✓"
	switch t.Tool.Status {
	case wire.ToolFailed:
		mark = "✗"
	case wire.ToolRunning:
		mark = "…"
	}
	fmt.Fprintf(b, "%s %s %s", mark, t.Tool.Name, oneLine(t.Tool.Input))
	if t.Tool.Status == wire.ToolRunning {
		b.WriteString(" (running)")
	}
	b.WriteByte('\n')
	if t.Tool.Status == wire.ToolFailed {
		for _, l := range tailLines(t.Tool.Summary, tail) {
			fmt.Fprintf(b, "    %s\n", l)
		}
	}
}

func isRead(t wire.SessionTurn) bool {
	if t.Tool == nil {
		return false
	}
	name := strings.ToLower(t.Tool.Name)
	if readTools[name] {
		return true
	}
	if name == "bash" || name == "shell" || name == "exec" {
		first, _, _ := strings.Cut(strings.TrimSpace(t.Tool.Input), " ")
		return readCommands[first]
	}
	return false
}

// cut keeps the first max characters of s and tells how many it cut.
func cut(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return fmt.Sprintf("%s\n[cut: %d more characters]", string(r[:max]), len(r)-max)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func tailLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines[max(0, len(lines)-n):]
}
