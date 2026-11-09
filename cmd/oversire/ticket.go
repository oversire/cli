package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/proc"
	"github.com/oversire/cli/review"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const ticketUsage = `Usage:
  oversire ticket skill [new|plan|work|delegate|done|groom|sweep|spec]
                                                          print the body of the oversire agent skill
  oversire ticket skill --install|--refresh [--harness claude,codex]
                                                          install the skill file, or update the installed files
  oversire ticket new|work <KEY> [--session ID] [--harness H]
                                                          bind this session to a ticket (work prints the earlier sessions)
  oversire ticket plan <KEY>... [--session ID] [--harness H]
                                                          bind this session to one ticket or a batch (at most 20)
  oversire ticket groom <PROJECT> [--session ID]
                                                          check the Groom key of the project and list the busy items
  oversire ticket applied groom|sweep --file F [--project P] [--session ID]
                                                          write the event rows of the applied changes
  oversire ticket sweep [KEY...] [--project P] [--root DIR] [--stale DAYS] [--json]
                                                          the facts of the started tickets: sessions, review, worktree group, base commits
  oversire ticket stop <SESSION> [--session ID]
                                                          end a daemon session of a finished ticket
  oversire ticket done [KEY] [--session ID] [--harness H] [--out DIR]
                                                          pack the transcript of this session for the ticket
`

// ticketCLI makes no config folder and runs no migration: the skill calls it each time it loads.
func ticketCLI() *cli {
	home, _ := os.UserHomeDir()
	wd, _ := os.Getwd()
	c := &cli{out: os.Stdout, home: home, wd: wd}
	if dir, err := config.Dir(); err == nil {
		c.paths = config.Paths{Dir: dir}
	}
	return c
}

func (c *cli) ledger() *ticket.Ledger { return ticket.NewLedger(filepath.Join(c.paths.Dir, "tickets")) }

func (c *cli) machine() string {
	if c.paths.Dir != "" {
		if cfg, ok, err := c.paths.LoadConfig(); err == nil && ok {
			return cfg.Machine()
		}
	}
	return config.ShortHost()
}

// ticket runs `oversire ticket <skill|new|plan|work|done>`.
func (c *cli) ticket(args []string) error {
	if len(args) == 0 {
		return &exitError{2, fmt.Errorf("a ticket command is missing\n\n%s", ticketUsage)}
	}
	sub := args[0]
	fs := flag.NewFlagSet("ticket "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	session := fs.String("session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "the harness session id")
	harness := fs.String("harness", "", "the harness, or a comma-separated list for --install")
	install := fs.Bool("install", false, "install the skill file")
	refresh := fs.Bool("refresh", false, "update the installed skill files")
	out := fs.String("out", "", "the folder for the transcript files")
	file := fs.String("file", "", "the JSON file of the applied lines")
	project := fs.String("project", "", "the project identifier")
	root := fs.String("root", "", "the workspace root")
	stale := fs.Int("stale", 7, "the days with no work before a ticket is stalled")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(interleave(args[1:])); err != nil {
		return &exitError{2, fmt.Errorf("%w\n\n%s", err, ticketUsage)}
	}
	h := *harness
	if h == "" && !*install {
		h = digest.Claude
	}
	switch sub {
	case "skill":
		switch {
		case *install:
			return c.ticketInstall(h)
		case *refresh:
			return c.ticketRefresh(false)
		}
		if fs.NArg() > 1 {
			return &exitError{2, errors.New(ticketUsage)}
		}
		verb := fs.Arg(0)
		if verb == "" {
			_ = c.ticketRefresh(true)
		}
		return c.ticketSkill(verb, h, *session)
	case "new", "plan", "work":
		if fs.NArg() == 0 {
			return &exitError{2, fmt.Errorf("a ticket key is missing\n\n%s", ticketUsage)}
		}
		if sub != "plan" && fs.NArg() != 1 {
			return &exitError{2, fmt.Errorf("%s takes exactly one ticket key\n\n%s", sub, ticketUsage)}
		}
		return c.ticketBind(sub, fs.Args(), h, *session)
	case "groom":
		if fs.NArg() != 1 {
			return &exitError{2, fmt.Errorf("a project identifier is missing\n\n%s", ticketUsage)}
		}
		return c.ticketGroom(context.Background(), fs.Arg(0), *session)
	case "applied":
		if fs.NArg() != 1 || *file == "" {
			return &exitError{2, fmt.Errorf("give groom or sweep, and --file\n\n%s", ticketUsage)}
		}
		return c.ticketApplied(context.Background(), fs.Arg(0), *file, *project, h, *session)
	case "sweep":
		return c.ticketSweep(context.Background(), fs.Args(), *project, *root, *stale, *asJSON)
	case "stop":
		if fs.NArg() != 1 {
			return &exitError{2, fmt.Errorf("a session id is missing\n\n%s", ticketUsage)}
		}
		return c.ticketStop(context.Background(), fs.Arg(0), *session)
	case "done":
		if fs.NArg() > 1 {
			return &exitError{2, errors.New(ticketUsage)}
		}
		return c.ticketDone(fs.Arg(0), h, *session, *out)
	}
	return &exitError{2, fmt.Errorf("unknown ticket command %q\n\n%s", sub, ticketUsage)}
}

// interleave moves the flags before the arguments, so that `work KEY --session X` parses.
func interleave(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && i+1 < len(args) && takesValue(a) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, rest...)
}

func takesValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "session", "harness", "out", "file", "project", "root", "stale":
		return true
	}
	return false
}

