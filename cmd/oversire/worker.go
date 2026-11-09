package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/worker"
)

const workerUsage = `usage:
  oversire worker start <KEY> --harness H [--model M] [--dir D] [--plan FILE] [--session ID] [--budget-time 3h]
  oversire worker read <id or KEY> [--reader NAME] [--limit N] [--json]
  oversire worker wait <id or KEY> [--reader NAME] [--timeout 8m]
  oversire worker brief <id or KEY>
  oversire worker send <id or KEY> "<text>"
  oversire worker answer <id or KEY> --permission P --allow|--deny
  oversire worker answer <id or KEY> --question Q "<text>"
  oversire worker interrupt <id or KEY>
  oversire worker escalate <id or KEY> ["<question>"]
  oversire worker stop <id or KEY>
  oversire worker done --verdict done|blocked [--note "<text>"]   (the worker runs it, in its group)`

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
	case "start":
		return c.workerStart(ctx, args[1:])
	case "done":
		return c.workerDone(ctx, args[1:])
	case "escalate":
		return c.workerEscalate(ctx, args[1:])
	case "send":
		return c.workerSend(ctx, args[1:])
	case "answer":
		return c.workerAnswer(ctx, args[1:])
	case "interrupt":
		return c.workerAction(ctx, "interrupt", args[1:], "Interrupted the worker.")
	case "stop":
		return c.workerAction(ctx, "stop", args[1:], "Stopped the worker.")
	}
	return &exitError{2, errors.New(workerUsage)}
}

// Exit codes of worker wait. 0 is a wake, 3 is the end of the timeout.
const exitWaitTimeout = 3

// waitFloor is the least time between two wait calls that answer none.
var waitFloor = time.Second

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
		began := time.Now()
		if err := d.call(ctx, "GET", workerPath(ref)+"/wait?"+q.Encode(), nil, &last); err != nil {
			return err
		}
		if last.Reason != worker.WakeNone {
			break
		}
		// A call that answers none at once must not spin.
		if gap := waitFloor - time.Since(began); gap > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(gap, time.Until(deadline))):
			}
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

