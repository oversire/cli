// Package config holds the files of the runner in <config dir>/oversire-runner/.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// Dir is ~/.config/oversire-runner on macOS and Linux, %AppData%\oversire-runner on Windows.
func Dir() (string, error) {
	if d := os.Getenv("OVERSIRE_RUNNER_HOME"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "oversire-runner"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "oversire-runner"), nil
}

// Config is the pairing of this runner. The token is not here; package secret holds it.
type Config struct {
	Hub       string `json:"hub"`
	Device    string `json:"device"`
	Account   string `json:"account"`
	Name      string `json:"name"`
	KeepAwake bool   `json:"keepAwake,omitempty"`
}

// Paths names the files in one config dir.
type Paths struct{ Dir string }

func (p Paths) Config() string  { return filepath.Join(p.Dir, "config.json") }
func (p Paths) State() string   { return filepath.Join(p.Dir, "state.json") }
func (p Paths) Allow() string   { return filepath.Join(p.Dir, "allow.json") }
func (p Paths) Journal() string { return filepath.Join(p.Dir, "orders.json") }
func (p Paths) Token() string   { return filepath.Join(p.Dir, "token") }

// LoadConfig gives ok false when the runner is not paired.
func (p Paths) LoadConfig() (c Config, ok bool, err error) {
	b, err := os.ReadFile(p.Config())
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	err = json.Unmarshal(b, &c)
	return c, err == nil && c.Device != "", err
}

func (p Paths) SaveConfig(c Config) error { return WriteJSON(p.Config(), c) }

// WriteJSON writes v with mode 0600 through a temporary file and a rename.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
