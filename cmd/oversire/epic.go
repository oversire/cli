package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oversire/cli/appcall"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const epicUsage = `usage:
  oversire epic list [--project P]
  oversire epic show <epic> --project P
  oversire epic create --project P --title T [--goal G] [--start YYYY-MM-DD] [--target YYYY-MM-DD] [--lead ID]
  oversire epic update <epic> --project P [--title T] [--goal G] [--state S] [--start D] [--target D] [--lead ID]
                       [--health on_track|at_risk|off_track [--note-file F]]
  oversire epic close <epic> --project P
  oversire epic add <epic> KEY... [--project P]
  oversire epic remove KEY... [--project P]
  oversire epic plan [<epic>] --project P
Each command takes --json, --harness H and --session ID. <epic> is the id or the title of the epic.
The state is planned, active, paused, done or canceled. The value none removes a date or the lead.
A health or a note file posts an epic update. --note-file takes a path, or - for stdin.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

const projectUsage = `usage:
  oversire project north-star --project P [--set TEXT] [--json]
  oversire project statuses|labels|people [P] [--project P] [--json]
  oversire project labels [P] --add NAME [--color #RRGGBB]
  oversire project refresh [P] [--project P]
--add makes a label in the tracker project. The default color is #3B82F6. refresh loads the project from the tracker again.
The project is a positional argument or --project. The app on this Mac runs the command.`

const appUsage = "usage: oversire app run <action> ['<args as a JSON object>'] [--json]"

// appCallTimeout is longer than the wait of the daemon (appcall.Wait), so the daemon answers first.
const appCallTimeout = 70 * time.Second

// appFlags are the flags of each command that the app runs.
type appFlags struct {
	fs      *flag.FlagSet
	project *string
	harness *string
	session *string
	json    *bool
}

func newAppFlags(name string) *appFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &appFlags{
		fs:      fs,
		project: fs.String("project", "", "the project identifier, for example OVERSIRE"),
		harness: fs.String("harness", "", "the harness of the agent that runs the command"),
		session: fs.String("session", "", "the session id of the agent"),
		json:    fs.Bool("json", false, "print the result as JSON"),
	}
}

