package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/service"
)

type memSecrets map[string]string

func (m memSecrets) Get(_ context.Context, d string) (string, error) {
	if t, ok := m[d]; ok {
		return t, nil
	}
	return "", errors.New("missing")
}
func (m memSecrets) Set(_ context.Context, d, t string) error { m[d] = t; return nil }
func (m memSecrets) Delete(_ context.Context, d string)       { delete(m, d) }

type fakeRun struct {
	calls [][]string
	team  string
}

func (f *fakeRun) Run(_ context.Context, exe string, args []string, _ []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{exe}, args...))
	if exe == "/usr/bin/codesign" && slices.Contains(args, "-dv") {
		return nil, []byte("TeamIdentifier=" + f.team + "\n"), nil
	}
	if exe == "systemctl" && slices.Contains(args, "is-active") {
		return []byte("inactive"), nil, nil
	}
	return nil, nil, nil
}

type fixture struct {
	home        string
	layout      Layout
	old, fresh  memSecrets
	run         *fakeRun
	svc         *service.Manager
	appConfig   string
	appContents string
}

func setup(t *testing.T) *fixture {
	home := t.TempDir()
	f := &fixture{
		home: home,
		layout: Layout{
			OldConfig: filepath.Join(home, ".config", "oversire-runner"),
			NewConfig: filepath.Join(home, ".config", "oversire", "daemon"),
			OldData:   filepath.Join(home, ".local", "share", "oversire-runner"),
			NewData:   filepath.Join(home, ".local", "share", "oversire"),
			Binary:    filepath.Join(home, "bin", "oversire-runner"),
		},
		old: memSecrets{}, fresh: memSecrets{}, run: &fakeRun{team: "Q56WK6TB88"},
		appConfig: filepath.Join(home, ".config", "oversire", "config.json"), appContents: `{"app":true}`,
	}
	f.svc = service.NewManager("linux", home, 1000, "/usr/local/bin/oversire", f.run)
	write(t, f.appConfig, f.appContents)
	return f
}

func (f *fixture) oldLayout(t *testing.T) {
	write(t, filepath.Join(f.layout.OldConfig, "config.json"), `{"hub":"https://hub","device":"DEV1","name":"mini"}`)
	write(t, filepath.Join(f.layout.OldConfig, "allow.json"), `{"repos":[]}`)
	write(t, filepath.Join(f.layout.OldConfig, "orders.json"), `{}`)
	write(t, filepath.Join(f.layout.OldData, "worktrees", "app", "OS-1", ".git"), "gitdir: x")
	write(t, filepath.Join(f.home, ".config", "systemd", "user", service.Legacy.Unit), "[Unit]")
	write(t, f.layout.Binary, "bin")
	f.old["DEV1"] = "tok"
}

func (f *fixture) migrator(goos string) *Migrator {
	return NewMigrator(f.layout, goos, f.svc, f.old, f.fresh, f.run, nil)
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateOldLayout(t *testing.T) {
	f := setup(t)
	f.oldLayout(t)
	r, err := f.migrator("linux").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Config || !r.Data || !r.Token || !r.Service || r.Binary {
		t.Fatalf("report %+v", r)
	}
	cfg, ok, err := config.Paths{Dir: f.layout.NewConfig}.LoadConfig()
	if err != nil || !ok || cfg.Device != "DEV1" || !cfg.Has(config.RoleServe) || !cfg.Has(config.RoleRunner) {
		t.Fatalf("config %+v %v %v", cfg, ok, err)
	}
	for _, name := range []string{"allow.json", "orders.json"} {
		if _, err := os.Stat(filepath.Join(f.layout.NewConfig, name)); err != nil {
			t.Error(name, err)
		}
	}
	if _, err := os.Stat(f.layout.OldConfig + ".migrated"); err != nil {
		t.Fatal("old config not renamed")
	}
	if _, err := os.Stat(filepath.Join(f.layout.NewData, "worktrees", "app", "OS-1", ".git")); err != nil {
		t.Fatal("data not moved")
	}
	if f.fresh["DEV1"] != "tok" || f.old["DEV1"] != "" {
		t.Fatalf("token old %v new %v", f.old, f.fresh)
	}
	if !hasCall(f.run, "git", "worktree", "repair") || !hasCall(f.run, "systemctl", "disable", service.Legacy.Unit) || !hasCall(f.run, "systemctl", "enable", service.Current.Unit) {
		t.Fatalf("calls %q", f.run.calls)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "macOS") }) {
		t.Fatalf("notes %q", r.Notes)
	}
	if b, _ := os.ReadFile(f.appConfig); string(b) != f.appContents {
		t.Fatal("the app config changed")
	}

	f.run.calls = nil
	r, err = f.migrator("linux").Run(context.Background())
	if err != nil || r.Changed() {
		t.Fatalf("second run %+v %v", r, err)
	}
}

func TestMigrateNothing(t *testing.T) {
	f := setup(t)
	r, err := f.migrator("darwin").Run(context.Background())
	if err != nil || r.Changed() || len(r.Notes) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(f.appConfig); string(b) != f.appContents {
		t.Fatal("the app config changed")
	}
}

func TestMigrateBinary(t *testing.T) {
	for _, c := range []struct {
		name    string
		team    string
		link    bool
		removed bool
	}{{"signed", "Q56WK6TB88", false, true}, {"other team", "XXXX", false, false}, {"symlink", "Q56WK6TB88", true, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.run.team = c.team
			if c.link {
				target := filepath.Join(f.home, "real")
				write(t, target, "bin")
				os.MkdirAll(filepath.Dir(f.layout.Binary), 0o700)
				if err := os.Symlink(target, f.layout.Binary); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, f.layout.Binary, "bin")
			}
			r, _ := f.migrator("darwin").Run(context.Background())
			_, err := os.Lstat(f.layout.Binary)
			if r.Binary != c.removed || (err == nil) == c.removed || (len(r.Notes) == 0) == !c.removed {
				t.Fatalf("report %+v, stat %v", r, err)
			}
		})
	}
}

func hasCall(f *fakeRun, words ...string) bool {
	for _, c := range f.calls {
		if c[0] != words[0] {
			continue
		}
		all := true
		for _, w := range words[1:] {
			all = all && slices.Contains(c, w)
		}
		if all {
			return true
		}
	}
	return false
}
