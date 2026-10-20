package digest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oversire/runner/wire"
)

// Claude is the AgentBinder harness name of Claude Code.
const Claude = "claude"

const (
	promptChars     = 120
	closingChars    = 700
	lastPromptChars = 1500
)

// ClaudeReader reads Claude Code transcripts (JSONL) under a projects folder.
type ClaudeReader struct{ projects string }

// NewClaudeReader reads under projects, normally ~/.claude/projects.
func NewClaudeReader(projects string) *ClaudeReader { return &ClaudeReader{projects: projects} }

// Find gives the transcript file of session: the path of a file, else the newest <projects>/*/<session>.jsonl.
func (c *ClaudeReader) Find(session string) (string, error) {
	if st, err := os.Stat(session); err == nil && st.Mode().IsRegular() {
		return session, nil
	}
	if session == "" || strings.ContainsAny(session, `/\*?[`) {
		return "", fmt.Errorf("%w for session %s", ErrNoSession, session)
	}
	hits, err := filepath.Glob(filepath.Join(c.projects, "*", session+".jsonl"))
	if err != nil {
		return "", err
	}
	best, bestTime := "", int64(0)
	for _, h := range hits {
		st, err := os.Stat(h)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if t := st.ModTime().UnixNano(); best == "" || t > bestTime {
			best, bestTime = h, t
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w for session %s", ErrNoSession, session)
	}
	return best, nil
}

func (c *ClaudeReader) Digest(session string, b *wire.HistoryBudget, source string) (wire.SessionDigest, error) {
	path, err := c.Find(session)
	if err != nil {
		return wire.SessionDigest{}, err
	}
	return ReadClaudeFile(path, b, source)
}

// ReadClaudeFile reads one transcript file. The file name is the session id when no line has one.
func ReadClaudeFile(path string, b *wire.HistoryBudget, source string) (wire.SessionDigest, error) {
	f, err := os.Open(path)
	if err != nil {
		return wire.SessionDigest{}, err
	}
	defer f.Close()
	stem := filepath.Base(path)
	if i := strings.LastIndex(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	return ReadClaude(f, stem, b, source)
}

type claudeTurn struct {
	prompt      string
	text        []string
	tools       int
	interrupted bool
}

// claudeLine holds the fields that the reader uses. The flags stay loose, as JSON truthiness decides.
type claudeLine struct {
	Type             any             `json:"type"`
	IsMeta           any             `json:"isMeta"`
	IsCompactSummary any             `json:"isCompactSummary"`
	IsSidechain      any             `json:"isSidechain"`
	SessionID        any             `json:"sessionId"`
	Message          json.RawMessage `json:"message"`
}

// ReadClaude reads a transcript from r. Lines that are not JSON objects are skipped. name is the fallback session id.
func ReadClaude(r io.Reader, name string, b *wire.HistoryBudget, source string) (wire.SessionDigest, error) {
	earlier, full, maxChars := b.Values()
	var turns []*claudeTurn
	session := ""
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		raw, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 {
			turns = addClaudeLine(turns, raw, &session)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return wire.SessionDigest{}, err
		}
	}

	kept := turns[:0]
	for _, t := range turns {
		if len(t.text) > 0 {
			kept = append(kept, t)
		}
	}
	full = min(max(full, 0), len(kept))
	head, tail := kept[:len(kept)-full], kept[len(kept)-full:]
	if session == "" {
		session = name
	}
	d := wire.SessionDigest{
		Harness: Claude, SessionID: session, Source: source, Turns: len(kept),
		Earlier: []wire.EarlierTurn{}, Last: []wire.LastTurn{}, Truncated: len(head) > earlier,
	}
	if earlier > 0 {
		for _, t := range head[max(len(head)-earlier, 0):] {
			d.Earlier = append(d.Earlier, wire.EarlierTurn{
				Prompt: first(t.prompt, promptChars), Closing: first(t.text[len(t.text)-1], closingChars), Interrupted: t.interrupted,
			})
		}
	}
	for _, t := range tail {
		text := []rune(strings.Join(t.text, "\n\n"))
		if len(text) > maxChars {
			text, d.Truncated = text[len(text)-maxChars:], true
		}
		d.Last = append(d.Last, wire.LastTurn{
			Prompt: first(t.prompt, lastPromptChars), Text: string(text), Tools: t.tools, Interrupted: t.interrupted,
		})
	}
	return d, nil
}

func addClaudeLine(turns []*claudeTurn, raw []byte, session *string) []*claudeTurn {
	var e claudeLine
	if json.Unmarshal(raw, &e) != nil || truthy(e.IsSidechain) {
		return turns
	}
	// A null line decodes to an empty struct. It changes nothing.
	if s, ok := e.SessionID.(string); ok && *session == "" {
		*session = s
	}
	if p, ok := e.prompt(); ok {
		switch {
		case strings.HasPrefix(p, "[Request interrupted"):
			if len(turns) > 0 {
				turns[len(turns)-1].interrupted = true
			}
		case p != "" && !isNotice(p) && !strings.HasPrefix(p, "Caveat:"):
			turns = append(turns, &claudeTurn{prompt: p})
		}
		return turns
	}
	if e.Type != "assistant" || len(turns) == 0 {
		return turns
	}
	t := turns[len(turns)-1]
	blocks, _ := e.content().([]any)
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			if s, _ := m["text"].(string); pyStrip(s) != "" {
				t.text = append(t.text, pyStrip(s))
			}
		case "tool_use":
			t.tools++
		}
	}
	return turns
}

// prompt gives the text of a human prompt. ok is false for every other line.
func (e claudeLine) prompt() (string, bool) {
	if e.Type != "user" || truthy(e.IsMeta) || truthy(e.IsCompactSummary) {
		return "", false
	}
	switch c := e.content().(type) {
	case string:
		return pyStrip(c), true
	case []any:
		var parts []string
		for _, b := range c {
			m, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if m["type"] == "tool_result" {
				return "", false
			}
			if m["type"] == "text" {
				s, _ := m["text"].(string)
				parts = append(parts, s)
			}
		}
		return pyStrip(strings.Join(parts, "\n")), true
	}
	return "", false
}

func (e claudeLine) content() any {
	var m struct {
		Content any `json:"content"`
	}
	if len(e.Message) == 0 || json.Unmarshal(e.Message, &m) != nil {
		return nil
	}
	return m.Content
}

// isNotice is true for harness and Orca notices (task, artifact, command output). They start with a tag.
func isNotice(text string) bool {
	return strings.HasPrefix(text, "<") && !strings.HasPrefix(text, "<pasted")
}

// truthy is the truth value of a JSON value in Python.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// pyStrip is str.strip() of Python. Python also strips U+001C to U+001F.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return (r >= 0x1c && r <= 0x1f) || isSpace(r)
	})
}

func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// first keeps the first n code points of s.
func first(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
