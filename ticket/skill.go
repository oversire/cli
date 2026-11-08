// Package ticket holds the oversire agent skill (OVERSIRE-434): its body, its install, the session ledger
// of each ticket, and the transcript pack. The body ships in the binary, so the skill and the CLI agree.
package ticket

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"text/template"
)

//go:embed skill/*.md
var files embed.FS

// Context is what the body tells the agent about this session.
type Context struct {
	Version, Machine, Cwd, Session, Harness string
	Items                                   []string // the tickets bound to the session
}

type Skill struct {
	fs   fs.FS
	tmpl *template.Template
}

func NewSkill() *Skill {
	sub, _ := fs.Sub(files, "skill")
	t := template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join}).ParseFS(sub, "*.md"))
	return &Skill{fs: sub, tmpl: t}
}

// Files gives the raw body files.
func (s *Skill) Files() fs.FS { return s.fs }

// Verbs gives the verbs that have a body. A file that starts with _ holds shared parts.
func (s *Skill) Verbs() []string {
	names, _ := fs.Glob(s.fs, "*.md")
	var out []string
	for _, n := range names {
		if v := strings.TrimSuffix(n, ".md"); v != "main" && !strings.HasPrefix(v, "_") {
			out = append(out, v)
		}
	}
	return out
}

// Body renders the main body (verb "") or the body of one verb.
func (s *Skill) Body(verb string, c Context) (string, error) {
	name := "main.md"
	if verb != "" {
		if !slices.Contains(s.Verbs(), verb) {
			return "", fmt.Errorf("unknown skill verb %q (one of %s)", verb, strings.Join(s.Verbs(), ", "))
		}
		name = path.Base(verb) + ".md"
	}
	var b bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&b, name, c); err != nil {
		return "", err
	}
	return b.String(), nil
}
