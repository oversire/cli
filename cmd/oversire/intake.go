package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oversire/cli/ticket"
)

const intakeUsage = `usage:
  oversire intake create --project P --title T [--body-file F|-] [--priority X]
  oversire intake list [--project P] [--status pending|snoozed|all] [--detail] [--limit N]
  oversire intake show <KEY> [--project P]
  oversire intake accept|decline <KEY> [--comment-file F|-]
  oversire intake snooze <KEY> --until YYYY-MM-DD | --days N [--comment-file F|-]
  oversire intake duplicate <KEY> --of KEEP [--comment-file F|-]
  oversire intake edit <KEY> [--title T] [--body-file F|-] [--priority X] [--parent KEY|none] [--blocks|--blocked-by|--relates-to|--duplicate-of OTHER] [--comment-file F|-]
<KEY> is the key of the work item, a number with --project, or an intake id with --project.
--status all means pending and snoozed. --until means 09:00 local time on that day.
Each command takes --json, --harness H and --session ID.
--body-file - and --comment-file - read the text from stdin. The text has at most 1 MB.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

// intake runs `oversire intake <create|list|show|accept|decline|snooze|duplicate|edit>`.
func (c *cli) intake(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(intakeUsage)}
	}
	switch args[0] {
	case "create":
		return c.intakeCreate(ctx, args[1:])
	case "list", "show", "accept", "decline", "snooze", "duplicate", "edit":
		return c.intakeSub(ctx, args[0], args[1:])
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

// intakeFlags are the flags of the intake commands. A command registers only its own flags.
type intakeFlags struct {
	status, commentFile, until, of, bodyFile, priority, parent, title string
	days, limit                                                       int
	detail                                                            bool
	// The relation flags by relation kind.
	relate map[string]*string
}

func (it *intakeFlags) register(fs *flag.FlagSet, sub string) {
	switch sub {
	case "list":
		fs.StringVar(&it.status, "status", "", "pending (default), snoozed or all")
		fs.BoolVar(&it.detail, "detail", false, "add the body and the labels to the JSON")
		fs.IntVar(&it.limit, "limit", 0, "at most this many items")
	case "snooze":
		fs.StringVar(&it.until, "until", "", "the day, YYYY-MM-DD (09:00 local time)")
		fs.IntVar(&it.days, "days", 0, "days from today")
	case "duplicate":
		fs.StringVar(&it.of, "of", "", "the key of the item that the intake item duplicates")
	case "edit":
		fs.StringVar(&it.title, "title", "", "the new title")
		fs.StringVar(&it.bodyFile, "body-file", "", "the new Markdown description: a file, or - for stdin")
		fs.StringVar(&it.priority, "priority", "", "a priority name")
		fs.StringVar(&it.parent, "parent", "", "the key of the parent item, or none")
		it.relate = map[string]*string{}
		for flagName, kind := range map[string]string{"blocks": "blocks", "blocked-by": "blockedBy", "relates-to": "relatesTo", "duplicate-of": "duplicate"} {
			it.relate[kind] = fs.String(flagName, "", "the key of the other item")
		}
	}
	if sub != "list" && sub != "show" {
		fs.StringVar(&it.commentFile, "comment-file", "", "a Markdown comment for the work item: a file, or - for stdin")
	}
}

// intakeSub runs each intake command except create.
func (c *cli) intakeSub(ctx context.Context, sub string, args []string) error {
	f := newAppFlags("intake " + sub)
	var it intakeFlags
	it.register(f.fs, sub)
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(intakeUsage, err)
	}
	bad := func(err error) error { return f.usage(intakeUsage, err) }
	a := map[string]any{}
	if sub == "list" {
		if len(pos) > 0 {
			return bad(nil)
		}
		if it.status != "" {
			s := strings.ToLower(it.status)
			if s != "pending" && s != "snoozed" && s != "all" {
				return bad(fmt.Errorf("the status is pending, snoozed or all, not %q", it.status))
			}
			a["status"] = s
		}
		if *f.project != "" {
			if a["project"], err = ticket.NormalProject(*f.project); err != nil {
				return bad(err)
			}
		}
		if it.detail {
			a["detail"] = true
		}
		if it.limit < 0 {
			return bad(errors.New("the limit is not negative"))
		}
		if it.limit > 0 {
			a["limit"] = it.limit
		}
		return c.runApp(ctx, f, "intake.list", a, c.printIntakeList)
	}
	if len(pos) != 1 {
		return bad(nil)
	}
	project, id, err := intakeRef(pos[0], *f.project)
	if err != nil {
		return bad(err)
	}
	a["project"], a["id"] = project, id
	action, show := "intake."+sub, (func(json.RawMessage) error)(nil)
	switch sub {
	case "show":
		action, show = "intake.list", c.printIntake
		a["detail"] = true
	case "accept", "decline":
	case "snooze":
		switch {
		case it.until != "" && it.days != 0:
			return bad(errors.New("give --until or --days, not both"))
		case it.until != "":
			if a["until"], err = snoozeUntil(it.until, time.Now()); err != nil {
				return bad(err)
			}
		case it.days > 0:
			a["days"] = it.days
		default:
			return bad(errors.New("give --until YYYY-MM-DD or --days N"))
		}
	case "duplicate":
		if it.of == "" {
			return bad(errors.New("give --of with the key of the item to keep"))
		}
		if a["of"], err = keyIn(it.of, project); err != nil {
			return bad(err)
		}
		action = "intake.merge"
	case "edit":
		if err := it.edit(c, a, project); err != nil {
			if _, ok := errors.AsType[*exitError](err); ok {
				return err
			}
			return bad(err)
		}
	}
	if it.commentFile != "" {
		if it.commentFile == "-" && it.bodyFile == "-" {
			return bad(errors.New("only one of --body-file and --comment-file can read stdin"))
		}
		if a["comment"], err = c.readBody(it.commentFile); err != nil {
			return err
		}
	}
	return c.runApp(ctx, f, action, a, show)
}

// edit puts the flags of `intake edit` in the args.
func (it *intakeFlags) edit(c *cli, a map[string]any, project string) (err error) {
	patch := map[string]any{}
	if it.title != "" {
		patch["title"] = it.title
	}
	if len(patch) > 0 {
		a["patch"] = patch
	}
	if it.bodyFile != "" {
		if a["body"], err = c.readBody(it.bodyFile); err != nil {
			return err
		}
	}
	if it.priority != "" {
		a["priority"] = it.priority
	}
	if it.parent != "" {
		a["parent"] = "none"
		if strings.ToLower(it.parent) != "none" {
			if a["parent"], err = keyIn(it.parent, project); err != nil {
				return err
			}
		}
	}
	for kind, other := range it.relate {
		if *other == "" {
			continue
		}
		if a["kind"] != nil {
			return errors.New("give only one relation flag")
		}
		a["kind"] = kind
		if a["other"], err = keyIn(*other, project); err != nil {
			return err
		}
	}
	if len(a) == 2 {
		return errors.New("give a field to change")
	}
	return nil
}

var digits = regexp.MustCompile(`^[0-9]+$`)

// intakeRef gives the project and the id for the app. A key gives its project and the key. A number with a project gives
// P-N. Other text with a project is an intake id.
func intakeRef(arg, project string) (proj, id string, err error) {
	arg = strings.TrimSpace(arg)
	if project != "" {
		if proj, err = ticket.NormalProject(project); err != nil {
			return "", "", err
		}
	}
	if k, kerr := ticket.NormalKey(arg); kerr == nil {
		p, _, _ := strings.Cut(k, "-")
		if proj != "" && proj != p {
			return "", "", fmt.Errorf("the key is not in project %s", proj)
		}
		return p, k, nil
	}
	switch {
	case proj == "" && digits.MatchString(arg):
		return "", "", errors.New("give --project with a number")
	case proj == "":
		return "", "", errors.New("give --project with an intake id")
	case digits.MatchString(arg):
		return proj, proj + "-" + arg, nil
	case arg == "":
		return "", "", errors.New("give a key")
	}
	return proj, arg, nil
}

// keyIn normalizes a key. A bare number is in the project.
func keyIn(arg, project string) (string, error) {
	if digits.MatchString(strings.TrimSpace(arg)) {
		return itemKey(arg, project)
	}
	return ticket.NormalKey(arg)
}

// snoozeUntil gives 09:00 local time on the day as RFC 3339. A day that has no 09:00 ahead is an error.
func snoozeUntil(arg string, now time.Time) (string, error) {
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(arg), now.Location())
	if err != nil {
		return "", fmt.Errorf("give --until as YYYY-MM-DD, not %q", arg)
	}
	at := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, now.Location())
	if !at.After(now) {
		return "", fmt.Errorf("%s is in the past", arg)
	}
	return at.Format(time.RFC3339), nil
}

type intakeRow struct {
	Project, Key, ID, Title, Priority, Author, Source, CreatedAt, Status, SnoozedUntil, Body string
	Labels                                                                                   []string
}

type intakeData struct {
	Items []intakeRow
	Total int
}

func (r intakeRow) status() string {
	s := dash(r.Status)
	if r.SnoozedUntil != "" {
		s += " until " + localDay(r.SnoozedUntil)
	}
	return s
}

// localDay is the local date of an RFC 3339 time. Text that is not a time stays as it is.
func localDay(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02")
}

func (c *cli) printIntakeList(data json.RawMessage) error {
	var d intakeData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(d.Items) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATUS\tPRIORITY\tCREATED\tAUTHOR\tTITLE")
	for _, r := range d.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(r.Key), r.status(), dash(r.Priority), localDay(r.CreatedAt), dash(r.Author), r.Title)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if d.Total > len(d.Items) {
		fmt.Fprintf(c.out, "Showing %d of %d.\n", len(d.Items), d.Total)
	}
	return nil
}

func (c *cli) printIntake(data json.RawMessage) error {
	var d intakeData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(d.Items) == 0 {
		return nil
	}
	r := d.Items[0]
	fmt.Fprintf(c.out, "\n# %s\n\n", r.Title)
	facts := []string{"Key: " + dash(r.Key), "Status: " + r.status(), "Priority: " + dash(r.Priority), "Author: " + dash(r.Author)}
	if r.Source != "" {
		facts = append(facts, "Source: "+r.Source)
	}
	facts = append(facts, "Created: "+localDay(r.CreatedAt))
	if len(r.Labels) > 0 {
		facts = append(facts, "Labels: "+strings.Join(r.Labels, ", "))
	}
	fmt.Fprintln(c.out, strings.Join(facts, ". ")+".")
	if strings.TrimSpace(r.Body) != "" {
		fmt.Fprintf(c.out, "\n%s", r.Body)
		if !strings.HasSuffix(r.Body, "\n") {
			fmt.Fprintln(c.out)
		}
	}
	return nil
}
