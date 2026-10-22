// Package config holds the files of the daemon in <config dir>/oversire/daemon/.
// The app owns the other files in <config dir>/oversire/. The daemon never writes them.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// Roles of a daemon.
const (
	RoleServe  = "serve"  // serve clients on loopback and Tailscale
	RoleRunner = "runner" // do work orders from the hub
)

// homeEnv overrides the config folder. legacyHomeEnv is the old name; it stays for one release.
const (
	homeEnv       = "OVERSIRE_DAEMON_HOME"
	legacyHomeEnv = "OVERSIRE_RUNNER_HOME"
)

// Home gives the override folder of OVERSIRE_DAEMON_HOME, or of OVERSIRE_RUNNER_HOME.
func Home() string {
	if d := os.Getenv(homeEnv); d != "" {
		return d
	}
	return os.Getenv(legacyHomeEnv)
}

// Dir is ~/.config/oversire/daemon on macOS and Linux, %AppData%\oversire\daemon on Windows.
func Dir() (string, error) {
	if d := Home(); d != "" {
		return d, nil
	}
	return configRoot("oversire", "daemon")
}

// LegacyDir is the config folder of oversire-runner.
func LegacyDir() (string, error) { return configRoot("oversire-runner") }

func configRoot(parts ...string) (string, error) {
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(append([]string{d}, parts...)...), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".config"}, parts...)...), nil
}

// DefaultMaxSessions is the default of Config.MaxSessions.
const DefaultMaxSessions = 2

// DataDir is ~/.local/share/oversire on macOS and Linux, %LocalAppData%\oversire on Windows.
// With a home override, it is the data folder in that folder.
func DataDir() (string, error) {
	if d := Home(); d != "" {
		return filepath.Join(d, "data"), nil
	}
	return dataRoot("oversire")
}

// LegacyDataDir is the data folder of oversire-runner.
func LegacyDataDir() (string, error) { return dataRoot("oversire-runner") }

func dataRoot(name string) (string, error) {
	if runtime.GOOS == "windows" {
		d, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", name), nil
}

// ServeConfig sets the listeners of the serve role.
type ServeConfig struct {
	Port      int   `json:"port,omitempty"`      // DefaultPort when zero
	Tailscale *bool `json:"tailscale,omitempty"` // true when nil
}

// DefaultPort is the daemon port on loopback and Tailscale.
const DefaultPort = 7690

func (s ServeConfig) ListenPort() int {
	if s.Port > 0 {
		return s.Port
	}
	return DefaultPort
}

func (s ServeConfig) UseTailscale() bool { return s.Tailscale == nil || *s.Tailscale }

// Config is the setup of this daemon. The hub token is not here; package secret holds it.
type Config struct {
	Hub       string   `json:"hub,omitempty"`
	Device    string   `json:"device,omitempty"`
	Account   string   `json:"account,omitempty"`
	Name      string   `json:"name,omitempty"`
	Roles     []string `json:"roles,omitempty"`
	KeepAwake bool     `json:"keepAwake,omitempty"`
	// MaxSessions is the maximum of oversire sessions that run at the same time. Zero is DefaultMaxSessions.
	MaxSessions int `json:"maxSessions,omitempty"`
	// DataDir holds the worktrees and transcripts of the oversire runtime. Empty is DataDir().
	DataDir string       `json:"dataDir,omitempty"`
	Serve   *ServeConfig `json:"serve,omitempty"`
}

// Has tells if the config has role.
func (c Config) Has(role string) bool { return slices.Contains(c.Roles, role) }

// Paired tells if this daemon has a hub device.
func (c Config) Paired() bool { return c.Device != "" }

// Machine is Name, or the host name when Name is not set.
func (c Config) Machine() string {
	if c.Name != "" {
		return c.Name
	}
	h, _ := os.Hostname()
	return h
}

// ServeOrDefault is Serve, or the zero ServeConfig.
func (c Config) ServeOrDefault() ServeConfig {
	if c.Serve != nil {
		return *c.Serve
	}
	return ServeConfig{}
}

// Sessions is MaxSessions, or DefaultMaxSessions when it is not set.
func (c Config) Sessions() int {
	if c.MaxSessions > 0 {
		return c.MaxSessions
	}
	return DefaultMaxSessions
}

// Data is DataDir, or DataDir() when it is not set.
func (c Config) Data() (string, error) {
	if c.DataDir != "" {
		return c.DataDir, nil
	}
	return DataDir()
}

// Paths names the files in one config dir.
type Paths struct{ Dir string }

func (p Paths) Config() string  { return filepath.Join(p.Dir, "config.json") }
func (p Paths) State() string   { return filepath.Join(p.Dir, "state.json") }
func (p Paths) Allow() string   { return filepath.Join(p.Dir, "allow.json") }
func (p Paths) Journal() string { return filepath.Join(p.Dir, "orders.json") }
func (p Paths) Token() string   { return filepath.Join(p.Dir, "token") }

// LoadConfig gives ok false when the config file is missing.
func (p Paths) LoadConfig() (c Config, ok bool, err error) {
	b, err := os.ReadFile(p.Config())
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	err = json.Unmarshal(b, &c)
	return c, err == nil, err
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