func (c *cli) ticketSkill(verb, harness, session string) error {
	items, _ := c.ledger().Items(session)
	body, err := ticket.NewSkill().Body(verb, ticket.Context{
		Version: version, Machine: c.machine(), Cwd: c.wd, Session: session, Harness: harness, Items: items,
	})
	if err != nil {
		return &exitError{2, err}
	}
	_, err = fmt.Fprint(c.out, body)
	return err
}

func (c *cli) ticketInstall(harnesses string) error {
	var names []string
	for _, n := range strings.Split(harnesses, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	done, err := ticket.NewInstaller(c.home).Install(names...)
	c.printInstalled(done)
	return err
}

// ticketRefresh updates old skill files. Quiet: the skill body must stay clean, so it prints nothing.
func (c *cli) ticketRefresh(quiet bool) error {
	done, err := ticket.NewInstaller(c.home).Refresh()
	if !quiet {
		c.printInstalled(done)
	}
	return err
}

func (c *cli) printInstalled(done []ticket.Installed) {
	for _, d := range done {
		state := "up to date"
		if d.Changed {
			state = "written"
		}
		fmt.Fprintf(c.out, "%-7s %s (%s)\n", d.Harness, d.Path, state)
	}
}

// maxPlanKeys caps a batch plan.
const maxPlanKeys = 20

func (c *cli) ticketBind(role string, keys []string, harness, session string) error {
	var norm []string
	for _, k := range keys {
		k, err := ticket.NormalKey(k)
		if err != nil {
			return &exitError{2, err}
		}
		if !slices.Contains(norm, k) {
			norm = append(norm, k)
		}
	}
	if len(norm) > maxPlanKeys {
		return &exitError{2, fmt.Errorf("Plan at most %d tickets in one batch.", maxPlanKeys)}
	}
	l := c.ledger()
	earlier := make([][]ticket.SessionRecord, len(norm))
	for i, k := range norm {
		var err error
		if earlier[i], err = l.Sessions(k); err != nil {
			return err
		}
	}
	for _, key := range norm {
		if session == "" {
			fmt.Fprintf(c.out, "%s: no session id (set --session), so this session is not bound.\n", key)
			continue
		}
		r := ticket.SessionRecord{Key: key, Harness: harness, Session: session, Machine: c.machine(), Cwd: c.wd,
			Role: role, At: time.Now().UTC()}
		if err := l.Add(r); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s: this session (%s %s) is bound (%s).\n", key, harness, session, role)
	}
	if role == "new" {
		return nil
	}
	if len(norm) > 1 {
		fmt.Fprintln(c.out)
		for i, key := range norm {
			others := otherSessions(earlier[i], session)
			if len(others) == 0 {
				fmt.Fprintf(c.out, "%s: no earlier sessions on this machine\n", key)
				continue
			}
			s := others[len(others)-1]
			fmt.Fprintf(c.out, "%s: %d earlier sessions, last %s %s %s on %s %s\n", key, len(others), s.Role, s.Harness,
				s.Session, s.Machine, s.At.Local().Format("2006-01-02 15:04"))
		}
		return nil
	}
	others := otherSessions(earlier[0], session)
	if len(others) == 0 {
		fmt.Fprintln(c.out, "No earlier sessions on this machine.")
		return nil
	}
	fmt.Fprintln(c.out, "\nEarlier sessions:")
	for _, s := range others {
		fmt.Fprintf(c.out, "- %s  %-4s  %s %s  on %s  in %s\n", s.At.Local().Format("2006-01-02 15:04"), s.Role,
			s.Harness, s.Session, s.Machine, s.Cwd)
	}
	c.printDigest(others[len(others)-1])
	return nil
}

func otherSessions(rs []ticket.SessionRecord, session string) []ticket.SessionRecord {
	var out []ticket.SessionRecord
	for _, r := range rs {
		if r.Session != session {
			out = append(out, r)
		}
	}
	return out
}

// printDigest prints the digest of a session as text, or how to get it when its transcript is not here.
func (c *cli) printDigest(s ticket.SessionRecord) {
	b := wire.NewHistoryBudget(wire.DefaultEarlier, wire.DefaultFull, wire.DefaultMaxChars)
	r, err := digest.ForHarness(s.Harness, c.home)
	var d wire.SessionDigest
	if err == nil {
		d, err = r.Digest(s.Session, &b, wire.DigestFromFile)
	}
	if err != nil {
		fmt.Fprintf(c.out, "\nNo digest of session %s here: %v\n", s.Session, err)
		if s.Machine != c.machine() {
			fmt.Fprintf(c.out, "Its transcript is on %s. Get it with: ssh %s oversire digest --harness %s --session %s\n",
				s.Machine, s.Machine, s.Harness, s.Session)
		}
		return
	}
	fmt.Fprintf(c.out, "\nDigest of session %s (%d turns):\n", s.Session, d.Turns)
	if len(d.Earlier) > 0 {
		fmt.Fprintln(c.out, "\nEarlier turns:")
		for _, t := range d.Earlier {
			fmt.Fprintf(c.out, "- %s\n  → %s\n", oneLine(t.Prompt, 300), oneLine(t.Closing, 300))
		}
	}
	for _, t := range d.Last {
		fmt.Fprintf(c.out, "\n### Prompt\n%s\n\n### Answer (%d tools)\n%s\n", t.Prompt, t.Tools, t.Text)
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (c *cli) ticketDone(key, harness, session, out string) error {
	l := c.ledger()
	if key == "" {
		items, err := l.Items(session)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			return &exitError{2, fmt.Errorf("give the ticket key: this session has %d open tickets (%s)", len(items),
				strings.Join(items, ", "))}
		}
		key = items[0]
	}
	key, err := ticket.NormalKey(key)
	if err != nil {
		return &exitError{2, err}
	}
	if session == "" {
		return &exitError{2, errors.New("no session id: set --session")}
	}
	r, err := digest.ForHarness(harness, c.home)
	if err != nil {
		return err
	}
	finder, ok := r.(interface{ Find(string) (string, error) })
	if !ok {
		return fmt.Errorf("no transcript file for harness %s", harness)
	}
	src, err := finder.Find(session)
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join(os.TempDir(), "oversire-ticket", key)
	}
	paths, err := ticket.Packer{Max: ticket.MaxAttachment}.Pack(src, out, harness+"-session-"+session)
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Fprintf(c.out, "%s\t%s\n", p, filepath.Base(p))
	}
	if err := c.packWorkerTurns(context.Background(), key, out); err != nil {
		return err
	}
	return l.Close(key, session)
}

