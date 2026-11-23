package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"

	"golang.org/x/term"

	"github.com/oversire/cli/link"
	"github.com/oversire/cli/wire"
)

const openUsage = "usage: oversire open <KEY|route> [--on <machine>] [--json] [--harness H --session ID]"

// open sends a page to the OverSire app on the Mac where the user is, or on the machine of --on.
func (c *cli) open(ctx context.Context, args []string) error {
	f := newAppFlags("open")
	on := f.fs.String("on", "", "the machine of the app")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 {
		return &exitError{2, errors.New(openUsage)}
	}
	p, err := link.ParseArg(pos[0])
	if err != nil {
		return &exitError{2, err}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	res, err := d.Open(ctx, wire.OpenRequest{Path: p.Route, Focus: p.Focus, On: *on, Agent: f.agent()})
	if de, ok := errors.AsType[*daemonError](err); ok && de.Status == http.StatusNotFound && de.Plain {
		return &exitError{1, errors.New("Update the daemon: oversire update")}
	}
	if err != nil {
		return &exitError{1, err}
	}
	if *f.json {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	switch {
	case res.How == wire.OpenByInput && res.Idle != nil:
		fmt.Fprintf(c.out, "Opened on %s (active %d s ago, from the %q group)\n", res.Machine, *res.Idle, res.Group)
	default:
		fmt.Fprintf(c.out, "Opened on %s\n", res.Machine)
	}
	c.printLinks(ctx, d, p)
	return nil
}

// Open posts POST /v1/open.
func (d daemonClient) Open(ctx context.Context, r wire.OpenRequest) (wire.OpenResult, error) {
	var res wire.OpenResult
	err := d.call(ctx, http.MethodPost, "/v1/open", r, &res)
	return res, err
}

// WebBase gives the base of the /go/ links of the daemon. An old daemon or an error gives "".
func (d daemonClient) WebBase(ctx context.Context) string {
	var info wire.DaemonInfo
	if err := d.call(ctx, http.MethodGet, "/v1/whoami", nil, &info); err != nil || !slices.Contains(info.Features, wire.FeatureLinks) {
		return ""
	}
	return info.Listen.Web
}

// printLinks prints the app link and the web link of p. It never fails the command.
func (c *cli) printLinks(ctx context.Context, d daemonClient, p link.Path) {
	_ = p.Print(c.out, p.Web(d.WebBase(ctx)), c.isTTY())
}

func (c *cli) isTTY() bool {
	if c.tty != nil {
		return c.tty(c.out)
	}
	f, ok := c.out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
