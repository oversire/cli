// Package migrate moves an oversire-runner install to the oversire daemon layout.
// Each step does nothing when its old part is missing or the new part exists. Thus a second run does nothing.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/service"
	"github.com/oversire/cli/update"
)

// LegacyBinary is the install path of oversire-runner.
const LegacyBinary = "/usr/local/bin/oversire-runner"

// configFiles are the files that move from the old config folder.
var configFiles = []string{"config.json", "allow.json", "orders.json", "state.json"}

// Secrets is one keychain service.
type Secrets interface {
	Get(ctx context.Context, device string) (string, error)
	Set(ctx context.Context, device, token string) error
	Delete(ctx context.Context, device string)
}

// Layout names the old and the new places.
type Layout struct {
	OldConfig, NewConfig string
	OldData, NewData     string
	Binary               string // LegacyBinary, or a test path
}

type Migrator struct {
	layout     Layout
	goos       string
	svc        *service.Manager
	old, fresh Secrets
	run        update.Runner
	log        func(msg string, args ...any)
}

// NewMigrator copies the token from old to fresh. svc is the manager of the new service.
func NewMigrator(layout Layout, goos string, svc *service.Manager, old, fresh Secrets, run update.Runner, log func(string, ...any)) *Migrator {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Migrator{layout: layout, goos: goos, svc: svc, old: old, fresh: fresh, run: run, log: log}
}

// Report tells which steps made a change. Notes hold what the user must do.
type Report struct {
	Config, Data, Token, Service, Binary bool
	Notes                                []string
}

func (r Report) Changed() bool { return r.Config || r.Data || r.Token || r.Service || r.Binary }

func (m *Migrator) Run(ctx context.Context) (Report, error) {
	var r Report
	var err error
	if r.Config, err = m.config(); err != nil {
		return r, fmt.Errorf("migrate config: %w", err)
	}
	if r.Data, err = m.data(ctx); err != nil {
		return r, fmt.Errorf("migrate data: %w", err)
	}
	r.Token = m.token(ctx)
	if r.Service, err = m.service(ctx); err != nil {
		return r, fmt.Errorf("migrate service: %w", err)
	}
	r.Binary, r.Notes = m.binary(ctx)
	return r, nil
}

// config copies the files of the old folder, then renames it to <old>.migrated.
// A migrated runner gets the roles serve and runner.
func (m *Migrator) config() (bool, error) {
	old, fresh := m.layout.OldConfig, m.layout.NewConfig
	if old == "" || old == fresh || !isDir(old) || exists(filepath.Join(fresh, "config.json")) {
		return false, nil
	}
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		return false, err
	}
	for _, f := range configFiles {
		if err := copyFile(filepath.Join(old, f), filepath.Join(fresh, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	p := config.Paths{Dir: fresh}
	cfg, ok, err := p.LoadConfig()
	if err != nil {
		return false, err
	}
	if ok {
		for _, role := range []string{config.RoleServe, config.RoleRunner} {
			if !cfg.Has(role) {
				cfg.Roles = append(cfg.Roles, role)
			}
		}
		if err := p.SaveConfig(cfg); err != nil {
			return false, err
		}
	}
	m.log("migrated config", "from", old, "to", fresh)
	return true, os.Rename(old, old+".migrated")
}

// data moves the old data folder and repairs the git worktrees in it.
func (m *Migrator) data(ctx context.Context) (bool, error) {
	old, fresh := m.layout.OldData, m.layout.NewData
	if old == "" || old == fresh || !isDir(old) || exists(fresh) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(fresh), 0o700); err != nil {
		return false, err
	}
	if err := os.Rename(old, fresh); err != nil {
		return false, err
	}
	// The layout is worktrees/<repo>/<key>. A moved worktree needs `git worktree repair`.
	wts, _ := filepath.Glob(filepath.Join(fresh, "worktrees", "*", "*", ".git"))
	for _, g := range wts {
		dir := filepath.Dir(g)
		if _, stderr, err := m.run.Run(ctx, "git", []string{"-C", dir, "worktree", "repair"}, nil); err != nil {
			m.log("worktree repair failed", "dir", dir, "err", string(stderr))
		}
	}
	m.log("migrated data", "from", old, "to", fresh)
	return true, nil
}

// token copies the hub token to the new keychain service through stdin, then deletes the old entry.
func (m *Migrator) token(ctx context.Context) bool {
	cfg, ok, err := config.Paths{Dir: m.layout.NewConfig}.LoadConfig()
	if err != nil || !ok || !cfg.Paired() || m.old == nil || m.fresh == nil {
		return false
	}
	if _, err := m.fresh.Get(ctx, cfg.Device); err == nil {
		return false
	}
	tok, err := m.old.Get(ctx, cfg.Device)
	if err != nil {
		return false
	}
	if err := m.fresh.Set(ctx, cfg.Device, tok); err != nil {
		m.log("token copy failed", "err", err)
		return false
	}
	m.old.Delete(ctx, cfg.Device)
	return true
}

// service replaces an installed oversire-runner service with the daemon service.
func (m *Migrator) service(ctx context.Context) (bool, error) {
	if m.svc == nil {
		return false, nil
	}
	legacy := m.svc.Legacy()
	if !legacy.Status(ctx).Installed {
		return false, nil
	}
	if err := legacy.Uninstall(ctx); err != nil {
		return false, err
	}
	return true, m.svc.Install(ctx)
}

// binary removes the old binary only when it is a regular file with our signature.
func (m *Migrator) binary(ctx context.Context) (bool, []string) {
	path := m.layout.Binary
	st, err := os.Lstat(path)
	if path == "" || err != nil {
		return false, nil
	}
	keep := func(why string) (bool, []string) {
		return false, []string{fmt.Sprintf("%s stays: %s. Remove it yourself when you do not use it.", path, why)}
	}
	if !st.Mode().IsRegular() {
		return keep("it is not a regular file")
	}
	if m.goos != "darwin" {
		return keep("the signature check works only on macOS")
	}
	if err := update.SignedByTeam(ctx, m.run, path); err != nil {
		return keep("it does not have the OverSire signature")
	}
	if err := os.Remove(path); err != nil {
		return keep(err.Error())
	}
	return true, nil
}

// Layouts gives the default layout of this machine. With a home override, nothing moves:
// the old binary and the old service belong to the whole machine.
func Layouts() (Layout, error) {
	if config.Home() != "" {
		return Layout{}, nil
	}
	l := Layout{Binary: LegacyBinary}
	var err error
	for _, p := range []struct {
		dst *string
		fn  func() (string, error)
	}{{&l.OldConfig, config.LegacyDir}, {&l.NewConfig, config.Dir}, {&l.OldData, config.LegacyDataDir}, {&l.NewData, config.DataDir}} {
		if *p.dst, err = p.fn(); err != nil {
			return l, err
		}
	}
	return l, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
