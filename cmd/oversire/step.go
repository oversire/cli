package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"

	"github.com/oversire/cli/runbook"
	"github.com/oversire/cli/wire"
)

const stepUsage = "usage: oversire step done --run <id> --step <step> --verdict pass|gaps|done|skipped|blocked [--note <text>]"

// step ends a runbook step. The agent of the step runs it, through the loopback API of the daemon.
func (c *cli) step(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "done" {
		return &exitError{2, errors.New(stepUsage)}
	}
	fs := flag.NewFlagSet("step done", flag.ContinueOnError)
	fs.SetOutput(c.out)
	run := fs.String("run", "", "the runbook run id")
	step := fs.String("step", "", "the step id")
	verdict := fs.String("verdict", "", "pass or gaps for a review, done or skipped for other steps, or blocked")
	note := fs.String("note", "", "one line for the next steps (300 characters at most)")
	if err := fs.Parse(args[1:]); err != nil {
		return &exitError{2, err}
	}
	if *run == "" || *step == "" || *verdict == "" || fs.NArg() > 0 {
		return &exitError{2, errors.New(stepUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if r := []rune(*note); len(r) > runbook.MaxNote {
		*note = string(r[:runbook.MaxNote])
	}
	var r wire.RunbookRun
	path := "/v1/runbook-runs/" + url.PathEscape(*run) + "/steps/" + url.PathEscape(*step) + "/done"
	if err := d.call(ctx, "POST", path, wire.StepDone{Verdict: wire.Verdict(*verdict), Note: *note}, &r); err != nil {
		return err
	}
	name := *step
	if s := r.Runbook.Step(*step); s != nil {
		name = s.Name
	}
	next := string(r.State)
	switch {
	case r.State == wire.RunDone:
		next = "done"
	case r.Current != *step && (r.State == wire.RunRunning || r.State == wire.RunGate):
		if s := r.Runbook.Step(r.Current); s != nil {
			next = s.Name
		}
		if r.State == wire.RunGate {
			next += " (waits for approval)"
		}
	}
	fmt.Fprintf(c.out, "Step %d of %d %s: %s. Next: %s.\n", r.Runbook.Index(*step)+1, len(r.Runbook.Steps), name, *verdict, next)
	return nil
}