// parse takes the flags before and after the positional arguments, and gives the positional arguments.
func (f *appFlags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.fs.Parse(args); err != nil {
			return nil, err
		}
		args = f.fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// agent names the agent for the event rows of a change: "<harness> <session>". Empty: a human ran the command.
func (f *appFlags) agent() string {
	h, s := *f.harness, *f.session
	if s == "" {
		if s = os.Getenv("CLAUDE_CODE_SESSION_ID"); s != "" && h == "" {
			h = "claude"
		}
	}
	if s == "" {
		s = os.Getenv("OVERSIRE_SESSION")
	}
	if h == "" && s != "" {
		h = "agent"
	}
	return strings.TrimSpace(h + " " + s)
}

// projectArg gives the normal project. Without --project it takes the project of the keys, when they have one.
func (f *appFlags) projectArg(keys ...string) (string, error) {
	p := *f.project
	if p == "" {
		for _, k := range keys {
			kp, _, _ := strings.Cut(k, "-")
			if p != "" && kp != p {
				return "", errors.New("the keys are in more than one project: give --project")
			}
			p = kp
		}
	}
	if p == "" {
		return "", errors.New("give --project")
	}
	return ticket.NormalProject(p)
}

func (f *appFlags) usage(text string, err error) error {
	if err == nil {
		return &exitError{2, errors.New(text)}
	}
	return &exitError{2, fmt.Errorf("%w\n%s", err, text)}
}

// epic runs `oversire epic <list|show|create|update|close|add|remove|plan>`.
func (c *cli) epic(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(epicUsage)}
	}
	sub := args[0]
	f := newAppFlags("epic " + sub)
	// The epic fields by flag name, with the arg name of the action.
	type field struct {
		arg string
		v   *string
	}
	fields := map[string]field{}
	add := func(name, arg, help string) { fields[name] = field{arg, f.fs.String(name, "", help)} }
	if sub == "create" || sub == "update" {
		add("title", "title", "the title")
		add("goal", "goal", "the goal, in Markdown")
		add("start", "startDate", "the start date, YYYY-MM-DD")
		add("target", "targetDate", "the target date, YYYY-MM-DD")
		add("lead", "lead", "the person id of the lead")
	}
	var noteFile *string
	if sub == "update" {
		add("state", "state", "planned, active, paused, done or canceled")
		add("health", "health", "on_track, at_risk or off_track")
		noteFile = f.fs.String("note-file", "", "the Markdown text of the epic update: a path, or - for stdin")
	}
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(epicUsage, err)
	}
	keys := make([]string, 0, len(pos))
	if sub == "add" || sub == "remove" {
		from := 1
		if sub == "remove" {
			from = 0
		}
		if len(pos) <= from {
			return f.usage(epicUsage, nil)
		}
		for _, k := range pos[from:] {
			n, err := ticket.NormalKey(k)
			if err != nil {
				return f.usage(epicUsage, err)
			}
			keys = append(keys, n)
		}
	}
	a := map[string]any{}
	var show func(json.RawMessage) error
	needEpic := false
	switch sub {
	case "list":
		if len(pos) > 0 {
			return f.usage(epicUsage, nil)
		}
		if *f.project != "" {
			p, err := f.projectArg()
			if err != nil {
				return f.usage(epicUsage, err)
			}
			a["project"] = p
		}
		show = c.printEpicList
	case "show", "close":
		needEpic = true
		if sub == "show" {
			show = c.printEpic
		}
	case "create":
		if len(pos) > 0 || *fields["title"].v == "" {
			return f.usage(epicUsage, nil)
		}
		show = c.printID
	case "update":
		needEpic = true
		f.fs.Visit(func(fl *flag.Flag) {
			if fd, ok := fields[fl.Name]; ok {
				a[fd.arg] = *fd.v
			}
		})
		f.fs.Visit(func(fl *flag.Flag) {
			if fl.Name == "note-file" {
				a["note"] = *noteFile
			}
		})
		if len(a) == 0 {
			return f.usage(epicUsage, errors.New("give a field to change"))
		}
		if path, ok := a["note"].(string); ok {
			if a["note"], err = c.readBody(path); err != nil {
				return err
			}
		}
	case "add":
		a["epic"], a["keys"] = pos[0], keys
	case "remove":
		a["keys"] = keys
	case "plan":
		if len(pos) > 1 {
			return f.usage(epicUsage, nil)
		}
		if len(pos) == 1 {
			a["epic"] = pos[0]
		}
		show = c.printPlan
	default:
		return f.usage(epicUsage, fmt.Errorf("unknown epic command %q", sub))
	}
	if needEpic {
		if len(pos) != 1 {
			return f.usage(epicUsage, nil)
		}
		a["epic"] = pos[0]
	}
	if sub == "create" {
		for _, fd := range fields {
			if *fd.v != "" {
				a[fd.arg] = *fd.v
			}
		}
	}
	if sub != "list" {
		p, err := f.projectArg(keys...)
		if err != nil {
			return f.usage(epicUsage, err)
		}
		a["project"] = p
	}
	action := map[string]string{"list": "epic.list", "show": "epic.show", "create": "epic.create", "update": "epic.update",
		"close": "epic.close", "add": "epic.addItems", "remove": "epic.removeItems", "plan": "epic.plan"}[sub]
	return c.runApp(ctx, f, action, a, show)
}

// labelColor is a hex color of a new label.
var labelColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// project runs `oversire project <north-star|statuses|labels|people|refresh>`.
func (c *cli) project(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(projectUsage)}
	}
	sub := args[0]
	reads := map[string]struct {
		action string
		show   func(json.RawMessage) error
	}{
		"statuses": {"project.statuses", c.printStatuses},
		"labels":   {"project.labels", c.printLabels},
		"people":   {"project.people", c.printPeople},
	}
	read, isRead := reads[sub]
	if sub != "north-star" && sub != "refresh" && !isRead {
		return &exitError{2, fmt.Errorf("unknown project command %q\n%s", sub, projectUsage)}
	}
	f := newAppFlags("project " + sub)
	var set, addLabel, color string
	switch sub {
	case "north-star":
		f.fs.StringVar(&set, "set", "", "the new north star")
	case "labels":
		f.fs.StringVar(&addLabel, "add", "", "the name of a new label")
		f.fs.StringVar(&color, "color", "", "the color of the new label, for example #3B82F6")
	}
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(projectUsage, err)
	}
	if len(pos) > 1 || (len(pos) == 1 && sub == "north-star") {
		return f.usage(projectUsage, nil)
	}
	if len(pos) == 1 {
		if *f.project != "" && !strings.EqualFold(*f.project, pos[0]) {
			return f.usage(projectUsage, errors.New("the project argument and --project differ"))
		}
		*f.project = pos[0]
	}
	p, err := f.projectArg()
	if err != nil {
		return f.usage(projectUsage, err)
	}
	given := map[string]bool{}
	f.fs.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
	if sub == "labels" {
		if given["color"] && !given["add"] {
			return f.usage(projectUsage, errors.New("--color goes with --add"))
		}
		if given["add"] {
			name := strings.TrimSpace(addLabel)
			if name == "" {
				return f.usage(projectUsage, errors.New("give a label name"))
			}
			if color == "" {
				color = "#3B82F6"
			}
			if !labelColor.MatchString(color) {
				return f.usage(projectUsage, errors.New("the color must look like #3B82F6"))
			}
			return c.runApp(ctx, f, "project.addLabel", map[string]any{"project": p, "name": name, "color": color}, c.printID)
		}
	}
	if sub == "refresh" {
		return c.runApp(ctx, f, "project.refresh", map[string]any{"project": p}, nil)
	}
	if isRead {
		return c.runApp(ctx, f, read.action, map[string]any{"project": p}, read.show)
	}
	if given["set"] {
		return c.runApp(ctx, f, "project.setNorthStar", map[string]any{"project": p, "text": set}, nil)
	}
	return c.runApp(ctx, f, "project.northStar", map[string]any{"project": p}, nil)
}

