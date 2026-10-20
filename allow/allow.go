// Package allow holds the local allow list. The runner checks each order against it.
package allow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oversire/runner/wire"
)

// List is the content of allow.json.
type List struct {
	Runtimes     []string `json:"runtimes"`
	Repos        []string `json:"repos"`
	Agents       []string `json:"agents"`
	Environments []string `json:"environments"`
}

// Check gives an error that tells why the list refuses o, else nil.
func (l List) Check(o wire.WorkOrder) error {
	if !slices.Contains(l.Runtimes, o.Runtime) {
		return fmt.Errorf("The allow list does not have the runtime %s.", o.Runtime)
	}
	switch p := o.Payload.(type) {
	case wire.LaunchRequest:
		if !slices.Contains(l.Agents, p.Agent) {
			return fmt.Errorf("The allow list does not have the agent %s.", p.Agent)
		}
		if !l.hasRepo(p.Repo) {
			return fmt.Errorf("The allow list has no repo %s.", p.Repo)
		}
		return l.checkEnvironment(environment(p.Environment))
	case wire.StopPayload:
		return l.checkEnvironment(p.Session.Workspace.Account.ID)
	case wire.ReadPayload:
		return l.checkEnvironment(p.Session.Workspace.Account.ID)
	case wire.SendPayload:
		return l.checkEnvironment(p.Session.Workspace.Account.ID)
	case wire.HistoryPayload:
		return l.checkEnvironment(p.Session.Workspace.Account.ID)
	}
	return fmt.Errorf("The allow list does not know this order.")
}

func (l List) checkEnvironment(env string) error {
	if !slices.Contains(l.Environments, env) {
		return fmt.Errorf("The allow list does not have the environment %s.", env)
	}
	return nil
}

// hasRepo matches the exact path or a listed parent folder. A remote URL must match exactly.
func (l List) hasRepo(repo string) bool {
	if slices.Contains(l.Repos, repo) {
		return true
	}
	if !filepath.IsAbs(repo) {
		return false
	}
	repo = filepath.Clean(repo)
	for _, r := range l.Repos {
		if !filepath.IsAbs(r) {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(r), repo)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Summary is the part of the list that the hub shows.
func (l List) Summary() wire.AllowSummary {
	return wire.AllowSummary{Runtimes: nonNil(l.Runtimes), Repos: len(l.Repos), Agents: nonNil(l.Agents)}
}

func environment(env string) string {
	if env == "" {
		return "local"
	}
	return env
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// File reads allow.json again for each check, so an edit applies without a restart.
type File struct{ Path string }

func (f File) Load() (List, error) {
	var l List
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("The allow list %s does not exist. Make it to allow orders.", f.Path)
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("The allow list %s is not valid JSON: %v", f.Path, err)
	}
	return l, nil
}

func (f File) Check(o wire.WorkOrder) error {
	l, err := f.Load()
	if err != nil {
		return err
	}
	return l.Check(o)
}

// Summary is empty when the file is missing or bad.
func (f File) Summary() wire.AllowSummary {
	l, _ := f.Load()
	return l.Summary()
}
