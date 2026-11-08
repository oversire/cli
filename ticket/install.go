package ticket

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// mark identifies a skill file that OverSire wrote. A file without it is not ours, so we do not change it.
const mark = "<!-- oversire-skill -->"

const description = `Make, plan, work and close tracker tickets (Plane) with full specs. Use for /oversire new|plan|work|done, and when the user says "create a ticket", "make a ticket for this", "plan OVERSIRE-12", "work SLING-40", "pick up <KEY>" or "close the ticket".`

// The thin files only load the body from the CLI, so they change very rarely.
var thin = map[string]string{
	"claude": `---
name: oversire
description: ` + description + `
argument-hint: new|plan|work|done [KEY or text]
allowed-tools: Bash(oversire ticket:*)
---
` + mark + `
!` + "`oversire ticket skill`" + `

Arguments: $ARGUMENTS
`,
	"codex": `---
name: oversire
description: ` + description + `
---
` + mark + `
First run ` + "`oversire ticket skill`" + ` and do what its output tells. The command is the text after the skill name.
`,
}

// Harnesses gives the harnesses that the installer knows.
func Harnesses() []string { return []string{"claude", "codex"} }

type Installed struct {
	Harness string `json:"harness"`
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
}

// Installer writes the thin skill file of each harness.
type Installer struct{ home string }

func NewInstaller(home string) *Installer { return &Installer{home: home} }

func (i *Installer) path(harness string) string {
	return filepath.Join(i.home, "."+harness, "skills", "oversire", "SKILL.md")
}

// Install writes the file of each harness. No harness: claude.
func (i *Installer) Install(harnesses ...string) ([]Installed, error) {
	if len(harnesses) == 0 {
		harnesses = []string{"claude"}
	}
	var out []Installed
	for _, h := range harnesses {
		if _, ok := thin[h]; !ok {
			return out, fmt.Errorf("unknown harness %q (one of claude, codex)", h)
		}
		r, err := i.write(h, true)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Refresh rewrites only the files that exist, carry our mark and differ from the current text.
func (i *Installer) Refresh() ([]Installed, error) {
	var out []Installed
	for _, h := range Harnesses() {
		r, err := i.write(h, false)
		if err != nil {
			return out, err
		}
		if r.Path != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func (i *Installer) write(harness string, create bool) (Installed, error) {
	p, want := i.path(harness), []byte(thin[harness])
	old, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return Installed{}, nil
		}
	case err != nil:
		return Installed{}, err
	case !bytes.Contains(old, []byte(mark)):
		return Installed{}, fmt.Errorf("%s is not an OverSire skill file; move it away first", p)
	case bytes.Equal(old, want):
		return Installed{Harness: harness, Path: p}, nil
	}
	if err := writeAtomic(p, want); err != nil {
		return Installed{}, err
	}
	return Installed{Harness: harness, Path: p, Changed: true}, nil
}

func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".oversire.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