// app runs `oversire app run <action> [json]`: any action of the allowed list of the app.
func (c *cli) app(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return &exitError{2, errors.New(appUsage)}
	}
	f := newAppFlags("app run")
	pos, err := f.parse(args[1:])
	if err != nil {
		return f.usage(appUsage, err)
	}
	if len(pos) == 0 || len(pos) > 2 {
		return f.usage(appUsage, nil)
	}
	a := map[string]any{}
	if len(pos) == 2 {
		if err := json.Unmarshal([]byte(pos[1]), &a); err != nil || a == nil {
			return f.usage(appUsage, errors.New("the args must be a JSON object"))
		}
	}
	return c.runApp(ctx, f, pos[0], a, c.printData)
}

// callApp sends the call through the daemon to the app and gives the result.
func (c *cli) callApp(ctx context.Context, f *appFlags, action string, args map[string]any) (wire.AppCallResult, error) {
	d, err := c.daemonClient()
	if err != nil {
		return wire.AppCallResult{}, err
	}
	b, err := json.Marshal(args)
	if err != nil {
		return wire.AppCallResult{}, err
	}
	return d.appCall(ctx, wire.AppCall{Action: action, Args: b, Agent: f.agent()})
}

// runApp sends the call through the daemon to the app and prints the result. show prints the data of an ok
// result after the summary; nil prints only the summary.
func (c *cli) runApp(ctx context.Context, f *appFlags, action string, args map[string]any, show func(json.RawMessage) error) error {
	res, err := c.callApp(ctx, f, action, args)
	if err != nil {
		return err
	}
	if *f.json {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
		if res.Status != wire.AppCallOK {
			return &exitError{1, errSilent}
		}
		return nil
	}
	if res.Status != wire.AppCallOK {
		return &exitError{1, errors.New(res.Summary)}
	}
	fmt.Fprintln(c.out, res.Summary)
	if show == nil || len(res.Data) == 0 || string(res.Data) == "null" {
		return nil
	}
	return show(res.Data)
}

// appCall posts the call to POST /v1/app-calls. No daemon, no app, or a daemon without app calls gives exit code 3.
func (d daemonClient) appCall(ctx context.Context, call wire.AppCall) (wire.AppCallResult, error) {
	d.http = &http.Client{Timeout: appCallTimeout}
	d.maxBody = appcall.MaxResultBody
	var res wire.AppCallResult
	err := d.call(ctx, http.MethodPost, "/v1/app-calls", call, &res)
	if de, ok := errors.AsType[*daemonError](err); ok {
		switch de.Status {
		case 0, http.StatusServiceUnavailable:
			return res, &exitError{3, err}
		case http.StatusNotFound:
			return res, &exitError{3, errors.New("The daemon does not run app calls. Update it: oversire update")}
		}
	}
	return res, err
}

func (c *cli) printData(data json.RawMessage) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func (c *cli) printID(data json.RawMessage) error {
	var e struct{ ID string }
	if json.Unmarshal(data, &e) == nil && e.ID != "" {
		fmt.Fprintln(c.out, "Id: "+e.ID)
	}
	return nil
}

type epicRow struct {
	Project, ID, Title, State, Progress, Health string
	Next                                        []string
	LateDays                                    *int
	ReadyToClose                                bool
}

func (c *cli) printEpicList(data json.RawMessage) error {
	var rows []epicRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tEPIC\tSTATE\tPROGRESS\tLATE\tNEXT")
	for _, r := range rows {
		state := r.State
		if r.Health != "" {
			state += ", " + healthText(r.Health)
		}
		if r.ReadyToClose {
			state += ", ready to close"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Project, r.Title, state, r.Progress, days(r.LateDays), dash(strings.Join(r.Next, " ")))
	}
	return tw.Flush()
}

// healthText is "at risk" for at_risk.
func healthText(h string) string { return strings.ReplaceAll(h, "_", " ") }

