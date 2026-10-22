package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oversire/cli/wire"
)

// FS reads and writes the config files of the harnesses.
type FS interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
}

// OSFS writes through a temporary file and a rename, and keeps the mode of the old file.
type OSFS struct{}

func (OSFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (OSFS) WriteFile(path string, data []byte) error {
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".oversire.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Installer adds and removes our entries in the hook configs of the harnesses. It never writes the Codex trust.
type Installer struct {
	home, exe string
	harnesses []Harness
	fs        FS
}

// NewInstaller uses OSFS. exe is the absolute path of the oversire binary that the hooks call.
func NewInstaller(home, exe string, hs ...Harness) *Installer {
	if len(hs) == 0 {
		hs = Harnesses()
	}
	return &Installer{home: home, exe: exe, harnesses: hs, fs: OSFS{}}
}

// WithFS sets the file system, for tests.
func (i *Installer) WithFS(f FS) *Installer {
	i.fs = f
	return i
}

func (i *Installer) command(harness string) string { return Command(i.exe, harness) }

// Install adds or updates our entries. No names means all harnesses. It gives the status after the change.
func (i *Installer) Install(names ...string) ([]wire.HarnessHooks, error) {
	hs, err := i.pick(names)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if err := i.edit(h, func(cfg []byte) ([]byte, error) { return h.Install(cfg, i.command) }); err != nil {
			return nil, err
		}
	}
	return i.Status(), nil
}

// Remove takes out only our entries. No names means all harnesses.
func (i *Installer) Remove(names ...string) error {
	hs, err := i.pick(names)
	if err != nil {
		return err
	}
	for _, h := range hs {
		path := h.ConfigPath(i.home)
		if _, err := i.fs.ReadFile(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := i.edit(h, h.Remove); err != nil {
			return err
		}
	}
	return nil
}

// Status gives the hook state of each harness.
func (i *Installer) Status() []wire.HarnessHooks {
	out := make([]wire.HarnessHooks, 0, len(i.harnesses))
	for _, h := range i.harnesses {
		cfg, err := i.fs.ReadFile(h.ConfigPath(i.home))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			out = append(out, wire.HarnessHooks{Name: h.Name(), Hooks: wire.HooksMissing})
			continue
		}
		var trust []byte
		if t, ok := h.(Trusting); ok {
			trust, _ = i.fs.ReadFile(t.TrustPath(i.home))
		}
		out = append(out, h.Status(cfg, trust))
	}
	return out
}

func (i *Installer) edit(h Harness, change func([]byte) ([]byte, error)) error {
	path := h.ConfigPath(i.home)
	cfg, err := i.fs.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := change(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(out, cfg) {
		return nil
	}
	return i.fs.WriteFile(path, out)
}

func (i *Installer) pick(names []string) ([]Harness, error) {
	if len(names) == 0 {
		return i.harnesses, nil
	}
	var out []Harness
	for _, n := range names {
		found := false
		for _, h := range i.harnesses {
			if h.Name() == n {
				out, found = append(out, h), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("The harness %q has no hook support.", n)
		}
	}
	return out, nil
}
