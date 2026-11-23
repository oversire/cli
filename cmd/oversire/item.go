package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const itemUsage = `usage:
  oversire item list [--project P] [--group G[,G]] [--mine] [--label L] [--text T] [--created-since DATE] [--limit N]
  oversire item search <text> [--project P] [--limit N]
  oversire item show <KEY> [--project P]
  oversire item history <KEY> [--limit N]
  oversire item create --project P --title T [--body-file F|-] [--status S] [--priority X] [--parent KEY] [--label L]... [--epic E]
                       [--start DATE] [--due DATE] [--estimate E] [--worktree DIR|here] [--share-parent-group] [--make-group]
  oversire item bind <KEY> [--worktree DIR|here] [--share-parent-group]
  oversire item edit <KEY> [--title T] [--body-file F|-] [--start DATE] [--due DATE] [--estimate E]
  oversire item status <KEY> <status> [--force] [--sub-items]
  oversire item start <KEY>
  oversire item done <KEY> [--force] [--sub-items]
  oversire item reopen <KEY>
  oversire item assign <KEY> --me | PERSON... | --none
  oversire item priority <KEY> <name>
  oversire item label <KEY> --add|--remove|--set NAME... | --none
  oversire item areas <KEY> AREA... | --none
  oversire item comment <KEY> --body-file F|-
  oversire item relate <KEY> --blocks|--blocked-by|--relates-to|--duplicate-of OTHER [--remove]
  oversire item parent <KEY> <PARENT|none>
  oversire item link <KEY> <URL> [--title T]
  oversire item attach <KEY> <FILE> [--name N]
Each command takes --json, --harness H and --session ID, and --project P for a bare item number.
The status is an id, a name, or one of todo, progress, review, blocked, done. A group is one of the same words.
--sub-items sets the open sub-items to the same status. --force closes a parent and keeps its sub-items open.
An area has at most 32 lower-case letters, digits or hyphens. An item has at most 6 areas.
A DATE is YYYY-MM-DD. For --start, --due and --estimate, none removes the value. The estimate uses the units of the tracker, for example S, M, L or XL.
--created-since lists only the loaded items. It leaves out an item with no creation time.
--worktree sets the fixed worktree folder of the item: a path, a template with {key} (default .wt/{key}), or here for the
working folder. --share-parent-group puts a sub-item in the group of its parent. --make-group makes the folder on this Mac.
--body-file - reads the text from stdin. The text has at most 1 MB. A file has at most 10 MB.
The app on this Mac runs the command. Exit codes: 0 ok, 1 failed or not allowed, 2 usage, 3 no daemon or no app.`

const (
	maxBodyBytes   = 1 << 20
	maxAttachBytes = 10 << 20
	maxAreas       = 6
)

// areaName is the pattern of Contract/schema/suggestion.json.
var areaName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// stringsFlag is a flag that you can repeat.
type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }

func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// itemFlags are the flags of the item commands. A command registers only its own flags.
type itemFlags struct {
	title, bodyFile, status, priority, parent, epic string
	group, label, text, name                        string
	start, due, estimate, createdSince              string
	labels                                          stringsFlag
	limit                                           int
	mine, force, me, none, remove                   bool
	add, set, subItems                              bool
	worktree                                        string
	shareParentGroup, makeGroup                     bool
	// The relate flags by relation kind.
	relate map[string]*string
}