type forecast struct {
	Start, End string
	LateBy     *int
}

func (f forecast) String() string {
	s := "starts " + f.Start
	if f.End != "" {
		s = f.Start + " to " + f.End
	}
	if f.LateBy != nil && *f.LateBy > 0 {
		s += fmt.Sprintf(", %d days late", *f.LateBy)
	}
	return s
}

type relations struct{ Loaded, Total, Failed int }

func (r relations) note() string {
	if r.Loaded >= r.Total {
		return ""
	}
	return fmt.Sprintf("Relations %d of %d loaded: the order can change.", r.Loaded, r.Total)
}

// slot is the place of a step in the schedule.
type slot struct {
	Key, Title, State string
	Start, End        string
	Slack             *int
	Critical          bool
}

func (s slot) key() string {
	if s.Critical {
		return "*" + s.Key
	}
	return s.Key
}

func (c *cli) printEpic(data json.RawMessage) error {
	var e struct {
		Title, State, URL, Goal, StartDate, TargetDate, Health string
		Forecast                                               forecast
		Relations                                              relations
		Progress                                               struct{ Done, Total, Open, Started int }
		Steps                                                  []struct {
			slot
			Number      int
			WaitsOnStep *int
			WaitsOnKey  string
		}
		Done []string
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	line := fmt.Sprintf("State: %s. Progress: %d of %d done, %d started. Forecast: %s.", e.State, e.Progress.Done, e.Progress.Total, e.Progress.Started, e.Forecast)
	if e.StartDate != "" || e.TargetDate != "" {
		line += fmt.Sprintf(" Dates: %s to %s.", dash(e.StartDate), dash(e.TargetDate))
	}
	if e.Health != "" {
		line += " Health: " + healthText(e.Health) + "."
	}
	fmt.Fprintln(c.out, line)
	if e.Goal != "" {
		fmt.Fprintln(c.out, "Goal: "+strings.ReplaceAll(strings.TrimSpace(e.Goal), "\n", "\n  "))
	}
	if e.URL != "" {
		fmt.Fprintln(c.out, "URL: "+e.URL)
	}
	if len(e.Steps) > 0 {
		tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tKEY\tSTATE\tSTART\tEND\tSLACK\tWAITS ON\tTITLE")
		for _, s := range e.Steps {
			waits := s.WaitsOnKey
			if s.WaitsOnStep != nil {
				waits = fmt.Sprintf("step %d", *s.WaitsOnStep)
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Number, s.key(), s.State, dash(s.Start), dash(s.End), days(s.Slack), dash(waits), s.Title)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(e.Done) > 0 {
		fmt.Fprintln(c.out, "Done: "+strings.Join(e.Done, ", "))
	}
	if n := e.Relations.note(); n != "" {
		fmt.Fprintln(c.out, n)
	}
	return nil
}

func (c *cli) printPlan(data json.RawMessage) error {
	var p struct {
		Relations relations
		Critical  []string
		Epics     []struct {
			Title, State string
			Forecast     forecast
			Steps        []struct {
				slot
				WaitsOn []string
			}
		}
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	for _, e := range p.Epics {
		fmt.Fprintf(c.out, "\n%s (%s): %s.\n", e.Title, e.State, e.Forecast)
		if len(e.Steps) == 0 {
			continue
		}
		tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  KEY\tSTATE\tSTART\tEND\tSLACK\tWAITS ON\tTITLE")
		for _, s := range e.Steps {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.key(), s.State, dash(s.Start), dash(s.End), days(s.Slack), dash(strings.Join(s.WaitsOn, " ")), s.Title)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(p.Critical) > 0 {
		fmt.Fprintln(c.out, "\nCritical path (*): "+strings.Join(p.Critical, ", "))
	}
	if n := p.Relations.note(); n != "" {
		fmt.Fprintln(c.out, n)
	}
	return nil
}

func days(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprintf("%dd", *n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (c *cli) printStatuses(data json.RawMessage) error {
	var rows []struct{ ID, Name, Group, Category string }
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tGROUP\tCATEGORY\tID")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, dash(r.Group), r.Category, r.ID)
	}
	return tw.Flush()
}

func (c *cli) printLabels(data json.RawMessage) error {
	var rows []struct{ ID, Name string }
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\n", r.Name, r.ID)
	}
	return tw.Flush()
}

func (c *cli) printPeople(data json.RawMessage) error {
	var rows []struct {
		ID, Name string
		Me       bool
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID")
	for _, r := range rows {
		name := r.Name
		if r.Me {
			name += " (you)"
		}
		fmt.Fprintf(tw, "%s\t%s\n", name, r.ID)
	}
	return tw.Flush()
}
