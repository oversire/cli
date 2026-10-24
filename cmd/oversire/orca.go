package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/oversire/cli/wire"
)

const orcaUsage = `Usage:
  oversire orca pair            read a runtime pairing link from Orca › Settings on stdin
  oversire orca status [--json] show the state of the live Orca view
  oversire orca unpair          forget the pairing
`

// orca pairs the daemon with the local Orca through the daemon API. The link goes on stdin, never in argv.
func (c *cli) orca(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(orcaUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	var rt wire.DaemonRuntime
	switch {
	case args[0] == "pair" && len(args) == 1:
		fmt.Fprintln(c.out, "Paste the Orca pairing link, then press Return:")
		line, err := bufio.NewReader(c.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := d.call(ctx, "POST", "/v1/runtimes/orca/pair", wire.RuntimePair{Link: line}, &rt); err != nil {
			return err
		}
	case args[0] == "unpair" && len(args) == 1:
		if err := d.call(ctx, "DELETE", "/v1/runtimes/orca/pair", nil, &rt); err != nil {
			return err
		}
	case args[0] == "status" && (len(args) == 1 || len(args) == 2 && args[1] == "--json"):
		var page wire.DaemonRuntimesPage
		if err := d.call(ctx, "GET", "/v1/runtimes", nil, &page); err != nil {
			return err
		}
		i := slices.IndexFunc(page.Runtimes, func(r wire.DaemonRuntime) bool { return r.ID == "orca" })
		if i < 0 {
			return errors.New("The daemon has no Orca runtime.")
		}
		rt = page.Runtimes[i]
		if len(args) == 2 {
			return json.NewEncoder(c.out).Encode(rt.Stream)
		}
	default:
		return &exitError{2, errors.New(orcaUsage)}
	}
	return c.printOrca(rt)
}

func (c *cli) printOrca(rt wire.DaemonRuntime) error {
	switch st := rt.Stream; {
	case !rt.Reachable:
		fmt.Fprintln(c.out, "Orca: not reachable.", rt.Error)
	case st == nil:
		fmt.Fprintln(c.out, "Orca: no live view.")
	case st.State == wire.StreamLive:
		fmt.Fprintln(c.out, "Orca: paired. The view is live.")
	default:
		fmt.Fprintln(c.out, "Orca: text only.", st.Reason)
	}
	return nil
}
