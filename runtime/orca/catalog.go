package orca

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// Probe reads `orca status`. An error of the CLI makes the runtime not reachable.
func (o *Orca) Probe(ctx context.Context) wire.DaemonRuntime {
	d := wire.DaemonRuntime{ID: ID, Name: "Orca", Installed: o.installed()}
	if !d.Installed {
		d.Error = "The Orca CLI is not at " + o.exe + "."
		return d
	}
	var st struct {
		Runtime struct {
			Reachable  bool   `json:"reachable"`
			AppVersion string `json:"appVersion"`
		} `json:"runtime"`
	}
	if err := o.callInto(ctx, 10*time.Second, &st, "status"); err != nil {
		d.Error = err.Error()
		return d
	}
	d.Reachable, d.Version = st.Runtime.Reachable, st.Runtime.AppVersion
	if !d.Reachable {
		d.Error = "Orca is not running."
	}
	return d
}

func (o *Orca) installed() bool {
	if filepath.IsAbs(o.exe) {
		_, err := o.stat(o.exe)
		return err == nil
	}
	return true
}

func (o *Orca) Repos(ctx context.Context) ([]wire.DaemonRepo, error) {
	var r struct {
		Repos []struct {
			ID          string `json:"id"`
			Path        string `json:"path"`
			DisplayName string `json:"displayName"`
			Kind        string `json:"kind"`
		} `json:"repos"`
	}
	if err := o.callInto(ctx, 20*time.Second, &r, "repo", "list"); err != nil {
		return nil, err
	}
	out := []wire.DaemonRepo{}
	for _, d := range r.Repos {
		if d.ID == "" || d.Path == "" {
			continue
		}
		name := d.DisplayName
		if name == "" {
			name = filepath.Base(d.Path)
		}
		out = append(out, wire.DaemonRepo{ID: d.ID, Runtime: ID, Name: name, Path: d.Path, Folder: d.Kind == "folder"})
	}
	return out, nil
}

// worktree is one row of `worktree ps`.
type worktree struct {
	ID           string `json:"worktreeId"`
	Repo         string `json:"repo"`
	Path         string `json:"path"`
	DisplayName  string `json:"displayName"`
	Comment      string `json:"comment"`
	Branch       string `json:"branch"`
	IsArchived   bool   `json:"isArchived"`
	IsMain       bool   `json:"isMainWorktree"`
	LiveTerms    int    `json:"liveTerminalCount"`
	LastActivity int64  `json:"lastActivityAt"` // ms
}

func (w worktree) kind() wire.WorkspaceKind {
	switch {
	case w.IsMain:
		return wire.KindMain
	case strings.Contains(w.ID, "::workspace:"):
		return wire.KindFolder
	}
	return wire.KindWorktree
}

func (o *Orca) worktrees(ctx context.Context) ([]worktree, error) {
	var ps struct {
		Worktrees []worktree `json:"worktrees"`
	}
	if err := o.callInto(ctx, 20*time.Second, &ps, "worktree", "ps", "--limit", "500"); err != nil {
		return nil, err
	}
	return ps.Worktrees, nil
}

// Workspaces gives the cards that are not archived, each with its terminals.
func (o *Orca) Workspaces(ctx context.Context) ([]wire.DaemonWorkspace, error) {
	wts, err := o.worktrees(ctx)
	if err != nil {
		return nil, err
	}
	var list struct {
		Terminals []terminal `json:"terminals"`
	}
	if err := o.callInto(ctx, 20*time.Second, &list, "terminal", "list", "--limit", "1000"); err != nil {
		return nil, err
	}
	terms := map[string][]wire.DaemonTerminal{}
	for _, t := range list.Terminals {
		if t.Handle == "" {
			continue
		}
		terms[t.Worktree] = append(terms[t.Worktree], wire.DaemonTerminal{ID: t.Handle, Title: t.Title, Session: t.paneKey(), Orphaned: t.Orphaned})
	}
	out := []wire.DaemonWorkspace{}
	for _, w := range wts {
		if w.ID == "" || w.IsArchived {
			continue
		}
		out = append(out, wire.DaemonWorkspace{
			ID: w.ID, Runtime: ID, Repo: w.Repo, Path: w.Path, Name: w.DisplayName, Comment: w.Comment, Kind: w.kind(),
			Branch: strings.TrimPrefix(w.Branch, "refs/heads/"), LiveTerminals: w.LiveTerms,
			LastActivity: time.UnixMilli(w.LastActivity).UTC(), Terminals: terms[w.ID],
		})
	}
	return out, nil
}

// RemoveWorkspace runs `orca worktree rm`. Orca stops the terminals. A folder card closes, and the folder stays.
func (o *Orca) RemoveWorkspace(ctx context.Context, id string, force, runHooks bool) (wire.WorkspaceRemoval, error) {
	var none wire.WorkspaceRemoval
	wts, err := o.worktrees(ctx)
	if err != nil {
		return none, err
	}
	var found *worktree
	for i := range wts {
		if wts[i].ID == id && !wts[i].IsArchived {
			found = &wts[i]
		}
	}
	if found == nil {
		return none, fmt.Errorf("%w: Orca has no workspace %s", runtime.ErrNotFound, id)
	}
	if found.IsMain {
		return none, errors.New("OverSire does not remove the main checkout of a repo.")
	}
	args := []string{"worktree", "rm", "--worktree", "id:" + id}
	if force {
		args = append(args, "--force")
	}
	if runHooks {
		args = append(args, "--run-hooks")
	}
	var r struct {
		Warning  string   `json:"warning"`
		Warnings []string `json:"warnings"`
	}
	if err := o.callInto(ctx, 120*time.Second, &r, args...); err != nil {
		return none, err
	}
	if r.Warning != "" {
		r.Warnings = append(r.Warnings, r.Warning)
	}
	return wire.WorkspaceRemoval{Warning: strings.Join(r.Warnings, " ")}, nil
}

// Reveal brings the terminal to the front of the Orca window, then shows Orca.
func (o *Orca) Reveal(ctx context.Context, s wire.SessionRef) error {
	if _, err := o.call(ctx, 20*time.Second, "terminal", "switch", "--terminal", s.ID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, stderr, err := o.run.Run(ctx, "/usr/bin/open", []string{"-a", "Orca"}, nil)
	if err != nil {
		return fmt.Errorf("open Orca: %s", strings.TrimSpace(string(stderr)+" "+err.Error()))
	}
	return nil
}
