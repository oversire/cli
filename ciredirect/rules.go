// Package ciredirect finds CI polls of gh and glab in a shell command, and names the oversire ci command that replaces them.
package ciredirect

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Escape is the env prefix that lets a command pass.
const Escape = "OVERSIRE_CI_REDIRECT=0"

// Redirect is one refused command.
type Redirect struct {
	Command     string // the matched simple command, for example "gh run watch 18233412"
	Replacement string // the oversire ci command, for example "oversire ci wait 18233412"
	Reason      string // the full text for the agent
}

// Rules is the rule set. It has no state.
type Rules struct{}

var wrappers = map[string]bool{"env": true, "command": true, "exec": true, "time": true, "nohup": true}

var (
	pullURL    = regexp.MustCompile(`/pull/([0-9]+)`)
	safeWord   = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	digitsOnly = regexp.MustCompile(`^[0-9]+$`)
)

// Match finds the CI polls in a command line. It names the first one. A line that does not parse never matches.
func (Rules) Match(command string) (Redirect, bool) {
	cmds, ok := split(command)
	if !ok {
		return Redirect{}, false
	}
	var found []Redirect
	for _, c := range cmds {
		if r, ok := matchSimple(withoutRedirects(c)); ok {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		return Redirect{}, false
	}
	first := found[0]
	first.Reason = reason(first.Replacement, len(found)-1)
	return first, true
}

func reason(replacement string, more int) string {
	var b strings.Builder
	b.WriteString("OverSire refused this command. Use: " + replacement + "\n")
	if more > 0 {
		fmt.Fprintf(&b, "The command line has %d more CI polls. Replace them in the same way.\n", more)
	}
	b.WriteString("OverSire watches CI for all agents and keeps the GitHub quota.\n")
	b.WriteString("If oversire ci cannot help (exit 3, or the repo is not tracked), run the command again with " + Escape + " in front.")
	return b.String()
}

func matchSimple(w []word) (Redirect, bool) {
	for len(w) > 0 {
		t := w[0].text
		switch {
		case t == Escape:
			return Redirect{}, false
		case assignment.MatchString(t) || keywords[t]:
			w = w[1:]
		case wrappers[t] && !w[0].quoted:
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0].text, "-") {
				w = w[1:]
			}
		default:
			return matchProgram(w)
		}
	}
	return Redirect{}, false
}

func matchProgram(w []word) (Redirect, bool) {
	args := make([]string, len(w)-1)
	for i, a := range w[1:] {
		args[i] = a.text
	}
	var out []string
	switch path.Base(w[0].text) {
	case "gh":
		out = ghRule(args)
	case "glab":
		out = glabRule(args)
	}
	if out == nil {
		return Redirect{}, false
	}
	cmd := make([]string, len(w))
	for i, x := range w {
		cmd[i] = quote(x.text)
	}
	cmd[0] = path.Base(w[0].text)
	return Redirect{Command: strings.Join(cmd, " "), Replacement: "oversire ci " + strings.Join(out, " ")}, true
}

// quote quotes a word for a shell when it needs it.
func quote(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// flagDef names a flag of gh or glab. key is the name that the rule reads. value is true when the flag takes a value.
type flagDef struct {
	key   string
	value bool
	names []string
}

type flagSet map[string]flagDef

func newFlagSet(defs ...flagDef) flagSet {
	fs := flagSet{}
	for _, d := range defs {
		for _, n := range d.names {
			fs[n] = d
		}
	}
	return fs
}

type opt struct{ key, value string }

// parsed is the positional words and the known flags of a command, in input order.
type parsed struct {
	pos  []string
	opts []opt
}

func (p parsed) has(key string) bool { _, ok := p.get(key); return ok }

func (p parsed) get(key string) (string, bool) {
	for _, o := range p.opts {
		if o.key == key {
			return o.value, true
		}
	}
	return "", false
}

func (p parsed) first() string {
	if len(p.pos) > 0 {
		return p.pos[0]
	}
	return ""
}

// parse reads args. An unknown flag counts as a flag with no value.
func (fs flagSet) parse(args []string) parsed {
	var p parsed
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.pos = append(p.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			p.pos = append(p.pos, a)
			continue
		}
		name, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			name, val, hasVal = strings.Cut(a, "=")
		} else if len(a) > 2 {
			if _, ok := fs[a[:2]]; ok && fs[a[:2]].value {
				name, val, hasVal = a[:2], a[2:], true
			}
		}
		d, known := fs[name]
		if !known {
			continue
		}
		if d.value && !hasVal && i+1 < len(args) {
			i++
			val = args[i]
		}
		p.opts = append(p.opts, opt{d.key, val})
	}
	return p
}

