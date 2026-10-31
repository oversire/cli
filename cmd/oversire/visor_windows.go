package main

import (
	"context"
	"errors"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/runtime"
)

// agent-visor does not run on Windows. The daemon there has no oversire runtime.
var errNoVisor = errors.New("The agent sessions of the daemon are not available on Windows.")

func visorCLI([]string) error { return errNoVisor }

type localSessions struct{}

func (c *cli) openSessions(context.Context, config.Config, string, *reports) (*localSessions, error) {
	return &localSessions{}, nil
}

func (*localSessions) Runtime() runtime.Runtime             { return nil }
func (*localSessions) Boot(context.Context, *reports) error { return nil }
func (*localSessions) Serve(ctx context.Context) error      { <-ctx.Done(); return nil }
func (*localSessions) Close(context.Context)                {}

func idleRuntime() runtime.Runtime { return nil }
