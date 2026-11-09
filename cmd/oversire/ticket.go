package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/digest"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

const ticketUsage = `Usage:
  oversire ticket skill [new|plan|work|done|spec]          print the body of the oversire agent skill
  oversire ticket skill --install|--refresh [--harness claude,codex]
                                                          install the skill file, or update the installed files
  oversire ticket new|work <KEY> [--session ID] [--harness H]
                                                          bind this session to a ticket (work prints the earlier sessions)
  oversire ticket plan <KEY>... [--session ID] [--harness H]
                                                          bind this session to one ticket or a batch (at most 20)
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
	case "session", "harness", "out":
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
	return l.Close(key, session)
}
