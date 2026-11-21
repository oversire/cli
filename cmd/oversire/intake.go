package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const intakeUsage = `usage:
  oversire intake create --project P --title T [--body-file F|-] [--priority X]
Each command takes --json, --harness H and --session ID.
--body-file - reads the text from stdin. The text has at most 1 MB.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

// intake runs `oversire intake <create>`.
func (c *cli) intake(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(intakeUsage)}
	}
	switch args[0] {
	case "create":
		return c.intakeCreate(ctx, args[1:])
	}
	return &exitError{2, fmt.Errorf("unknown intake command %q\n%s", args[0], intakeUsage)}
}

func (c *cli) intakeCreate(ctx context.Context, args []string) error {
	f := newAppFlags("intake create")
	title := f.fs.String("title", "", "the title")
	bodyFile := f.fs.String("body-file", "", "the Markdown description: a file, or - for stdin")
	priority := f.fs.String("priority", "", "a priority name")
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(intakeUsage, err)
	}
	if len(pos) > 0 {
		return f.usage(intakeUsage, nil)
	}
	if *f.project == "" || strings.TrimSpace(*title) == "" {
		return f.usage(intakeUsage, errors.New("give --project and --title"))
	}
	project, err := f.projectArg()
	if err != nil {
		return f.usage(intakeUsage, err)
	}
	a := map[string]any{"project": project, "title": *title}
	if *bodyFile != "" {
		if a["body"], err = c.readBody(*bodyFile); err != nil {
			return err
		}
	}
	if *priority != "" {
		a["priority"] = *priority
	}
	return c.runApp(ctx, f, "intake.create", a, c.printKey)
}
