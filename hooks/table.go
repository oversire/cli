package hooks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Version is the shape of the hook command. Increase it when the command changes. Then Status reports outdated.
const Version = 1

// marker starts each command that we install. It finds our entries next to the entries of the user and of Orca.
const marker = ": oversire-hook="

// Command is the hook command for a harness. It calls exe by its absolute path, and only reads stdin when exe is missing.
func Command(exe, harness string) string {
	q := shellQuote(exe)
	return fmt.Sprintf("%s%d; if [ -x %s ]; then %s hook %s; else cat >/dev/null; fi", marker, Version, q, q, harness)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// commandVersion gives the marker version of a command, or ok false when it is not ours.
func commandVersion(cmd string) (v int, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(cmd), marker)
	if !found {
		return 0, false
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	v, _ = strconv.Atoi(rest[:end])
	return v, true
}

// hookEvent is one event that we hook. Matcher is a tool name pattern, or empty for all.
type hookEvent struct{ Name, Matcher string }

// table edits the hook table of a config file: hooks.<Event>[] of groups {matcher, hooks[]} of handlers {type, command, timeout}.
// Claude settings.json and Codex hooks.json have the same shape.
type table struct {
	events  []hookEvent
	timeout int // seconds
}

// place is where our handler is in the table.
type place struct {
	group, handler, version int
}

func (t table) group(e hookEvent, cmd string) (json.RawMessage, error) {
	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	}
	type group struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []handler `json:"hooks"`
	}
	return marshal(group{e.Matcher, []handler{{"command", cmd, t.timeout}}})
}

func (t table) wants(event string) (hookEvent, bool) {
	for _, e := range t.events {
		if e.Name == event {
			return e, true
		}
	}
	return hookEvent{}, false
}

// install puts our group in each event. An old group of ours is replaced in place, so a reinstall keeps the positions.
func (t table) install(cfg []byte, cmd string) ([]byte, error) {
	root, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, m := range hooks {
		e, want := t.wants(m.Key)
		groups, err := groupsOf(m.Value)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", m.Key, err)
		}
		var mine json.RawMessage
		if want {
			seen[m.Key] = true
			if mine, err = t.group(e, cmd); err != nil {
				return nil, err
			}
		}
		if hooks[i].Value, err = replaceOurs(groups, mine); err != nil {
			return nil, err
		}
	}
	for _, e := range t.events {
		if seen[e.Name] {
			continue
		}
		g, err := t.group(e, cmd)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, member{e.Name, json.RawMessage("[" + string(g) + "]")})
	}
	return t.write(root, hooks)
}

// remove takes out only our handlers. An event without groups and an empty table go away.
func (t table) remove(cfg []byte) ([]byte, error) {
	root, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	kept := object{}
	for _, m := range hooks {
		groups, err := groupsOf(m.Value)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", m.Key, err)
		}
		v, err := replaceOurs(groups, nil)
		if err != nil {
			return nil, err
		}
		if string(v) != "[]" || len(groups) == 0 {
			kept = append(kept, member{m.Key, v})
		}
	}
	return t.write(root, kept)
}

// places finds our handler in each event of the table.
func (t table) places(cfg []byte) (map[string]place, error) {
	_, hooks, err := t.read(cfg)
	if err != nil {
		return nil, err
	}
	out := map[string]place{}
	for _, m := range hooks {
		groups, err := groupsOf(m.Value)
		if err != nil {
			return nil, err
		}
		for gi, g := range groups {
			handlers, err := handlersOf(g)
			if err != nil {
				return nil, err
			}
			for hi, h := range handlers {
				if v, ok := commandVersion(h.command); ok {
					if _, dup := out[m.Key]; !dup {
						out[m.Key] = place{gi, hi, v}
					}
				}
			}
		}
	}
	return out, nil
}

// state compares the places with the events. Codex adds the trust check.
func (t table) state(p map[string]place) (installed bool, outdated bool) {
	if len(p) == 0 {
		return false, false
	}
	for _, e := range t.events {
		if pl, ok := p[e.Name]; !ok || pl.version != Version {
			return true, true
		}
	}
	for name := range p {
		if _, ok := t.wants(name); !ok {
			return true, true
		}
	}
	return true, false
}

func (t table) read(cfg []byte) (root, hooks object, err error) {
	if root, err = parseObject(cfg); err != nil {
		return nil, nil, err
	}
	raw, ok := root.get("hooks")
	if !ok || string(raw) == "null" {
		return root, object{}, nil
	}
	if hooks, err = parseObject(raw); err != nil {
		return nil, nil, fmt.Errorf("hooks: %w", err)
	}
	return root, hooks, nil
}

func (t table) write(root, hooks object) ([]byte, error) {
	if len(hooks) == 0 {
		return root.without("hooks").file()
	}
	raw, err := hooks.compact()
	if err != nil {
		return nil, err
	}
	return root.set("hooks", raw).file()
}

func groupsOf(raw json.RawMessage) ([]json.RawMessage, error) {
	var groups []json.RawMessage
	if string(raw) == "null" {
		return nil, nil
	}
	err := json.Unmarshal(raw, &groups)
	return groups, err
}

type handlerRef struct {
	raw     json.RawMessage
	command string
}

func handlersOf(group json.RawMessage) ([]handlerRef, error) {
	g, err := parseObject(group)
	if err != nil {
		return nil, err
	}
	raw, ok := g.get("hooks")
	if !ok {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]handlerRef, 0, len(list))
	for _, h := range list {
		var c struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(h, &c)
		out = append(out, handlerRef{h, c.Command})
	}
	return out, nil
}

// replaceOurs takes our handlers out of the groups. mine, when set, goes at the place of our first group, else at the end.
// A group of the user that also holds our handler keeps its other handlers.
func replaceOurs(groups []json.RawMessage, mine json.RawMessage) (json.RawMessage, error) {
	out := []json.RawMessage{}
	placed := false
	for _, g := range groups {
		handlers, err := handlersOf(g)
		if err != nil {
			return nil, err
		}
		var rest []json.RawMessage
		ours := false
		for _, h := range handlers {
			if _, ok := commandVersion(h.command); ok {
				ours = true
			} else {
				rest = append(rest, h.raw)
			}
		}
		if !ours {
			out = append(out, g)
			continue
		}
		if mine != nil && !placed {
			out = append(out, mine)
			placed = true
		}
		if len(rest) > 0 {
			obj, err := parseObject(g)
			if err != nil {
				return nil, err
			}
			list, err := marshal(rest)
			if err != nil {
				return nil, err
			}
			c, err := obj.set("hooks", list).compact()
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	if mine != nil && !placed {
		out = append(out, mine)
	}
	return marshal(out)
}
