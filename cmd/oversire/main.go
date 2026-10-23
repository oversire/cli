// Command oversire is the OverSire CLI. Its daemon serves the live state of this machine to OverSire clients,
// and does work orders from the hub.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"syscall"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/migrate"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/secret"
	"github.com/oversire/cli/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `oversire is the OverSire CLI.

Usage:
  oversire daemon run                                  run the daemon (the service runs this)
  oversire daemon install [--keep-awake]               start the daemon as a service at login
  oversire daemon uninstall                            remove the service
  oversire daemon status [--json]                      show the state (short form: oversire status)
  oversire hub pair <code> [--hub URL] [--name NAME]   pair with the cloud hub as a runner
  oversire hub pair - [--hub URL] [--name NAME]        read the code from stdin
  oversire hooks install [--harness claude,codex]      add or update the OverSire agent hooks
  oversire hooks remove [--harness claude,codex]       remove only the OverSire agent hooks
  oversire hooks status [--json]                       show the hook state of each harness
  oversire group show [--json]                         show the machines and apps of the group
  oversire group invite | requests | approve <id> | deny <id>
  oversire group request-code | admit <machine> <code>
  oversire group remove <machine|deviceId> | home <machine> | rotate
  oversire join <host> <code>|-                        join the group of the member at host
  oversire digest --harness claude --session <id or file> [--markers]
                  [--source S] [--earlier N] [--full N] [--max-chars N]
                                                       print the history digest of a session
  oversire update                                      install the latest release now
  oversire version                                     show the version
`

// renamed maps the commands of oversire-runner to the new commands.
var renamed = map[string]string{
	"run":       "oversire daemon run",
	"install":   "oversire daemon install",
	"uninstall": "oversire daemon uninstall",
	"pair":      "oversire hub pair",
}

// exitError ends the program with a code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// errSilent ends the program without a message. The command printed its own.
var errSilent = errors.New("")

type cli struct {
	paths   config.Paths
	in      io.Reader
	out     io.Writer
	log     *slog.Logger
	level   *slog.LevelVar
	secrets *secret.Store
	service *service.Manager
	migrate func(context.Context) (migrate.Report, error)
	exe     string
	home    string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "hook" {
		// The hook of an agent CLI: it must be fast and always exit 0.
		_ = hookCLI().hook(context.Background(), os.Args[2:])
		os.Exit(0)
	}
	if os.Args[1] == "digest" {
		// digest also runs on hosts without a daemon setup, so it makes no config folder.
		exit(digestCLI().digest(os.Args[2:]))
	}
	c, err := newCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "oversire:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit(c.dispatch(ctx, os.Args[1:]))
}

func (c *cli) dispatch(ctx context.Context, args []string) error {
	cmd, rest := args[0], args[1:]
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch {
	case cmd == "daemon" && sub == "run":
		return c.run(ctx)
	case cmd == "daemon" && sub == "install":
		return c.install(ctx, rest[1:])
	case cmd == "daemon" && sub == "uninstall":
		return c.uninstall(ctx)
	case cmd == "daemon" && sub == "status":
		return c.status(ctx, rest[1:])
	case cmd == "status":
		return c.status(ctx, rest)
	case cmd == "hooks":
		return c.hooks(ctx, rest)
	case cmd == "group":
		return c.group(ctx, rest)
	case cmd == "join":
		return c.join(ctx, rest)
	case cmd == "hub" && sub == "pair":
		return c.pair(ctx, rest[1:])
	case cmd == "update":
		return c.update(ctx)
	case cmd == "version" || cmd == "--version" || cmd == "-v":
		fmt.Fprintln(c.out, version)
		return nil
	case cmd == "help" || cmd == "--help" || cmd == "-h":
		fmt.Fprint(c.out, usage)
		return nil
	case renamed[cmd] != "":
		return &exitError{2, fmt.Errorf("%q is now %q.", cmd, renamed[cmd])}
	}
	return &exitError{2, fmt.Errorf("unknown command %q\n\n%s", cmd, usage)}
}

func exit(err error) {
	if err == nil {
		os.Exit(0)
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(os.Stderr, "oversire:", err)
	}
	var e *exitError
	if errors.As(err, &e) {
		os.Exit(e.code)
	}
	os.Exit(1)
}

func digestCLI() *cli {
	home, _ := os.UserHomeDir()
	return &cli{out: os.Stdout, home: home}
}

func newCLI() (*cli, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths := config.Paths{Dir: dir}
	run := proc.Local{}
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	c := &cli{
		paths:   paths,
		in:      os.Stdin,
		out:     os.Stdout,
		log:     log,
		level:   level,
		secrets: secret.New(run, goruntime.GOOS, secret.Daemon, paths.Token(), exec.LookPath),
		service: service.NewManager(goruntime.GOOS, home, os.Getuid(), exe, run),
		exe:     exe,
		home:    home,
	}
	if config.Home() != "" {
		// A home override runs beside the real install. It must not change the machine service.
		return c, os.MkdirAll(dir, 0o700)
	}
	c.migrate = func(ctx context.Context) (migrate.Report, error) {
		layout, err := migrate.Layouts()
		if err != nil {
			return migrate.Report{}, err
		}
		legacyDir, _ := config.LegacyDir()
		old := secret.New(run, goruntime.GOOS, secret.Legacy, filepath.Join(legacyDir, "token"), exec.LookPath)
		m := migrate.NewMigrator(layout, goruntime.GOOS, c.service, old, c.secrets, run, log.Info)
		r, err := m.Run(ctx)
		if err == nil {
			err = os.MkdirAll(dir, 0o700)
		}
		return r, err
	}
	return c, nil
}

// setup migrates an oversire-runner install, then loads the config. A new install gets the role serve.
func (c *cli) setup(ctx context.Context) (config.Config, error) {
	if c.migrate != nil {
		r, err := c.migrate(ctx)
		if err != nil {
			return config.Config{}, err
		}
		for _, n := range r.Notes {
			c.log.Warn(n)
		}
	}
	cfg, ok, err := c.paths.LoadConfig()
	if err != nil {
		return cfg, err
	}
	if !ok {
		cfg.Roles = []string{config.RoleServe}
		if err := c.paths.SaveConfig(cfg); err != nil {
			return cfg, err
		}
	}
	if c.level != nil {
		l, err := cfg.LogLevel(os.Getenv("OVERSIRE_LOG_LEVEL"))
		if err != nil {
			c.log.Warn("log level", "err", err)
		}
		c.level.Set(l)
	}
	return cfg, nil
}
