package hooks

import (
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

// Settings reads and writes the hook switches in config.json of this machine.
type Settings struct{ paths config.Paths }

func NewSettings(p config.Paths) *Settings { return &Settings{paths: p} }

func (s *Settings) Get() (wire.HookSettings, error) {
	cfg, _, err := s.paths.LoadConfig()
	if err != nil {
		return wire.HookSettings{}, err
	}
	return wire.HookSettings{CIRedirect: cfg.CIRedirect()}, nil
}

// Set writes the switches and gives the new value.
func (s *Settings) Set(v wire.HookSettings) (wire.HookSettings, error) {
	cfg, _, err := s.paths.LoadConfig()
	if err != nil {
		return wire.HookSettings{}, err
	}
	if cfg.Hooks == nil {
		cfg.Hooks = &config.HooksConfig{}
	}
	cfg.Hooks.CIRedirect = &v.CIRedirect
	if err := s.paths.SaveConfig(cfg); err != nil {
		return wire.HookSettings{}, err
	}
	return v, nil
}
