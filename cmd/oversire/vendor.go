package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const vendorUsage = `usage:
  oversire vendor ls [--json]
  oversire vendor run [--project P | --account A] [--json] -- <command of the vendor CLI> [its flags]
  oversire vendor skill <vendor> [--json]
  oversire vendor install <vendor> [--json]
  oversire vendor remove <vendor> [--json]
The app on this Mac runs the vendor CLI of the account (plane-cli, big-timer-cli or gh) with the token of its Keychain.
--project picks the tracker account of the project. --account is <provider>:<id> or <id>, for example github:oversire.
vendor ls shows the version, the source (oversire or path) and the state (ok, old, missing) of each vendor CLI.
vendor run refuses a missing vendor CLI, or one that is older than the version that OverSire needs.
Put the vendor command first, or give help or --help first. The app refuses the host and token flags, the login and
config commands, and the stream commands. The output has at most 1 MB. The vendor CLI has 45 s.
On Plane the app sets the workspace, and with --project the project: do not give -w, or -p with --project.
<vendor> is the PROVIDER of vendor ls: plane, bigtimer or github. vendor skill prints how to use the vendor CLI
through OverSire. Run it before the first vendor run for a vendor.
vendor install puts the newest checked vendor CLI in ~/.config/oversire/vendors/<vendor>/<version>/ (plane from
the signed Trebi catalog, bigtimer from its GitHub release). vendor run then uses that copy first. Install gh with
Homebrew. vendor remove deletes only the OverSire copy. An install waits at most 50 s. A longer install goes on in
the app: run oversire vendor ls later.
Exit codes: 0 ok, 1 failed, refused, or a vendor exit that is not 0, 2 usage, 3 no daemon or no app.`

// vendor runs `oversire vendor ls|run|skill|install|remove`.
func (c *cli) vendor(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(vendorUsage)}
	}
	switch args[0] {
	case "ls":
		return c.vendorList(ctx, args[1:])
	case "run":
		return c.vendorRun(ctx, args[1:])
	case "skill":
		return c.vendorSkill(ctx, args[1:])
	case "install", "remove":
		return c.vendorChange(ctx, "vendor."+args[0], args[1:])
	}
	return &exitError{2, errors.New(vendorUsage)}
}

func (c *cli) vendorList(ctx context.Context, args []string) error {
	f := newAppFlags("vendor ls")
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(vendorUsage, err)
	}
	if len(pos) > 0 {
		return f.usage(vendorUsage, nil)
	}
	return c.runApp(ctx, f, "vendor.list", map[string]any{}, c.printVendors)
}

func (c *cli) vendorSkill(ctx context.Context, args []string) error {
	f := newAppFlags("vendor skill")
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(vendorUsage, err)
	}
	if len(pos) != 1 {
		return f.usage(vendorUsage, nil)
	}
	return c.runApp(ctx, f, "vendor.skill", map[string]any{"vendor": pos[0]}, c.printVendorSkill)
}

func (c *cli) printVendorSkill(data json.RawMessage) error {
	var out wire.VendorSkillOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	fmt.Fprint(c.out, "\n"+strings.TrimRight(out.Markdown, "\n")+"\n")
	return nil
}

// vendorChange runs `oversire vendor install|remove <vendor>`.
func (c *cli) vendorChange(ctx context.Context, action string, args []string) error {
	f := newAppFlags(strings.Replace(action, ".", " ", 1))
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(vendorUsage, err)
	}
	if len(pos) != 1 || pos[0] == "" {
		return f.usage(vendorUsage, errors.New("give one vendor, for example plane"))
	}
	return c.runApp(ctx, f, action, map[string]any{"vendor": strings.ToLower(pos[0])}, nil)
}

// vendorRun writes the stdout and the stderr of the vendor CLI as they are. A vendor exit that is not 0 gives exit 1.
func (c *cli) vendorRun(ctx context.Context, args []string) error {
	flags, rest, ok := splitDash(args)
	switch {
	case !ok:
		return &exitError{2, fmt.Errorf("give -- before the command of the vendor CLI\n%s", vendorUsage)}
	case len(rest) == 0:
		return &exitError{2, fmt.Errorf("give the command of the vendor CLI after --\n%s", vendorUsage)}
	}
	f := newAppFlags("vendor run")
	account := f.fs.String("account", "", "the account, <provider>:<id> or <id>")
	if err := f.fs.Parse(flags); err != nil {
		return f.usage(vendorUsage, err)
	}
	if f.fs.NArg() > 0 {
		return f.usage(vendorUsage, fmt.Errorf("unexpected argument %q before --", f.fs.Arg(0)))
	}
	if (*f.project == "") == (*account == "") {
		return f.usage(vendorUsage, errors.New("give --project or --account, not both"))
	}
	call := map[string]any{"args": rest}
	if *f.project != "" {
		p, err := ticket.NormalProject(*f.project)
		if err != nil {
			return f.usage(vendorUsage, err)
		}
		call["project"] = p
	} else {
		call["account"] = *account
	}
	wd, err := c.workdir()
	if err != nil {
		return err
	}
	call["cwd"] = wd

	res, err := c.callApp(ctx, f, "vendor.run", call)
	if err != nil {
		return err
	}
	var out wire.VendorRunOutput
	hasOut := len(res.Data) > 0 && json.Unmarshal(res.Data, &out) == nil && out.Vendor != ""
	if *f.json {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
		if res.Status != wire.AppCallOK || (hasOut && out.Exit != 0) {
			return &exitError{1, errSilent}
		}
		return nil
	}
	if hasOut {
		fmt.Fprint(c.out, out.Stdout)
		fmt.Fprint(c.stderr(), out.Stderr)
		if out.Truncated {
			fmt.Fprintln(c.stderr(), "oversire: the output is cut at 1 MB.")
		}
	}
	if res.Status != wire.AppCallOK {
		return &exitError{1, errors.New(res.Summary)}
	}
	if !hasOut {
		return fmt.Errorf("The app answer is not valid: no vendor output")
	}
	if out.Exit != 0 {
		return &exitError{1, fmt.Errorf("%s exited with %d", out.Vendor, out.Exit)}
	}
	return nil
}

// splitDash splits args at the first "--". ok is false when there is no "--".
func splitDash(args []string) (flags, rest []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

func (c *cli) printVendors(data json.RawMessage) error {
	var rows []wire.VendorCLIRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tBINARY\tVERSION\tSOURCE\tSTATE\tPATH\tACCOUNTS\tPROJECTS")
	for _, r := range rows {
		source, state := dash(r.Source), dash(r.State)
		if r.Source == "path" && r.OwnVersion != "" {
			source = "path (oversire " + r.OwnVersion + " old)"
		}
		if r.Job != "" {
			state += " (" + r.Job + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Provider, r.Binary, dash(r.Version), source,
			state, dash(r.Path), dash(strings.Join(r.Accounts, ",")), dash(strings.Join(r.Projects, ",")))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range rows {
		if p := r.Problem(); p != "" {
			fmt.Fprintf(c.out, "%s: %s\n", r.Provider, p)
		}
	}
	return nil
}
