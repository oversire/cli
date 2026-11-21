package ciredirect

import (
	"regexp"
	"strings"
)

// word is one shell word with its quotes removed.
type word struct {
	text   string
	quoted bool
}

var (
	redirectOp = regexp.MustCompile(`^[0-9]*(>>?|<|&>>?|>&|<&)`)
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	keywords   = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true, "!": true, "{": true}
)

// split cuts a command line into simple commands. It reads quotes, comments and heredocs.
// ok is false when the line does not parse, for example with an open quote.
func split(s string) (cmds [][]word, ok bool) {
	r := []rune(s)
	var cur []word
	var buf strings.Builder
	var has, quoted, wantDelim bool
	var pending []string
	flushWord := func() {
		if !has {
			return
		}
		t := buf.String()
		switch {
		case wantDelim:
			pending, wantDelim = append(pending, t), false
		case strings.HasPrefix(t, "<<") && !strings.HasPrefix(t, "<<<"):
			if d := strings.TrimPrefix(strings.TrimPrefix(t, "<<"), "-"); d != "" {
				pending = append(pending, d)
			} else {
				wantDelim = true
			}
		}
		cur = append(cur, word{t, quoted})
		buf.Reset()
		has, quoted = false, false
	}
	flushCmd := func() {
		flushWord()
		if len(cur) > 0 {
			cmds = append(cmds, cur)
		}
		cur = nil
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j == len(r) {
				return nil, false
			}
			buf.WriteString(string(r[i+1 : j]))
			has, quoted, i = true, true, j
		case c == '"':
			j := i + 1
			for ; j < len(r) && r[j] != '"'; j++ {
				if r[j] == '\\' && j+1 < len(r) && strings.ContainsRune("$`\"\\", r[j+1]) {
					j++
				}
				buf.WriteRune(r[j])
			}
			if j == len(r) {
				return nil, false
			}
			has, quoted, i = true, true, j
		case c == '\\':
			if i+1 < len(r) && r[i+1] != '\n' {
				buf.WriteRune(r[i+1])
				has = true
			}
			i++
		case c == ' ' || c == '\t':
			flushWord()
		case c == '#' && !has:
			for i+1 < len(r) && r[i+1] != '\n' {
				i++
			}
		case c == '\n':
			flushCmd()
			if len(pending) > 0 {
				i = skipHeredocs(r, i+1, pending) - 1
				pending = nil
			}
		case c == ';' || c == '|' || c == '(' || c == ')' || c == '`':
			flushCmd()
		case c == '$' && i+1 < len(r) && r[i+1] == '(':
			flushCmd()
			i++
		case c == '&':
			prev := buf.String()
			if has && (strings.HasSuffix(prev, ">") || strings.HasSuffix(prev, "<")) || i+1 < len(r) && r[i+1] == '>' {
				buf.WriteRune(c)
				has = true
			} else {
				flushCmd()
			}
		default:
			buf.WriteRune(c)
			has = true
		}
	}
	flushCmd()
	return cmds, true
}

// skipHeredocs gives the index after the bodies of the heredocs that start at from.
func skipHeredocs(r []rune, from int, delims []string) int {
	i := from
	for _, d := range delims {
		for i < len(r) {
			j := i
			for j < len(r) && r[j] != '\n' {
				j++
			}
			line := strings.TrimLeft(string(r[i:j]), "\t")
			i = min(j+1, len(r))
			if line == d {
				break
			}
		}
	}
	return i
}

// withoutRedirects drops the redirections and their targets.
func withoutRedirects(w []word) []word {
	var out []word
	for i := 0; i < len(w); i++ {
		if m := redirectOp.FindString(w[i].text); !w[i].quoted && m != "" {
			if m == w[i].text {
				i++
			}
			continue
		}
		out = append(out, w[i])
	}
	return out
}
