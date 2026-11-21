package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oversire/cli/ciredirect"
	"github.com/oversire/cli/config"
	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/wire"
)

const hooksUsage = `Usage:
  oversire hooks install [--harness claude,codex]   add or update the OverSire hooks in the agent configs
  oversire hooks remove [--harness claude,codex]    remove only the OverSire hooks
  oversire hooks status [--json]                    show the hook state of each harness
  oversire hooks ci-redirect [on|off]               show or set the refusal of gh and glab CI polls
`

func (c *cli) installer() *hooks.Installer { return hooks.NewInstaller(c.home, c.exe) }

// hooks runs `oversire hooks <install|remove|status|ci-redirect>`.
func (c *cli) hooks(_ context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, fmt.Errorf("a hooks command is missing\n\n%s", hooksUsage)}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("hooks "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	harness := fs.String("harness", "", "comma-separated harness names")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(rest); err != nil {
		return &exitError{2, fmt.Errorf("%w\n\n%s", err, hooksUsage)}
	}
	var names []string
	for _, n := range strings.Split(*harness, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	i := c.installer()
	switch sub {
	case "install":
		st, err := i.Install(names...)
		if err != nil {
			return err
		}
		return c.printHooks(st, c.hookSettings(), *asJSON)
	case "remove", "uninstall":
		if err := i.Remove(names...); err != nil {
			return err
		}
		return c.printHooks(i.Status(), c.hookSettings(), *asJSON)
	case "status":
		return c.printHooks(i.Status(), c.hookSettings(), *asJSON)
	case "ci-redirect":
		return c.ciRedirect(fs.Args())
	}
	return &exitError{2, fmt.Errorf("unknown hooks command %q\n\n%s", sub, hooksUsage)}
}

// hookSettings reads the hook switches. A config that does not load gives the defaults.
func (c *cli) hookSettings() wire.HookSettings {
	v, err := hooks.NewSettings(c.paths).Get()
	if err != nil {
		return wire.HookSettings{CIRedirect: true}
	}
	return v
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// ciRedirect shows or sets the refusal of CI polls.
func (c *cli) ciRedirect(args []string) error {
	s := hooks.NewSettings(c.paths)
	switch {
	case len(args) == 0:
		fmt.Fprintf(c.out, "CI redirect: %s\n", onOff(c.hookSettings().CIRedirect))
		return nil
	case len(args) == 1 && (args[0] == "on" || args[0] == "off"):
		v, err := s.Set(wire.HookSettings{CIRedirect: args[0] == "on"})
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "CI redirect: %s\n", onOff(v.CIRedirect))
		return nil
	}
	return &exitError{2, fmt.Errorf("use on or off\n\n%s", hooksUsage)}
}

func (c *cli) printHooks(st []wire.HarnessHooks, set wire.HookSettings, asJSON bool) error {
	if asJSON {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(c.out, string(b))
		return nil
	}
	for _, h := range st {
		fmt.Fprintf(c.out, "%-8s %s\n", h.Name, h.Hooks)
		switch h.Hooks {
		case wire.HooksUntrusted:
			fmt.Fprintf(c.out, "         Start %s and approve the new hooks when it asks. OverSire does not approve them for you.\n", h.Name)
		case wire.HooksOutdated:
			fmt.Fprintln(c.out, "         Run `oversire hooks install` to update the hooks.")
		}
	}
	fmt.Fprintf(c.out, "CI redirect: %s\n", onOff(set.CIRedirect))
	return nil
}

// hookCLI is the CLI of `oversire hook`. It makes no config folder and runs no migration, so that it stays fast.
func hookCLI() *cli {
	c := &cli{in: os.Stdin, out: os.Stdout}
	if dir, err := config.Dir(); err == nil {
		c.paths = config.Paths{Dir: dir}
	}
	return c
}

// hook is `oversire hook <harness>`, which the agent CLIs call. It always succeeds and prints only what the harness needs.
func (c *cli) hook(ctx context.Context, args []string) error {
	if len(args) == 0 {
		_, _ = io.Copy(io.Discard, c.in)
		return nil
	}
	port, spool, home := config.DefaultPort, "", ""
	redirect := true
	if c.paths.Dir != "" {
		cfg, _, err := c.paths.LoadConfig()
		if err == nil {
			port = cfg.ServeOrDefault().ListenPort()
			redirect = cfg.CIRedirect()
		}
		if data, err := cfg.Data(); err == nil {
			spool, home = hooks.SpoolPath(data), filepath.Join(data, "visor")
		}
	}
	h := hooks.NewHook("http://127.0.0.1:"+strconv.Itoa(port), spool)
	h.Visor = visorHook(os.Getenv, home)
	if redirect {
		h.Gate = ciredirect.NewGate()
	}
	h.Run(ctx, args[0], c.in, c.out)
	return nil
}
