package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
	"strconv"
	"strings"

	"github.com/oversire/cli/worker"
)

const workerUsage = `usage:
  oversire worker read <id or KEY> [--reader NAME] [--limit N] [--json]`

// worker drives the worker sessions of the daemon, through the loopback API.
func (c *cli) worker(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	switch args[0] {
	case "read":
		return c.workerRead(ctx, args[1:])
	}
	return &exitError{2, errors.New(workerUsage)}
}

// workerRead prints the turns after the cursor of the reader, then moves the cursor.
// When the print fails, the cursor stays.
func (c *cli) workerRead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker read", flag.ContinueOnError)
	fs.SetOutput(c.out)
	reader := fs.String("reader", "overseer", "the name of the reader; each reader has its own cursor")
	limit := fs.Int("limit", 0, "the most turns to read; 0 is the daemon default")
	asJSON := fs.Bool("json", false, "print the raw turns as JSON")
	var ref string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	if ref == "" && fs.NArg() == 1 {
		ref = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	if ref == "" || *reader == "" || *limit < 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	base := "/v1/workers/" + url.PathEscape(ref)
	q := url.Values{"reader": {*reader}}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	var r worker.Read
	if err := d.call(ctx, "GET", base+"/read?"+q.Encode(), nil, &r); err != nil {
		return err
	}
	if *asJSON {
		err = json.NewEncoder(c.out).Encode(r.Turns)
	} else {
		err = worker.Fold{}.Write(c.out, r)
	}
	if err != nil {
		return err
	}
	return d.call(ctx, "PUT", base+"/cursors/"+url.PathEscape(*reader), worker.Cursor{Seq: r.Next}, nil)
}
