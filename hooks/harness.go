package hooks

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

// Harness knows the hook config and the payloads of one agent CLI. The names are the agent-wire harness names.
type Harness interface {
	Name() string
	ConfigPath(home string) string
	Install(cfg []byte, command func(harness string) string) ([]byte, error) // idempotent
	Remove(cfg []byte) ([]byte, error)
	Status(cfg []byte, codexTrust []byte) wire.HarnessHooks
	Map(ev Event) (sessions.Patch, bool)
}

// Trusting is a harness that runs a new hook only after the user trusts it. TrustPath is the file of that trust.
type Trusting interface {
	TrustPath(home string) string
}

// Harnesses gives all supported harnesses.
func Harnesses() []Harness { return []Harness{Claude{}, Codex{}} }

// Claude hooks ~/.claude/settings.json.
type Claude struct{}

var claudeTable = table{timeout: 5, events: []hookEvent{
	{Name: "SessionStart"},
	{Name: "UserPromptSubmit"},
	{Name: "PreToolUse", Matcher: "AskUserQuestion|ExitPlanMode"},
	{Name: "PostToolUse", Matcher: "AskUserQuestion|ExitPlanMode"},
	{Name: "PermissionRequest"},
	{Name: "Notification"},
	{Name: "Stop"},
	{Name: "StopFailure"},
	{Name: "SessionEnd"},
}}

var claudeMap = eventMap{harness: "claude", questions: []string{"AskUserQuestion"}, plan: "ExitPlanMode"}

func (Claude) Name() string { return "claude" }

func (Claude) ConfigPath(home string) string { return filepath.Join(home, ".claude", "settings.json") }

func (c Claude) Install(cfg []byte, command func(string) string) ([]byte, error) {
	return claudeTable.install(cfg, command(c.Name()))
}

func (Claude) Remove(cfg []byte) ([]byte, error) { return claudeTable.remove(cfg) }

func (c Claude) Status(cfg, _ []byte) wire.HarnessHooks {
	return wire.HarnessHooks{Name: c.Name(), Hooks: tableState(claudeTable, cfg, nil)}
}

func (Claude) Map(ev Event) (sessions.Patch, bool) { return claudeMap.patch(ev) }

// Codex hooks ~/.codex/hooks.json. Codex runs a new hook only after the user trusts it in Codex.
// We never write that trust. Status reports untrusted until then.
type Codex struct{}

var codexTable = table{timeout: 5, events: []hookEvent{
	{Name: "SessionStart"},
	{Name: "UserPromptSubmit"},
	{Name: "PreToolUse", Matcher: "request_user_input"},
	{Name: "PermissionRequest"},
	{Name: "Stop"},
}}

var codexMap = eventMap{harness: "codex", questions: []string{"request_user_input"}}

func (Codex) Name() string { return "codex" }

func (Codex) ConfigPath(home string) string { return filepath.Join(home, ".codex", "hooks.json") }

func (Codex) TrustPath(home string) string { return filepath.Join(home, ".codex", "config.toml") }

func (c Codex) Install(cfg []byte, command func(string) string) ([]byte, error) {
	return codexTable.install(cfg, command(c.Name()))
}

func (Codex) Remove(cfg []byte) ([]byte, error) { return codexTable.remove(cfg) }

// Status reads [hooks.state."<path>:<event>:<group>:<handler>"] trusted_hash in config.toml.
func (c Codex) Status(cfg, trust []byte) wire.HarnessHooks {
	return wire.HarnessHooks{Name: c.Name(), Hooks: tableState(codexTable, cfg, func(p map[string]place) bool {
		trusted := codexTrusted(trust)
		for name, pl := range p {
			if !trusted[fmt.Sprintf("hooks.json:%s:%d:%d", snake(name), pl.group, pl.handler)] {
				return false
			}
		}
		return true
	})}
}

func (Codex) Map(ev Event) (sessions.Patch, bool) { return codexMap.patch(ev) }

func tableState(t table, cfg []byte, trusted func(map[string]place) bool) wire.HookState {
	p, err := t.places(cfg)
	if err != nil {
		return wire.HooksOutdated
	}
	installed, outdated := t.state(p)
	switch {
	case !installed:
		return wire.HooksMissing
	case outdated:
		return wire.HooksOutdated
	case trusted != nil && !trusted(p):
		return wire.HooksUntrusted
	}
	return wire.HooksInstalled
}

// codexTrusted gives the keys of config.toml hooks.state tables with a trusted_hash and not enabled = false.
// The key keeps only the part from "hooks.json:" on, since the path in front depends on CODEX_HOME.
func codexTrusted(toml []byte) map[string]bool {
	out := map[string]bool{}
	key, hash, enabled := "", false, true
	done := func() {
		if key != "" && hash && enabled {
			out[key] = true
		}
		key, hash, enabled = "", false, true
	}
	sc := bufio.NewScanner(bytes.NewReader(toml))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			done()
			if rest, ok := strings.CutPrefix(line, `[hooks.state."`); ok {
				if k, _, ok := strings.Cut(rest, `"]`); ok {
					if i := strings.LastIndex(k, "hooks.json:"); i >= 0 {
						key = k[i:]
					}
				}
			}
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			continue
		}
		switch strings.TrimSpace(name) {
		case "trusted_hash":
			v, err := strconv.Unquote(strings.TrimSpace(value))
			hash = err == nil && v != ""
		case "enabled":
			enabled = strings.TrimSpace(value) != "false"
		}
	}
	done()
	return out
}

// snake gives the Codex key of an event: PreToolUse is pre_tool_use.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