var (
	repoDef = flagDef{"repo", true, []string{"-R", "--repo"}}

	watchFlags = newFlagSet(repoDef, flagDef{"interval", true, []string{"-i", "--interval"}})
	viewFlags  = newFlagSet(repoDef,
		flagDef{"job", true, []string{"-j", "--job"}},
		flagDef{"log", false, []string{"--log"}},
		flagDef{"log-failed", false, []string{"--log-failed"}},
		flagDef{"json", true, []string{"--json"}},
		flagDef{"web", false, []string{"-w", "--web"}},
		flagDef{"attempt", true, []string{"-a", "--attempt"}},
		flagDef{"jq", true, []string{"-q", "--jq"}},
		flagDef{"template", true, []string{"-t", "--template"}})
	listFlags = newFlagSet(repoDef,
		flagDef{"branch", true, []string{"-b", "--branch"}},
		flagDef{"workflow", true, []string{"-w", "--workflow"}},
		flagDef{"limit", true, []string{"-L", "--limit"}},
		flagDef{"sha", true, []string{"-c", "--commit"}},
		flagDef{"state", true, []string{"-s", "--status"}},
		flagDef{"json", true, []string{"--json"}},
		flagDef{"event", true, []string{"-e", "--event"}},
		flagDef{"user", true, []string{"-u", "--user"}},
		flagDef{"created", true, []string{"--created"}},
		flagDef{"jq", true, []string{"-q", "--jq"}},
		flagDef{"template", true, []string{"-t", "--template"}})
	checksFlags = newFlagSet(repoDef,
		flagDef{"watch", false, []string{"--watch"}},
		flagDef{"fail-fast", false, []string{"--fail-fast"}},
		flagDef{"interval", true, []string{"-i", "--interval"}},
		flagDef{"web", false, []string{"-w", "--web"}},
		flagDef{"json", true, []string{"--json"}},
		flagDef{"jq", true, []string{"-q", "--jq"}},
		flagDef{"template", true, []string{"-t", "--template"}})
	glabFlags = newFlagSet(repoDef,
		flagDef{"branch", true, []string{"-b", "--branch"}},
		flagDef{"live", false, []string{"-l", "--live"}},
		flagDef{"web", false, []string{"-w", "--web"}},
		flagDef{"pipeline", true, []string{"-p", "--pipeline-id"}},
		flagDef{"output", true, []string{"-F", "--output"}})
)

// states maps the gh --status values to the CIRollupState values. Other values drop.
var states = map[string]string{"failure": "failure", "in_progress": "running", "queued": "pending", "success": "success", "cancelled": "cancelled"}

// ghRule gives the arguments of the oversire ci command for a gh command, or nil.
func ghRule(args []string) []string {
	if len(args) < 2 {
		return nil
	}
	switch args[0] + " " + args[1] {
	case "run watch":
		p := watchFlags.parse(args[2:])
		return withRepo(p, "wait", p.first())
	case "run view":
		return ghRunView(viewFlags.parse(args[2:]))
	case "run list":
		return ghRunList(listFlags.parse(args[2:]))
	case "pr checks":
		return ghPRChecks(checksFlags.parse(args[2:]))
	}
	return nil
}

// withRepo builds `<verb> [ref] [--repo X]`.
func withRepo(p parsed, verb string, ref string) []string {
	out := []string{verb}
	if ref != "" {
		out = append(out, quote(ref))
	}
	if v, ok := p.get("repo"); ok {
		out = append(out, "--repo", quote(v))
	}
	return out
}

func ghRunView(p parsed) []string {
	if p.has("web") {
		return nil
	}
	run := p.first()
	var out []string
	switch job, hasJob := p.get("job"); {
	case hasJob:
		out = withRepo(p, "logs", job)
		if p.has("log-failed") {
			out = append(out, "--failed")
		}
	case run == "":
		out = withRepo(p, "status", "")
	case p.has("log-failed"):
		out = withRepo(p, "why", run)
	case p.has("log"):
		out = withRepo(p, "logs", run)
	default:
		out = withRepo(p, "show", run)
		if v, ok := p.get("attempt"); ok && digitsOnly.MatchString(v) {
			out = append(out, "--attempt", v)
		}
	}
	if p.has("json") {
		out = append(out, "--json")
	}
	return out
}

func ghRunList(p parsed) []string {
	out := []string{"ls"}
	for _, o := range p.opts {
		switch o.key {
		case "branch", "workflow", "sha", "repo":
			out = append(out, "--"+o.key, quote(o.value))
		case "limit":
			if digitsOnly.MatchString(o.value) {
				out = append(out, "--limit", o.value)
			}
		case "state":
			if s, ok := states[o.value]; ok {
				out = append(out, "--state", s)
			}
		case "json":
			out = append(out, "--json")
		}
	}
	return out
}

func ghPRChecks(p parsed) []string {
	if p.has("web") {
		return nil
	}
	verb := "status"
	if p.has("watch") {
		verb = "wait"
	}
	out := withRepo(p, verb, prTarget(p.first()))
	if verb == "wait" && p.has("fail-fast") {
		out = append(out, "--fail-fast")
	}
	if p.has("json") {
		out = append(out, "--json")
	}
	return out
}

// prTarget turns a PR number or URL into #N. A branch stays a branch.
func prTarget(s string) string {
	if digitsOnly.MatchString(s) {
		return "#" + s
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		if m := pullURL.FindStringSubmatch(u.Path); m != nil {
			return "#" + m[1]
		}
	}
	return s
}

// glabRule gives the arguments of the oversire ci command for a glab command, or nil.
// The aliases pipe and pipeline are not checked.
func glabRule(args []string) []string {
	if len(args) < 2 || args[0] != "ci" && args[0] != "pipe" && args[0] != "pipeline" || args[1] != "status" && args[1] != "view" {
		return nil
	}
	p := glabFlags.parse(args[2:])
	if p.has("web") {
		return nil
	}
	branch, _ := p.get("branch")
	if branch == "" {
		branch = p.first()
	}
	verb := "status"
	if p.has("live") {
		verb = "wait"
	}
	return withRepo(p, verb, branch)
}
