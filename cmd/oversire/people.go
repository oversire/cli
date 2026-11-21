package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

const peopleUsage = `usage:
  oversire people list [--project P] [--json]
Without --project the list has all projects, the code host collaborators and the owners of the machines.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

const activityUsage = `usage:
  oversire activity list [--project P] [--unread] [--limit N] [--json]
The list has the tracker activity, newest first. A star marks an unread row. N is 1 to 200; the default is 30.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

const maxActivityLimit = 200

// optionalProject gives the normal project of --project, or "" when the flag is not set.
func (f *appFlags) optionalProject() (string, error) {
	if *f.project == "" {
		return "", nil
	}
	return f.projectArg()
}

// people runs `oversire people list`.
func (c *cli) people(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(peopleUsage)}
	}
	if args[0] != "list" {
		return &exitError{2, fmt.Errorf("unknown people command %q\n%s", args[0], peopleUsage)}
	}
	f := newAppFlags("people list")
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(peopleUsage, err)
	}
	if len(pos) > 0 {
		return f.usage(peopleUsage, nil)
	}
	p, err := f.optionalProject()
	if err != nil {
		return f.usage(peopleUsage, err)
	}
	a := map[string]any{}
	if p != "" {
		a["project"] = p
	}
	return c.runApp(ctx, f, "people.list", a, c.printPersonDetails)
}

// printPersonDetails prints the people without the email. --json has it.
func (c *cli) printPersonDetails(data json.RawMessage) error {
	var rows []struct {
		ID, Name, Login   string
		Sources, Machines []string
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLOGIN\tSOURCES\tMACHINES\tID")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, dash(r.Login), dash(strings.Join(r.Sources, ",")), dash(strings.Join(r.Machines, ",")), r.ID)
	}
	return tw.Flush()
}

// activity runs `oversire activity list`.
func (c *cli) activity(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(activityUsage)}
	}
	if args[0] != "list" {
		return &exitError{2, fmt.Errorf("unknown activity command %q\n%s", args[0], activityUsage)}
	}
	f := newAppFlags("activity list")
	unread := f.fs.Bool("unread", false, "only the unread rows")
	limit := f.fs.Int("limit", 0, "at most this many rows: 1 to 200")
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(activityUsage, err)
	}
	if len(pos) > 0 {
		return f.usage(activityUsage, nil)
	}
	p, err := f.optionalProject()
	if err != nil {
		return f.usage(activityUsage, err)
	}
	a := map[string]any{}
	if p != "" {
		a["project"] = p
	}
	if *unread {
		a["unread"] = true
	}
	limitGiven := false
	f.fs.Visit(func(fl *flag.Flag) { limitGiven = limitGiven || fl.Name == "limit" })
	if limitGiven {
		if *limit < 1 || *limit > maxActivityLimit {
			return f.usage(activityUsage, fmt.Errorf("--limit must be 1 to %d", maxActivityLimit))
		}
		a["limit"] = *limit
	}
	return c.runApp(ctx, f, "activity.list", a, c.printActivity)
}

// printActivity prints the rows of activity.list: {entries: [...]}.
func (c *cli) printActivity(data json.RawMessage) error {
	var d struct {
		Entries []struct {
			At, Key, Actor, Text string
			Unread               bool
		}
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(d.Entries) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  AT\tKEY\tACTOR\tTEXT")
	for _, e := range d.Entries {
		mark, at := " ", e.At
		if e.Unread {
			mark = "*"
		}
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			at = t.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", mark, at, dash(e.Key), dash(e.Actor), strings.Join(strings.Fields(e.Text), " "))
	}
	return tw.Flush()
}