// turnsPage is the page size of the worker turns. A page stays below the 1 MiB answer limit of daemonClient.
const turnsPage = 200

// packWorkerTurns packs the turns of the worker of key as <key>-worker-turns. Without a daemon or a worker,
// it prints one line and goes on: the key of a stopped worker stays in the worker record.
func (c *cli) packWorkerTurns(ctx context.Context, key, out string) error {
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	rec, err := d.Worker(ctx, key)
	if err != nil {
		return c.noWorker(key, err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for after := 0; ; {
		page, err := d.Turns(ctx, rec.Session, after, turnsPage)
		if err != nil {
			return c.noWorker(key, err)
		}
		for _, t := range page.Turns {
			if err := enc.Encode(t); err != nil {
				return err
			}
			after = max(after, t.Seq)
		}
		if !page.More || len(page.Turns) == 0 {
			break
		}
	}
	paths, err := ticket.Packer{Max: ticket.MaxAttachment}.PackFrom(&buf, out, key+"-worker-turns")
	if err != nil {
		return err
	}
	for _, p := range paths {
		fmt.Fprintf(c.out, "%s\t%s\n", p, filepath.Base(p))
	}
	return nil
}

// noWorker turns a missing daemon or a missing worker into one line. Another error stays an error.
func (c *cli) noWorker(key string, err error) error {
	if de, ok := errors.AsType[*daemonError](err); ok && (de.Status == 0 || de.Status == http.StatusNotFound) {
		fmt.Fprintf(c.out, "No worker turns for %s: %s\n", key, de.Msg)
		return nil
	}
	return err
}

func (c *cli) ticketGroom(ctx context.Context, project, session string) error {
	p, err := ticket.NormalProject(project)
	if err != nil {
		return &exitError{2, err}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	g, err := ticket.CheckGroom(ctx, d, p, session)
	if err != nil {
		return err
	}
	if !g.Daemon {
		fmt.Fprintln(c.out, "No daemon answers: the Groom check did not run.")
		return nil
	}
	if h := g.Holder; h != nil {
		return &exitError{1, fmt.Errorf("A Groom session of %s runs on %s: %s (%s, %s since %s). Finish it in OverSire, or stop it first.",
			p, h.Machine, h.ID, h.Runtime, h.State, h.StateSince.Local().Format("2006-01-02 15:04"))}
	}
	fmt.Fprintf(c.out, "%s: no live Groom session (key %s is free).\n", p, g.Key)
	if len(g.Busy) == 0 {
		fmt.Fprintln(c.out, "Items with a live session: none.")
		return nil
	}
	keys := slices.Sorted(maps.Keys(g.Busy))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		s := g.Busy[k][0]
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", k, sessionRole(s), s.State))
	}
	fmt.Fprintf(c.out, "Items with a live session (skip them): %s\n", strings.Join(parts, ", "))
	return nil
}

// sessionRole is the start key prefix of a session, else its runtime.
func sessionRole(s wire.DaemonSession) string {
	if r, _, ok := strings.Cut(s.Key, ":"); ok {
		return r
	}
	return s.Runtime
}

func (c *cli) ticketApplied(ctx context.Context, verb, file, project, harness, session string) error {
	if verb != "groom" && verb != "sweep" {
		return &exitError{2, fmt.Errorf("%q: give groom or sweep", verb)}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return &exitError{2, err}
	}
	var lines []ticket.AppliedLine
	if err := json.Unmarshal(data, &lines); err != nil {
		return &exitError{2, fmt.Errorf("%s: %w", file, err)}
	}
	rows, err := ticket.AppliedDrafts(verb+".applied", project, lines,
		ticket.Actor{Harness: harness, Session: session, Machine: c.machine()}, time.Now())
	if err != nil {
		return &exitError{2, err}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if _, err := d.AppendEvents(ctx, rows); err != nil {
		if errors.Is(err, ticket.ErrNoDaemon) {
			fmt.Fprintln(c.out, "No daemon answers: the event rows are not written.")
			return nil
		}
		return err
	}
	fmt.Fprintf(c.out, "%d event rows written\n", len(rows))
	return nil
}

func (c *cli) ticketSweep(ctx context.Context, keys []string, project, root string, staleDays int, asJSON bool) error {
	if project != "" {
		p, err := ticket.NormalProject(project)
		if err != nil {
			return &exitError{2, err}
		}
		project = p
	}
	if root == "" {
		root = c.sweepRoot()
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if staleDays < 1 {
		return &exitError{2, errors.New("--stale must be 1 or more days")}
	}
	s := ticket.Sweeper{Daemon: d, Git: review.NewGit(proc.Local{}), Ledger: c.ledger(), Root: root,
		Stale: time.Duration(staleDays) * 24 * time.Hour}
	rep, err := s.Sweep(ctx, keys, project)
	if err != nil {
		return &exitError{2, err}
	}
	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	tw := tabwriter.NewWriter(c.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSIGNAL\tREVIEW\tSESSIONS\tGROUP\tBASE\tLAST")
	for _, r := range rep.Rows {
		live := 0
		for _, s := range r.Sessions {
			if s.Live {
				live++
			}
		}
		group, last := "-", "-"
		if r.Group != "" {
			group = "yes"
		}
		if r.LastWork != nil {
			last = r.LastWork.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%d\t%s\n", r.Key, r.Signal, cmp.Or(string(r.Review), "-"), live, len(r.Sessions),
			group, len(r.OnBase), last)
	}
	tw.Flush()
	for _, w := range rep.Warnings {
		fmt.Fprintf(c.out, "warning: %s\n", w)
	}
	return nil
}

// sweepRoot is the root of the group of the folder, else the first parent with a .wt folder, else the folder.
func (c *cli) sweepRoot() string {
	if root, _, ok := ticket.GroupOf(c.wd); ok {
		return root
	}
	for d := filepath.Clean(c.wd); ; d = filepath.Dir(d) {
		if isDir(filepath.Join(d, ".wt")) {
			return d
		}
		if filepath.Dir(d) == d {
			return c.wd
		}
	}
}

func (c *cli) ticketStop(ctx context.Context, ref, session string) error {
	if session != "" && ref == session {
		return &exitError{2, errors.New("This is the session of the caller. Do not stop it.")}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	list, err := d.Sessions(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(s wire.DaemonSession) bool {
		return s.ID == ref || s.HarnessSession != "" && s.HarnessSession == ref || s.Terminal != "" && s.Terminal == ref
	})
	if i < 0 {
		return &exitError{2, fmt.Errorf("The daemon has no session %s.", ref)}
	}
	s := list[i]
	switch {
	case session != "" && s.HarnessSession == session:
		return &exitError{2, errors.New("This is the session of the caller. Do not stop it.")}
	case s.Runtime == wire.RuntimeCLI:
		return &exitError{2, errors.New("The daemon does not own this terminal session. Close it there.")}
	}
	if err := d.Stop(ctx, s); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%s: stopped\n", s.ID)
	return nil
}
