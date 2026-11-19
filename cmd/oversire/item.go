package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const itemUsage = "usage: oversire item show <KEY> [--project P] [--json]"

// item runs `oversire item show`.
func (c *cli) item(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "show" {
		return &exitError{2, errors.New(itemUsage)}
	}
	f := newAppFlags("item show")
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(itemUsage, err)
	}
	if len(pos) != 1 {
		return f.usage(itemUsage, nil)
	}
	key, err := itemKey(pos[0], *f.project)
	if err != nil {
		return f.usage(itemUsage, err)
	}
	if !*f.json {
		return c.runApp(ctx, f, "item.markdown", map[string]any{"key": key}, c.printMarkdown)
	}
	res, err := c.callApp(ctx, f, "item.get", map[string]any{"key": key})
	if err != nil {
		return err
	}
	if res.Status != wire.AppCallOK {
		return &exitError{1, errors.New(res.Summary)}
	}
	return c.printData(res.Data)
}

// itemKey gives the normal key. With a project, a bare number N gives P-N.
func itemKey(arg, project string) (string, error) {
	if project == "" {
		return ticket.NormalKey(arg)
	}
	p, err := ticket.NormalProject(project)
	if err != nil {
		return "", err
	}
	n := strings.TrimSpace(arg)
	if n != "" && strings.Trim(n, "0123456789") == "" {
		n = p + "-" + n
	}
	k, err := ticket.NormalKey(n)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(k, p+"-") {
		return "", fmt.Errorf("the key is not in project %s", p)
	}
	return k, nil
}

// printMarkdown writes the Markdown of the item, then the note of the app when it has one.
func (c *cli) printMarkdown(data json.RawMessage) error {
	var d struct{ Markdown, CommentsNote string }
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	fmt.Fprintf(c.out, "\n%s", d.Markdown)
	if !strings.HasSuffix(d.Markdown, "\n") {
		fmt.Fprintln(c.out)
	}
	if d.CommentsNote != "" {
		fmt.Fprintf(c.out, "\n%s\n", d.CommentsNote)
	}
	return nil
}
