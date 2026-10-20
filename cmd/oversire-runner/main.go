// Command oversire-runner connects this machine to the OverSire hub and drives agent runtimes on it.
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

	"github.com/oversire/runner/config"
	"github.com/oversire/runner/proc"
	"github.com/oversire/runner/secret"
	"github.com/oversire/runner/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `oversire-runner connects this machine to the OverSire hub.

Usage:
  oversire-runner pair <code> [--hub URL] [--name NAME]   pair with a runner code from the console
  oversire-runner pair - [--hub URL] [--name NAME]        read the code from stdin
  oversire-runner run                                     connect and take orders
  oversire-runner status [--json]                         show the state
  oversire-runner install [--keep-awake]                  start run as a service at login
  oversire-runner uninstall                               remove the service
  oversire-runner digest --harness claude --session <id or file> [--markers]
                         [--source S] [--earlier N] [--full N] [--max-chars N]
                                                          print the history digest of a session
  oversire-runner version                                 show the version
`

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
	secrets *secret.Store
	service *service.Manager
	exe     string
	home    string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "digest" {
		// digest also runs on hosts without a runner setup, so it makes no config folder.
		exit(digestCLI().digest(os.Args[2:]))
	}
	c, err := newCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "oversire-runner:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[2:]
	switch os.Args[1] {
	case "run":
		err = c.run(ctx)
	case "pair":
		err = c.pair(ctx, args)
	case "status":
		err = c.status(ctx, args)
	case "install":
		err = c.install(ctx, args)
	case "uninstall":
		err = c.uninstall(ctx)
	case "version", "--version", "-v":
		fmt.Fprintln(c.out, version)
	case "help", "--help", "-h":
		fmt.Fprint(c.out, usage)
	default:
		err = &exitError{2, fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)}
	}
	exit(err)
}

func exit(err error) {
	if err == nil {
		os.Exit(0)
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(os.Stderr, "oversire-runner:", err)
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	return &cli{
		paths:   paths,
		in:      os.Stdin,
		out:     os.Stdout,
		log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		secrets: secret.New(run, goruntime.GOOS, paths.Token(), exec.LookPath),
		service: service.NewManager(goruntime.GOOS, home, os.Getuid(), exe, run),
		exe:     exe,
		home:    home,
	}, nil
}

func (c *cli) mustConfig() (config.Config, error) {
	cfg, ok, err := c.paths.LoadConfig()
	if err != nil {
		return cfg, err
	}
	if !ok {
		return cfg, &exitError{2, errors.New("This runner is not paired. Make a runner code in the console, then run `oversire-runner pair <code> --hub <url>`.")}
	}
	return cfg, nil
}