func (it *itemFlags) register(fs *flag.FlagSet, sub string) {
	switch sub {
	case "list":
		fs.StringVar(&it.group, "group", "", "todo, progress, review, blocked or done; separate with commas")
		fs.BoolVar(&it.mine, "mine", false, "only the items that are assigned to you")
		fs.StringVar(&it.label, "label", "", "a label name")
		fs.StringVar(&it.text, "text", "", "words in the key, title or body")
		fs.StringVar(&it.createdSince, "created-since", "", "only loaded items made on this day or later, as YYYY-MM-DD")
		fs.IntVar(&it.limit, "limit", 0, "at most this many items")
	case "search":
		fs.IntVar(&it.limit, "limit", 0, "at most this many results")
	case "history":
		fs.IntVar(&it.limit, "limit", 0, "at most this many entries, the newest")
	case "create":
		fs.StringVar(&it.title, "title", "", "the title")
		fs.StringVar(&it.bodyFile, "body-file", "", "the Markdown description: a file, or - for stdin")
		fs.StringVar(&it.status, "status", "", "a status name, id or category word")
		fs.StringVar(&it.priority, "priority", "", "a priority name")
		fs.StringVar(&it.parent, "parent", "", "the key of the parent item")
		fs.Var(&it.labels, "label", "a label name; repeat for more")
		fs.StringVar(&it.epic, "epic", "", "the epic id or title")
		it.registerSchedule(fs)
		it.registerWorktree(fs)
		fs.BoolVar(&it.makeGroup, "make-group", false, "make the worktree folder on this Mac")
	case "bind":
		it.registerWorktree(fs)
	case "edit":
		fs.StringVar(&it.title, "title", "", "the new title")
		fs.StringVar(&it.bodyFile, "body-file", "", "the new Markdown description: a file, or - for stdin")
		it.registerSchedule(fs)
	case "comment":
		fs.StringVar(&it.bodyFile, "body-file", "", "the Markdown comment: a file, or - for stdin")
	case "status", "done":
		fs.BoolVar(&it.force, "force", false, "close a parent that has open sub-items")
		fs.BoolVar(&it.subItems, "sub-items", false, "set the open sub-items to the same status")
	case "label":
		fs.BoolVar(&it.add, "add", false, "add the labels and keep the others")
		fs.BoolVar(&it.remove, "remove", false, "remove the labels and keep the others")
		fs.BoolVar(&it.set, "set", false, "set exactly these labels")
		fs.BoolVar(&it.none, "none", false, "remove all labels")
	case "areas":
		fs.BoolVar(&it.none, "none", false, "remove all areas")
	case "assign":
		fs.BoolVar(&it.me, "me", false, "add yourself and keep the others")
		fs.BoolVar(&it.none, "none", false, "remove all assignees")
	case "relate":
		fs.BoolVar(&it.remove, "remove", false, "remove the relation")
		it.relate = map[string]*string{}
		for flagName, kind := range map[string]string{"blocks": "blocks", "blocked-by": "blockedBy", "relates-to": "relatesTo", "duplicate-of": "duplicate"} {
			it.relate[kind] = fs.String(flagName, "", "the key of the other item")
		}
	case "link":
		fs.StringVar(&it.title, "title", "", "the link title")
	case "attach":
		fs.StringVar(&it.name, "name", "", "the file name in the tracker")
	}
}

func (it *itemFlags) registerWorktree(fs *flag.FlagSet) {
	fs.StringVar(&it.worktree, "worktree", "", "the worktree folder: a path, a template with {key}, or here")
	fs.BoolVar(&it.shareParentGroup, "share-parent-group", false, "work in the group of the parent item")
}

// worktreeArgs adds the worktree args. here and an absolute path are on this Mac, so they also send the machine.
func (it *itemFlags) worktreeArgs(c *cli, a map[string]any) error {
	w := it.worktree
	if strings.EqualFold(w, "here") {
		wd, err := c.workdir()
		if err != nil {
			return err
		}
		w = wd
	}
	if w != "" {
		a["worktree"] = w
		if filepath.IsAbs(w) {
			a["machine"] = c.machine()
		}
	}
	if it.shareParentGroup {
		a["shareParentGroup"] = true
	}
	return nil
}

func (it *itemFlags) registerSchedule(fs *flag.FlagSet) {
	fs.StringVar(&it.start, "start", "", "the start day as YYYY-MM-DD, or none")
	fs.StringVar(&it.due, "due", "", "the due day as YYYY-MM-DD, or none")
	fs.StringVar(&it.estimate, "estimate", "", "the estimate in the units of the tracker, or none")
}

// day checks a DATE flag: YYYY-MM-DD, or none when the flag allows it.
func day(flagName, v string, allowNone bool) error {
	if allowNone && strings.EqualFold(v, "none") {
		return nil
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return fmt.Errorf("--%s must be a date as YYYY-MM-DD", flagName)
	}
	return nil
}

// schedule puts the set date and estimate flags into the args of the app call.
func (it *itemFlags) schedule(a map[string]any, allowNone bool) error {
	for _, d := range []struct{ flag, arg, v string }{{"start", "startDate", it.start}, {"due", "dueDate", it.due}} {
		if d.v == "" {
			continue
		}
		if err := day(d.flag, d.v, allowNone); err != nil {
			return err
		}
		a[d.arg] = strings.ToLower(d.v)
	}
	if it.estimate != "" {
		a["estimate"] = it.estimate
	}
	return nil
}

// item runs `oversire item <sub>`.
func (c *cli) item(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(itemUsage)}
	}
	switch args[0] {
	case "show":
		return c.itemShow(ctx, args[1:])
	case "list", "search", "history", "create", "bind", "edit", "status", "start", "done", "reopen", "assign", "priority", "label", "areas", "comment", "relate", "parent", "link", "attach":
		return c.itemSub(ctx, args[0], args[1:])
	}
	return &exitError{2, fmt.Errorf("unknown item command %q\n%s", args[0], itemUsage)}
}