// workerParse parses fs with the flags and the arguments in any order. It gives the arguments.
func workerParse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, &exitError{2, err}
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// workerStart starts the worker session of an item. The overseer runs it.
func (c *cli) workerStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker start", flag.ContinueOnError)
	fs.SetOutput(c.out)
	harness := fs.String("harness", "", "the agent CLI of the worker, for example codex")
	model := fs.String("model", "", "the model of the worker")
	dir := fs.String("dir", "", "the folder of the worker; the default is the worktree group .wt/<key> of the workspace")
	plan := fs.String("plan", "", "the file with the plan of the worker")
	session := fs.String("session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "the session id of the overseer")
	budget := fs.Duration("budget-time", worker.DefaultBudget, "the time budget of the worker")
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *harness == "" || *budget <= 0 {
		return &exitError{2, errors.New(workerUsage)}
	}
	if *session == "" {
		return &exitError{2, errors.New("Give the session id of the overseer with --session, or set CLAUDE_CODE_SESSION_ID.")}
	}
	key := strings.ToUpper(pos[0])
	wd, err := c.workdir()
	if err != nil {
		return err
	}
	if c.wd == "" {
		c.wd = wd
	}
	folder := *dir
	if folder == "" {
		folder = filepath.Join(c.sweepRoot(), ".wt", strings.ToLower(key))
	} else if !filepath.IsAbs(folder) {
		folder = filepath.Join(wd, folder)
	}
	var text string
	if *plan != "" {
		file := *plan
		if !filepath.IsAbs(file) {
			file = filepath.Join(wd, file)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		text = string(b)
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	var got worker.Started
	req := worker.StartRequest{Key: key, Harness: *harness, Model: *model, Dir: folder, Plan: text, Overseer: *session, Budget: *budget}
	if err := d.call(ctx, "POST", "/v1/workers", req, &got); err != nil {
		return err
	}
	if got.Reused {
		fmt.Fprintf(c.out, "The worker %s for %s is live. You oversee it now.\n", got.Record.Session, key)
		return nil
	}
	fmt.Fprintf(c.out, "Started the worker %s for %s in %s.\n", got.Record.Session, key, folder)
	return nil
}

// workerDone reports the end of the work. The worker agent runs it in its worktree group.
func (c *cli) workerDone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker done", flag.ContinueOnError)
	fs.SetOutput(c.out)
	verdict := fs.String("verdict", "", "done or blocked")
	note := fs.String("note", "", "one line for the overseer (300 characters at most)")
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || (*verdict != "done" && *verdict != "blocked") {
		return &exitError{2, errors.New(workerUsage)}
	}
	wd, err := c.workdir()
	if err != nil {
		return err
	}
	_, name, ok := ticket.GroupOf(wd)
	if !ok {
		return &exitError{2, errors.New("Run the command in a worktree group .wt/<key>.")}
	}
	key := strings.ToUpper(name)
	if r := []rune(*note); len(r) > 300 {
		*note = string(r[:300])
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	body := map[string]string{"verdict": *verdict, "note": *note}
	if err := d.call(ctx, "POST", workerPath(key)+"/done", body, nil); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Reported %s for %s.\n", *verdict, key)
	return nil
}

// workerEscalate asks the user. With no question, the open ask of the worker goes to the user.
func (c *cli) workerEscalate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker escalate", flag.ContinueOnError)
	fs.SetOutput(c.out)
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return &exitError{2, errors.New(workerUsage)}
	}
	question := ""
	if len(pos) == 2 {
		question = pos[1]
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if err := d.call(ctx, "POST", workerPath(pos[0])+"/escalate", map[string]string{"question": question}, nil); err != nil {
		return err
	}
	if question == "" {
		fmt.Fprintln(c.out, "Showed the open ask of the worker to the user.")
	} else {
		fmt.Fprintln(c.out, "Asked the user. The answer goes to the worker.")
	}
	return nil
}

// workerSend gives text to the worker.
func (c *cli) workerSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker send", flag.ContinueOnError)
	fs.SetOutput(c.out)
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || strings.TrimSpace(pos[1]) == "" {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if err := d.call(ctx, "POST", workerPath(pos[0])+"/send", map[string]string{"text": pos[1]}, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "Sent the text to the worker.")
	return nil
}

// workerAnswer answers the open permission or question of the worker.
func (c *cli) workerAnswer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker answer", flag.ContinueOnError)
	fs.SetOutput(c.out)
	permission := fs.String("permission", "", "the id of the permission to answer")
	allow := fs.Bool("allow", false, "allow the permission")
	deny := fs.Bool("deny", false, "deny the permission")
	question := fs.String("question", "", "the id of the question to answer")
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	a := worker.Answer{Permission: *permission, Question: *question}
	switch {
	case len(pos) < 1 || (*permission == "") == (*question == ""):
		return &exitError{2, errors.New(workerUsage)}
	case *permission != "":
		if len(pos) != 1 || *allow == *deny {
			return &exitError{2, errors.New(workerUsage)}
		}
		a.Allow = allow
	default:
		if len(pos) != 2 || strings.TrimSpace(pos[1]) == "" || *allow || *deny {
			return &exitError{2, errors.New(workerUsage)}
		}
		a.Text = pos[1]
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if err := d.call(ctx, "POST", workerPath(pos[0])+"/answer", a, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "Answered the worker.")
	return nil
}

// workerAction runs a verb that takes only the worker: interrupt or stop.
func (c *cli) workerAction(ctx context.Context, verb string, args []string, done string) error {
	fs := flag.NewFlagSet("worker "+verb, flag.ContinueOnError)
	fs.SetOutput(c.out)
	pos, err := workerParse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return &exitError{2, errors.New(workerUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if err := d.call(ctx, "POST", workerPath(pos[0])+"/"+verb, nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.out, done)
	return nil
}
