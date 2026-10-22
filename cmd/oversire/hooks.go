package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/hooks"
	"github.com/oversire/cli/wire"
)

const hooksUsage = `Usage:
  oversire hooks install [--harness claude,codex]   add or update the OverSire hooks in the agent configs
  oversire hooks remove [--harness claude,codex]    remove only the OverSire hooks
  oversire hooks status [--json]                    show the hook state of each harness
`

func (c *cli) installer() *hooks.Installer { return hooks.NewInstaller(c.home, c.exe) }

// hooks runs `oversire hooks <install|remove|status>`.
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
		return c.printHooks(st, *asJSON)
	case "remove", "uninstall":
		if err := i.Remove(names...); err != nil {
			return err
		}
		return c.printHooks(i.Status(), *asJSON)
	case "status":
		return c.printHooks(i.Status(), *asJSON)
	}
	return &exitError{2, fmt.Errorf("unknown hooks command %q\n\n%s", sub, hooksUsage)}
}

func (c *cli) printHooks(st []wire.HarnessHooks, asJSON bool) error {
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
	port, spool := config.DefaultPort, ""
	if c.paths.Dir != "" {
		cfg, _, err := c.paths.LoadConfig()
		if err == nil {
			port = cfg.ServeOrDefault().ListenPort()
		}
		if data, err := cfg.Data(); err == nil {
			spool = hooks.SpoolPath(data)
		}
	}
	hooks.NewHook("http://127.0.0.1:"+strconv.Itoa(port), spool).Run(ctx, args[0], c.in, c.out)
	return nil
}