func (c *cli) itemShow(ctx context.Context, args []string) error {
	f := newAppFlags("item show")
	pos, err := f.parse(args)
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

// itemSub runs each item command except show.
func (c *cli) itemSub(ctx context.Context, sub string, args []string) error {
	f := newAppFlags("item " + sub)
	var it itemFlags
	it.register(f.fs, sub)
	pos, err := f.parse(args)
	if err != nil {
		return f.usage(itemUsage, err)
	}
	bad := func(err error) error { return f.usage(itemUsage, err) }
	// want checks the count of the positional arguments.
	want := func(min, max int) error {
		if len(pos) < min || len(pos) > max {
			return bad(nil)
		}
		return nil
	}
	project := ""
	if *f.project != "" {
		if project, err = ticket.NormalProject(*f.project); err != nil {
			return bad(err)
		}
	}
	a := map[string]any{}
	// first sets the key from the first positional argument.
	first := func() error {
		k, err := itemKey(pos[0], project)
		a["key"] = k
		return err
	}
	var action string
	var show func(json.RawMessage) error
	switch sub {
	case "list":
		if err := want(0, 0); err != nil {
			return err
		}
		action, show = "item.list", c.printItems
		if project != "" {
			a["project"] = project
		}
		if it.group != "" {
			a["group"] = strings.ToLower(it.group)
		}
		if it.mine {
			a["assignee"] = "me"
		}
		if it.label != "" {
			a["label"] = it.label
		}
		if it.text != "" {
			a["text"] = it.text
		}
		if it.createdSince != "" {
			if err := day("created-since", it.createdSince, false); err != nil {
				return bad(err)
			}
			a["createdSince"] = it.createdSince
		}
		if it.limit > 0 {
			a["limit"] = it.limit
		}
	case "history":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action, show = "item.history", c.printHistory
		if it.limit > 0 {
			a["limit"] = it.limit
		}
	case "search":
		if err := want(1, 1); err != nil {
			return err
		}
		action, show = "search.all", c.printSearch
		a["query"] = pos[0]
		if project != "" {
			a["project"] = project
		}
		if it.limit > 0 {
			a["limit"] = it.limit
		}
	case "create":
		if err := want(0, 0); err != nil {
			return err
		}
		if project == "" || it.title == "" {
			return bad(errors.New("give --project and --title"))
		}
		action, show = "item.create", c.printMade
		a["project"], a["title"] = project, it.title
		if err := it.worktreeArgs(c, a); err != nil {
			return err
		}
		if it.bodyFile != "" {
			if a["body"], err = c.readBody(it.bodyFile); err != nil {
				return err
			}
		}
		if it.parent != "" {
			if a["parent"], err = itemKey(it.parent, project); err != nil {
				return bad(err)
			}
		}
		for k, v := range map[string]string{"status": it.status, "priority": it.priority, "epic": it.epic} {
			if v != "" {
				a[k] = v
			}
		}
		if len(it.labels) > 0 {
			a["labels"] = []string(it.labels)
		}
		if err := it.schedule(a, false); err != nil {
			return bad(err)
		}
		if it.makeGroup {
			res, err := c.callApp(ctx, f, action, a)
			if err != nil {
				return err
			}
			if res.Status == wire.AppCallOK {
				c.makeGroup(res.Data)
			}
			return c.showApp(f, res, show)
		}
	case "bind":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action, show = "item.bindWorktree", c.printMade
		if err := it.worktreeArgs(c, a); err != nil {
			return err
		}
	case "edit":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		if err := it.schedule(a, true); err != nil {
			return bad(err)
		}
		switch {
		case it.bodyFile == "" && len(a) == 1 && it.title == "":
			return bad(errors.New("give --title, --body-file, --start, --due or --estimate"))
		case it.bodyFile == "" && len(a) == 1:
			// A title alone keeps the older call, which an app with no item.edit change still takes.
			action = "item.update"
			a["title"] = it.title
		default:
			action = "item.edit"
			if it.bodyFile != "" {
				if a["body"], err = c.readBody(it.bodyFile); err != nil {
					return err
				}
			}
			if it.title != "" {
				a["title"] = it.title
			}
		}
	case "status":
		if err := want(2, 2); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action = "item.setStatus"
		a["status"], a["force"] = pos[1], it.force
		if it.subItems {
			a["subItems"] = true
		}
	case "done":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action = "item.setStatus"
		a["status"], a["force"] = "done", it.force
		if it.subItems {
			a["subItems"] = true
		}
	case "start", "reopen":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action = "item." + sub
	case "assign":
		if err := want(1, 1<<10); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		people := pos[1:]
		switch {
		case it.me && (it.none || len(people) > 0):
			return bad(errors.New("--me does not go with --none or a person"))
		case it.me:
			action = "item.assignMe"
		case it.none && len(people) > 0:
			return bad(errors.New("--none does not go with a person"))
		case !it.none && len(people) == 0:
			return bad(errors.New("give --me, --none or a person"))
		default:
			action = "item.assign"
			a["people"] = append([]string{}, people...)
		}
	case "priority":
		if err := want(2, 2); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action = "item.setPriority"
		a["priority"] = pos[1]
	case "label":
		if err := want(1, 1<<10); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		names := pos[1:]
		mode := ""
		for m, on := range map[string]bool{"add": it.add, "remove": it.remove, "set": it.set, "none": it.none} {
			if !on {
				continue
			}
			if mode != "" {
				return bad(errors.New("give only one of --add, --remove, --set and --none"))
			}
			mode = m
		}
		switch {
		case mode == "":
			return bad(errors.New("give --add, --remove, --set or --none"))
		case mode == "none" && len(names) > 0:
			return bad(errors.New("--none does not go with a label"))
		case mode != "none" && len(names) == 0:
			return bad(errors.New("give at least one label"))
		}
		action = "item.setLabels"
		switch mode {
		case "none":
			a["labels"] = []string{}
		case "set":
			a["labels"] = append([]string{}, names...)
		default:
			a[mode] = append([]string{}, names...)
		}
	case "areas":
		if err := want(1, 1<<10); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		areas := pos[1:]
		switch {
		case it.none && len(areas) > 0:
			return bad(errors.New("--none does not go with an area"))
		case !it.none && len(areas) == 0:
			return bad(errors.New("give an area or --none"))
		case len(areas) > maxAreas:
			return bad(fmt.Errorf("an item has at most %d areas", maxAreas))
		}
		for _, n := range areas {
			if !areaName.MatchString(n) {
				return bad(fmt.Errorf("the area %q is not valid: use 1 to 32 lower-case letters, digits or hyphens", n))
			}
		}
		action = "item.setAreas"
		a["areas"] = append([]string{}, areas...)
	case "comment":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		if it.bodyFile == "" {
			return bad(errors.New("give --body-file"))
		}
		action = "item.comment"
		if a["body"], err = c.readBody(it.bodyFile); err != nil {
			return err
		}
	case "relate":
		if err := want(1, 1); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		kind, other := "", ""
		for k, v := range it.relate {
			if *v == "" {
				continue
			}
			if kind != "" {
				return bad(errors.New("give only one relation flag"))
			}
			kind, other = k, *v
		}
		if kind == "" {
			return bad(errors.New("give --blocks, --blocked-by, --relates-to or --duplicate-of"))
		}
		if other, err = itemKey(other, project); err != nil {
			return bad(err)
		}
		action = "item.relate"
		a["kind"], a["other"] = kind, other
		if it.remove {
			a["remove"] = true
		}
	case "parent":
		if err := want(2, 2); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		action = "item.update"
		a["parent"] = ""
		if strings.ToLower(pos[1]) != "none" {
			if a["parent"], err = itemKey(pos[1], project); err != nil {
				return bad(err)
			}
		}
	case "link":
		if err := want(2, 2); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		if u, err := url.Parse(pos[1]); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad(errors.New("give an http or https URL"))
		}
		action = "item.link"
		a["url"] = pos[1]
		if it.title != "" {
			a["title"] = it.title
		}
	case "attach":
		if err := want(2, 2); err != nil {
			return err
		}
		if err := first(); err != nil {
			return bad(err)
		}
		name := it.name
		if name == "" {
			name = filepath.Base(pos[1])
		}
		if a, err = readAttachment(a, pos[1], name); err != nil {
			return &exitError{2, err}
		}
		action = "item.attach"
	}
	return c.runApp(ctx, f, action, a, show)
}

