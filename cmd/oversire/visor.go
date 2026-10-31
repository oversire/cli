//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	agentvisor "github.com/trebi-ai/agent-visor"
	visorcli "github.com/trebi-ai/agent-visor/cli"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/runtime/oversire"
)

// visorHome is <data>/visor. The daemon and the command oversire visor use it.
func visorHome() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	cfg, _, err := config.Paths{Dir: dir}.LoadConfig()
	if err != nil {
		return "", err
	}
	data, err := cfg.Data()
	if err != nil {
		return "", err
	}
	return filepath.Join(data, "visor"), nil
}

// visorCLI runs the command tree of agent-visor as oversire visor. The holders and the hooks run it too.
func visorCLI(args []string) error {
	home, err := visorHome()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := visorcli.Command(visorcli.Options{Name: "oversire visor", Usage: "Show and control the agent sessions of the daemon", Home: home, Build: version})
	return cmd.Run(ctx, append([]string{"oversire visor"}, args...))
}

// localSessions is the oversire runtime on the visor of the daemon, in <data>/visor.
// The sessions run in holders, so they live through a restart of the daemon.
type localSessions struct {
	visor *agentvisor.Visor
	rt    *oversire.Runtime
	sock  string
}

func (c *cli) openSessions(ctx context.Context, cfg config.Config, data string, reps *reports) (*localSessions, error) {
	home := filepath.Join(data, "visor")
	sock := filepath.Join(home, "visor.sock")
	v, err := agentvisor.New(ctx, agentvisor.Options{
		Home: home, Client: "oversire", ClientVersion: version, Build: version, Socket: sock,
		HolderCommand: []string{c.exe, "visor", "hold"},
		HookCommand:   []string{c.exe, "visor", "--home", home, "hook"},
		Logger:        c.log.With("source", "visor"),
		// The daemon updates itself. A visor upgrade must not replace it.
		Exec: func(string, []string, []string) error {
			return errors.New("The oversire daemon updates itself. Use oversire update.")
		},
	})
	if err != nil {
		return nil, err
	}
	rt := oversire.New(oversire.Options{Runner: cfg.Device, DataDir: data, Slots: cfg.Sessions()}, v, c.cmds, reps)
	return &localSessions{visor: v, rt: rt, sock: sock}, nil
}

func (l *localSessions) Runtime() runtime.Runtime { return l.rt }

// Boot reads the sessions of the visor. Then the sinks end the sessions that the runtime does not know.
func (l *localSessions) Boot(ctx context.Context, reps *reports) error {
	if err := l.rt.Boot(ctx); err != nil {
		return err
	}
	reps.booted(l.rt.Known)
	return nil
}

// Serve serves the visor on its socket, for the hooks and for oversire visor.
func (l *localSessions) Serve(ctx context.Context) error {
	return agentvisor.Serve(ctx, l.visor, l.sock)
}

// Close stops the follow of the runtime and closes the visor. The sessions keep running.
func (l *localSessions) Close(ctx context.Context) {
	l.rt.Close(ctx)
	_ = l.visor.Close(ctx)
}

// idleRuntime is the oversire runtime with no visor, for its id in the status.
func idleRuntime() runtime.Runtime { return oversire.New(oversire.Options{}, nil, nil, nil) }
