//go:build !windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	visorcli "github.com/trebi-ai/agent-visor/cli"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/hooks"
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

// visorForward sends the hook calls of a visor session to the visor at socket. When the visor
// does not answer, the call goes to the spool of the visor home, the folder of the socket.
type visorForward struct{ session, socket string }

// visorHook gives the forward of the session in the env, or nil outside a visor session.
func visorHook(getenv func(string) string) hooks.Forwarder {
	f := visorForward{session: getenv(agentvisor.EnvSession), socket: getenv(agentvisor.EnvSocket)}
	if f.session == "" || f.socket == "" {
		return nil
	}
	return f
}

func (f visorForward) Forward(ctx context.Context, harness string, payload []byte) {
	call := agentvisor.HookCall{Harness: harness, Session: f.session, Payload: payload, At: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, hooks.TotalTimeout)
	defer cancel()
	c, err := agentvisor.Dial(ctx, f.socket)
	if err == nil {
		_, err = c.Hook(ctx, call)
		_ = c.Close(ctx)
	}
	// The visor read the call: a refused call does not go to the spool.
	if err == nil || errors.Is(err, agentvisor.ErrInvalid) || errors.Is(err, agentvisor.ErrNotFound) || errors.Is(err, agentvisor.ErrState) {
		return
	}
	_ = agentvisor.NewHookSpool(filepath.Dir(f.socket)).Write(call)
}

// localSessions is the oversire runtime on the visor of the daemon, in <data>/visor.
// The sessions run in holders, so they live through a restart of the daemon.
type localSessions struct {
	visor *agentvisor.Visor
	rt    *oversire.Runtime
	sock  string
	log   *slog.Logger
}

func (c *cli) openSessions(ctx context.Context, cfg config.Config, data string, reps *reports) (*localSessions, error) {
	home := filepath.Join(data, "visor")
	sock := filepath.Join(home, "visor.sock")
	v, err := agentvisor.New(ctx, agentvisor.Options{
		Home: home, Client: "oversire", ClientVersion: version, Build: version, Socket: sock,
		HolderCommand: []string{c.exe, "visor", "hold"},
		HookCommand:   []string{c.exe, "visor", "--home", home, "hook"},
		// A handover runs the agent CLI in a terminal: claude and codex resume there.
		TerminalCommand: oversire.TerminalCommand,
		Logger:          c.log.With("source", "visor"),
		// The daemon updates itself. A visor upgrade must not replace it.
		Exec: func(string, []string, []string) error {
			return errors.New("The oversire daemon updates itself. Use oversire update.")
		},
	})
	if err != nil {
		return nil, err
	}
	rt := oversire.New(oversire.Options{Runner: cfg.Device, DataDir: data, Slots: cfg.Sessions()}, v, c.cmds, reps)
	return &localSessions{visor: v, rt: rt, sock: sock, log: c.log}, nil
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

// Serve serves the visor on its socket, for the hooks and for oversire visor. Without the socket,
// the sessions of the daemon still run, so a listen error does not stop the daemon.
func (l *localSessions) Serve(ctx context.Context) error {
	ln, err := agentvisor.Listen(ctx, l.sock)
	if err != nil {
		l.log.Error("visor socket", "err", err)
		<-ctx.Done()
		return nil
	}
	return agentvisor.ServeListener(ctx, l.visor, ln)
}

// Close stops the follow of the runtime and closes the visor. The sessions keep running.
func (l *localSessions) Close(ctx context.Context) {
	l.rt.Close(ctx)
	_ = l.visor.Close(ctx)
}

// idleRuntime is the oversire runtime with no visor, for its id in the status.
func idleRuntime() runtime.Runtime { return oversire.New(oversire.Options{}, nil, nil, nil) }