// readBody reads the text of --body-file: a file, or - for stdin.
func (c *cli) readBody(path string) (string, error) {
	var r io.Reader = c.in
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return "", &exitError{2, err}
		}
		defer file.Close()
		r = file
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	switch {
	case err != nil:
		return "", &exitError{2, err}
	case len(b) > maxBodyBytes:
		return "", &exitError{2, errors.New("the body has more than 1 MB")}
	case strings.TrimSpace(string(b)) == "":
		return "", &exitError{2, errors.New("the body is empty")}
	}
	return string(b), nil
}

// readAttachment adds the name, the media type and the base64 bytes of the file to the args of item.attach.
func readAttachment(a map[string]any, path, name string) (map[string]any, error) {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, err
	case st.IsDir():
		return nil, fmt.Errorf("%s is a folder", path)
	case st.Size() > maxAttachBytes:
		return nil, fmt.Errorf("The file has %d MB. The limit is %d MB.", (st.Size()+1<<20-1)>>20, maxAttachBytes>>20)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	typ := mime.TypeByExtension(filepath.Ext(name))
	if typ == "" {
		typ = http.DetectContentType(b)
	}
	if m, _, err := mime.ParseMediaType(typ); err == nil {
		typ = m
	}
	a["name"], a["mimeType"], a["data"] = name, typ, base64.StdEncoding.EncodeToString(b)
	return a, nil
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

// printKey prints the key of a new item.
// made is the answer data of item.create and item.bindWorktree.
type made struct {
	Key      string
	Worktree *struct{ Group, Machine, Branch, Parent, Dir string }
}

// printMade prints the key, then the worktree lines.
func (c *cli) printMade(data json.RawMessage) error {
	var d made
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	if d.Key != "" {
		fmt.Fprintln(c.out, "Key: "+d.Key)
	}
	if w := d.Worktree; w != nil {
		fmt.Fprintf(c.out, "Worktree: %s on %s\n", w.Group, w.Machine)
		if w.Branch != "" {
			fmt.Fprintln(c.out, "Branch: "+w.Branch)
		}
		if w.Parent != "" {
			fmt.Fprintln(c.out, "Review in: "+w.Parent)
		}
	}
	return nil
}

// makeGroup makes the bound folder of the answer when it is on this Mac and is not a git checkout.
func (c *cli) makeGroup(data json.RawMessage) {
	var d made
	if json.Unmarshal(data, &d) != nil || d.Worktree == nil || d.Worktree.Dir == "" || !strings.EqualFold(d.Worktree.Machine, c.machine()) {
		return
	}
	if _, err := os.Stat(filepath.Join(d.Worktree.Dir, ".git")); err == nil {
		return
	}
	if err := os.MkdirAll(d.Worktree.Dir, 0o755); err != nil {
		fmt.Fprintf(c.out, "Could not make %s: %v\n", d.Worktree.Dir, err)
	}
}

func (c *cli) printKey(data json.RawMessage) error {
	var d struct{ Key string }
	if json.Unmarshal(data, &d) == nil && d.Key != "" {
		fmt.Fprintln(c.out, "Key: "+d.Key)
	}
	return nil
}

func (c *cli) printItems(data json.RawMessage) error {
	var d struct {
		Items []struct {
			Key, Title, Status string
			Assignees          []string
			DueDate, Estimate  string
		}
		Total int
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(d.Items) == 0 {
		return nil
	}
	hasDue, hasEstimate := false, false
	for _, i := range d.Items {
		hasDue = hasDue || i.DueDate != ""
		hasEstimate = hasEstimate || i.Estimate != ""
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	head, cols := "KEY\tSTATUS\tASSIGNEES", "%s\t%s\t%s"
	if hasDue {
		head, cols = head+"\tDUE", cols+"\t%s"
	}
	if hasEstimate {
		head, cols = head+"\tEST", cols+"\t%s"
	}
	fmt.Fprintln(tw, head+"\tTITLE")
	for _, i := range d.Items {
		row := []any{i.Key, dash(i.Status), dash(strings.Join(i.Assignees, ","))}
		if hasDue {
			row = append(row, dash(i.DueDate))
		}
		if hasEstimate {
			row = append(row, dash(i.Estimate))
		}
		fmt.Fprintf(tw, cols+"\t%s\n", append(row, i.Title)...)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if d.Total > len(d.Items) {
		fmt.Fprintf(c.out, "Showing %d of %d.\n", len(d.Items), d.Total)
	}
	return nil
}

// printHistory prints one line for each entry of item.history: day, time, actor, text.
func (c *cli) printHistory(data json.RawMessage) error {
	var d struct {
		Entries []struct {
			At    time.Time
			Actor string
			Text  string
		}
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	for _, e := range d.Entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.At.Local().Format("2006-01-02 15:04"), dash(e.Actor), e.Text)
	}
	return tw.Flush()
}

// printSearch prints only the items. Results from the tracker search have no status.
func (c *cli) printSearch(data json.RawMessage) error {
	var d struct {
		Items []struct{ Key, Title, Status string }
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("The app answer is not valid: %w", err)
	}
	if len(d.Items) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATUS\tTITLE")
	for _, i := range d.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", i.Key, i.Status, i.Title)
	}
	return tw.Flush()
}
