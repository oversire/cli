package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/worker"
)

const workerUsage = `usage:
  oversire worker read <id or KEY> [--reader NAME] [--limit N] [--json]
  oversire worker wait <id or KEY> [--reader NAME] [--timeout 8m]
  oversire worker brief <id or KEY>`

// worker drives the worker sessions of the daemon, through the loopback API.
func (c *cli) worker(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	switch args[0] {
	case "read":
		return c.workerRead(ctx, args[1:])
	case "wait":
		return c.workerWait(ctx, args[1:])
	case "brief":
		return c.workerBrief(ctx, args[1:])
	}
	return &exitError{2, errors.New(workerUsage)}
}

// Exit codes of worker wait. 0 is a wake, 3 is the end of the timeout.
const exitWaitTimeout = 3

// The longest timeout of worker wait. A Bash call of an agent stops after 10 min.
const (
	defaultWaitTimeout = 8 * time.Minute
	maxWaitTimeout     = 9 * time.Minute
)

// workerRef parses the flags of fs and the id of the worker, which stands before or after the flags.
func workerRef(fs *flag.FlagSet, args []string) (string, error) {
	var ref string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", &exitError{2, err}
	}
	if ref == "" && fs.NArg() == 1 {
		ref = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return "", &exitError{2, errors.New(workerUsage)}
	}
	return ref, nil
}

func workerPath(ref string) string { return "/v1/workers/" + url.PathEscape(ref) }

// workerRead prints the turns after the cursor of the reader, then moves the cursor.
// When the print fails, the cursor stays.
func (c *cli) workerRead(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker read", flag.ContinueOnError)
	fs.SetOutput(c.out)
	reader := fs.String("reader", "overseer", "the name of the reader; each reader has its own cursor")
	limit := fs.Int("limit", 0, "the most turns to read; 0 is the daemon default")
	asJSON := fs.Bool("json", false, "print the raw turns as JSON")
	ref, err := workerRef(fs, args)
	if err != nil {
		return err
	}
	if ref == "" || *reader == "" || *limit < 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	return c.showRead(ctx, d, ref, *reader, *limit, *asJSON)
}

// showRead reads the new turns, prints them and moves the cursor.
func (c *cli) showRead(ctx context.Context, d daemonClient, ref, reader string, limit int, asJSON bool) error {
	base := workerPath(ref)
	q := url.Values{"reader": {reader}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var r worker.Read
	if err := d.call(ctx, "GET", base+"/read?"+q.Encode(), nil, &r); err != nil {
		return err
	}
	var err error
	if asJSON {
		err = json.NewEncoder(c.out).Encode(r.Turns)
	} else {
		err = worker.Fold{}.Write(c.out, r)
	}
	if err != nil {
		return err
	}
	return d.call(ctx, "PUT", base+"/cursors/"+url.PathEscape(reader), worker.Cursor{Seq: r.Next}, nil)
}

// workerWait blocks until the worker needs an action, then prints the new turns as worker read
// does. The daemon holds each call for at most 25 s, so the loop calls it again. At the timeout
// it prints the header and exits with 3.
func (c *cli) workerWait(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker wait", flag.ContinueOnError)
	fs.SetOutput(c.out)
	reader := fs.String("reader", "overseer", "the name of the reader; each reader has its own cursor")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "the longest wait, up to "+maxWaitTimeout.String())
	ref, err := workerRef(fs, args)
	if err != nil {
		return err
	}
	if ref == "" || *reader == "" || *timeout <= 0 || *timeout > maxWaitTimeout {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	var last worker.Wake
	for {
		left := time.Until(deadline).Round(time.Millisecond)
		if left <= 0 {
			if err := (worker.Fold{}).WriteHeader(c.out, last.Header); err != nil {
				return err
			}
			return &exitError{exitWaitTimeout, errSilent}
		}
		q := url.Values{"max": {min(left, worker.MaxWait).String()}}
		if err := d.call(ctx, "GET", workerPath(ref)+"/wait?"+q.Encode(), nil, &last); err != nil {
			return err
		}
		if last.Reason != worker.WakeNone {
			break
		}
	}
	if last.Reason == worker.WakeStall {
		fmt.Fprintln(c.out, "Stalled: the worker made no turn for too long.")
	}
	return c.showRead(ctx, d, ref, *reader, 0, false)
}

// workerBrief prints the summary of a run for a new overseer.
func (c *cli) workerBrief(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker brief", flag.ContinueOnError)
	fs.SetOutput(c.out)
	ref, err := workerRef(fs, args)
	if err != nil {
		return err
	}
	if ref == "" {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	var b worker.Brief
	if err := d.call(ctx, "GET", workerPath(ref)+"/brief", nil, &b); err != nil {
		return err
	}
	return worker.Fold{}.WriteBrief(c.out, b)
}
