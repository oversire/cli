//go:build !windows

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"
	vorca "github.com/trebi-ai/agent-visor/backend/orca"
	visorcli "github.com/trebi-ai/agent-visor/cli"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/runtime/orca"
	"github.com/oversire/cli/runtime/oversire"
)

// orcaPoll is the time between two reads of the Orca state by the orca backend of the visor.
const orcaPoll = 3 * time.Second

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
// A call with only labels (an Orca pane that the visor did not start) does not go to the spool.
type visorForward struct {
	session, socket string
	labels          map[string]string
}

// visorHook gives the forward of the session in the env. Outside a visor session, an Orca pane
// goes to the visor in home with its labels. Else it gives nil.
func visorHook(getenv func(string) string, home string) hooks.Forwarder {
	f := visorForward{session: getenv(agentvisor.EnvSession), socket: getenv(agentvisor.EnvSocket)}
	if f.session == "" {
		f.socket = ""
		if labels := vorca.HookLabels(getenv); len(labels) > 0 && home != "" {
			f.socket, f.labels = filepath.Join(home, "visor.sock"), labels
		}
	}
	if f.socket == "" {
		return nil
	}
	return f
}

func (f visorForward) Forward(ctx context.Context, harness string, payload []byte) {
	call := agentvisor.HookCall{Harness: harness, Session: f.session, Labels: f.labels, Payload: payload, At: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, hooks.TotalTimeout)
	defer cancel()
	c, err := agentvisor.Dial(ctx, f.socket)
	if err == nil {
		_, err = c.Hook(ctx, call)
		_ = c.Close(ctx)
	}
	// The visor read the call: a refused call does not go to the spool.
	if err == nil || f.session == "" || errors.Is(err, agentvisor.ErrInvalid) || errors.Is(err, agentvisor.ErrNotFound) || errors.Is(err, agentvisor.ErrState) {
		return
	}
	_ = agentvisor.NewHookSpool(filepath.Dir(f.socket)).Write(call)
}

// localSessions is the oversire runtime on the visor of the daemon, in <data>/visor.
// The sessions run in holders, so they live through a restart of the daemon.
type localSessions struct {
	visor  *agentvisor.Visor
	rt     *oversire.Runtime
	orca   *vorca.Backend // nil when this machine has no Orca CLI
	polled atomic.Pointer[func()]
	sock   string
	log    *slog.Logger
}

func (c *cli) openSessions(ctx context.Context, cfg config.Config, data string, reps *reports) (*localSessions, error) {
	home := filepath.Join(data, "visor")
	sock := filepath.Join(home, "visor.sock")
	l := &localSessions{sock: sock, log: c.log}
	var backends []agentvisor.Backend
	if exe, ok := orca.FindCLI(); ok {
		// The backend follows the Orca sessions of the visor and adopts the agents that the user starts in Orca.
		b, err := vorca.New(vorca.Options{
			CLI: vorca.NewCLI(exe, nil), Dir: filepath.Join(home, "orca"), Poll: orcaPoll, Discover: true,
			Logger: c.log.With("source", "visor-orca"),
			OnPoll: func() {
				if f := l.polled.Load(); f != nil {
					(*f)()
				}
			},
		})
		if err != nil {
			return nil, err
		}
		l.orca, backends = b, append(backends, b)
	}
	v, err := agentvisor.New(ctx, agentvisor.Options{
		Home: home, Client: "oversire", ClientVersion: version, Build: version, Socket: sock,
		Backends:      backends,
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
	l.visor = v
	l.rt = oversire.New(oversire.Options{Runner: cfg.Device, DataDir: data, Slots: cfg.Sessions()}, v, c.cmds, reps)
	return l, nil
}

// useOrca runs the sessions of the orca driver in the visor.
func (l *localSessions) useOrca(o *orca.Orca) {
	if l.orca != nil {
		o.WithVisor(l.visor, l.orca)
	}
}

// OnOrcaPoll sets the function that runs after each read of the Orca state.
func (l *localSessions) OnOrcaPoll(f func()) { l.polled.Store(&f) }

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
