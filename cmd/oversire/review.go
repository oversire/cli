package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/oversire/cli/review"
	"github.com/oversire/cli/wire"
)

const reviewUsage = `usage:
  oversire review ready [--key K] [--kind code|plan] [--note <text>] [--plan <file.md>]
  oversire review reply [--key K] <comment id> "<text>"
  oversire review ls [--kind code|plan] [--json]`

// review is the hand-off of an agent to the review gate of the daemon, through the loopback API.
func (c *cli) review(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return &exitError{2, errors.New(reviewUsage)}
	}
	switch args[0] {
	case "ready":
		return c.reviewReady(ctx, args[1:])
	case "reply":
		return c.reviewReply(ctx, args[1:])
	case "ls":
		return c.reviewList(ctx, args[1:])
	}
	return &exitError{2, errors.New(reviewUsage)}
}

func (c *cli) reviewReady(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review ready", flag.ContinueOnError)
	fs.SetOutput(c.out)
	key := fs.String("key", "", "the item key; empty is the group of the working folder")
	kind := fs.String("kind", "", "code or plan; empty is plan with --plan, else code")
	note := fs.String("note", "", "one line for the user (300 characters at most)")
	planFile := fs.String("plan", "", "the Markdown file of the plan, for --kind plan")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	k := wire.ReviewKind(*kind)
	if k == "" {
		k = wire.ReviewCode
		if *planFile != "" {
			k = wire.ReviewPlan
		}
	}
	if fs.NArg() > 0 || k != wire.ReviewCode && k != wire.ReviewPlan || (k == wire.ReviewPlan) != (*planFile != "") {
		return &exitError{2, errors.New(reviewUsage)}
	}
	wd, err := c.workdir()
	if err != nil {
		return err
	}
	root, name, inGroup := groupOf(wd)
	if *key == "" && inGroup {
		*key = name
	}
	if *key == "" {
		return &exitError{2, errors.New("Give the item key with --key, or run the command in a worktree group .wt/<key>.")}
	}
	K := strings.ToUpper(*key)
	if r := []rune(*note); len(r) > review.MaxNote {
		*note = string(r[:review.MaxNote])
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	path := "/v1/reviews/" + url.PathEscape(K)
	rd := wire.ReviewReady{Kind: k, Note: *note}
	if k == wire.ReviewPlan {
		// The plan session works at the root: the folder of the group, else the working folder.
		if !inGroup {
			root = wd
		}
		file := *planFile
		if !filepath.IsAbs(file) {
			file = filepath.Join(wd, file)
		}
		text, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var p wire.PlanDoc
		if err := d.call(ctx, "PUT", path+"/plan", parsePlan(K, root, string(text)), &p); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "Plan of %s: version %d, %d blocks.\n", K, p.Version, len(p.Blocks))
	} else {
		switch {
		case inGroup && strings.EqualFold(name, K):
			rd.Root = root
		case isDir(filepath.Join(wd, ".wt", strings.ToLower(K))):
			rd.Root = wd
		case inGroup:
			if isDir(filepath.Join(root, ".wt", strings.ToLower(K))) {
				rd.Root = root
			}
		}
	}
	var s wire.ReviewSummary
	if err := d.call(ctx, "POST", path+"/ready", rd, &s); err != nil {
		return err
	}
	what := K
	if k == wire.ReviewPlan {
		what = "the plan of " + K
	}
	fmt.Fprintf(c.out, "Review of %s is ready. The user reviews it in OverSire. Do not merge.\n", what)
	if s.Unanswered == 0 {
		return nil
	}
	var det wire.ReviewDetail
	if err := d.call(ctx, "GET", path+"?kind="+string(k), nil, &det); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "%d comments have no answer. Answer each one:\n", s.Unanswered)
	for _, cm := range det.Comments {
		if cm.State == wire.CommentSubmitted {
			fmt.Fprintf(c.out, "  %s %s \"<what you did>\"\n", review.ReplyCommand(K), cm.ID)
		}
	}
	return nil
}

func (c *cli) reviewReply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review reply", flag.ContinueOnError)
	fs.SetOutput(c.out)
	key := fs.String("key", "", "the item key; empty is the group of the working folder, else the review with the comment")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	if fs.NArg() < 2 {
		return &exitError{2, errors.New(reviewUsage)}
	}
	id, text := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	if r := []rune(text); len(r) > review.MaxReply {
		text = string(r[:review.MaxReply])
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	if *key == "" {
		if wd, err := c.workdir(); err == nil {
			if _, name, ok := groupOf(wd); ok {
				*key = name
			}
		}
	}
	if *key == "" {
		if *key, err = findComment(ctx, d, id); err != nil {
			return err
		}
	}
	K := strings.ToUpper(*key)
	var cm wire.ReviewComment
	path := "/v1/reviews/" + url.PathEscape(K) + "/comments/" + url.PathEscape(id) + "/reply"
	if err := d.call(ctx, "POST", path, wire.CommentReply{Text: text}, &cm); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Comment %s of %s: answered.\n", cm.ID, K)
	return nil
}

// findComment gives the key of the review that has the comment id.
func findComment(ctx context.Context, d daemonClient, id string) (string, error) {
	var page wire.ReviewsPage
	if err := d.call(ctx, "GET", "/v1/reviews", nil, &page); err != nil {
		return "", err
	}
	for _, s := range page.Reviews {
		var det wire.ReviewDetail
		if err := d.call(ctx, "GET", "/v1/reviews/"+url.PathEscape(s.Key)+"?kind="+string(s.Kind), nil, &det); err != nil {
			continue
		}
		for _, cm := range det.Comments {
			if cm.ID == id {
				return s.Key, nil
			}
		}
	}
	return "", fmt.Errorf("No review has the comment %s. Give the key with --key.", id)
}

func (c *cli) reviewList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review ls", flag.ContinueOnError)
	fs.SetOutput(c.out)
	kind := fs.String("kind", "", "code or plan; empty is both")
	asJSON := fs.Bool("json", false, "print the page as JSON")
	if err := fs.Parse(args); err != nil {
		return &exitError{2, err}
	}
	if fs.NArg() > 0 {
		return &exitError{2, errors.New(reviewUsage)}
	}
	d, err := c.daemonClient()
	if err != nil {
		return err
	}
	path := "/v1/reviews"
	if *kind != "" {
		path += "?kind=" + url.QueryEscape(*kind)
	}
	var page wire.ReviewsPage
	if err := d.call(ctx, "GET", path, nil, &page); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(page)
	}
	if len(page.Reviews) == 0 {
		fmt.Fprintln(c.out, "No reviews.")
	} else {
		tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tKIND\tSTATE\tROUND\tPENDING\tOPEN\tTITLE")
		for _, s := range page.Reviews {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", s.Key, s.Kind, s.State, s.Round, s.Pending, s.Unanswered, s.Title)
		}
		tw.Flush()
	}
	for _, np := range page.NotPushed {
		fmt.Fprintf(c.out, "Not pushed: %s/%s %s has %d commits.\n", np.Root, np.Repo, np.Base, np.Ahead)
	}
	return nil
}

// workdir is the working folder of the command.
func (c *cli) workdir() (string, error) {
	if c.wd != "" {
		return c.wd, nil
	}
	return os.Getwd()
}

// groupOf finds the worktree group <root>/.wt/<name> that holds dir.
func groupOf(dir string) (root, name string, ok bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		parent := filepath.Dir(d)
		if filepath.Base(parent) == ".wt" {
			return filepath.Dir(parent), filepath.Base(d), true
		}
		if parent == d {
			return "", "", false
		}
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

var (
	slugDrop    = regexp.MustCompile(`[^a-z0-9]+`)
	subitemLine = regexp.MustCompile(`^\s*[-*]\s+(?:\[[ xX]\]\s+)?(.+)$`)
	subitemKey  = regexp.MustCompile(`^([A-Z][A-Z0-9]*-\d+)\s*[:\-–—]?\s+(.+)$`)
	subitemHead = regexp.MustCompile(`(?i)^sub-?\s?items?$`)
)

// parsePlan splits a Markdown plan at each ## heading. The # heading is the title. The text before the first ##
// heading is the block "Summary". Each list line of a block "Sub-items" is a sub-item, with an optional key.
func parsePlan(key, root, text string) wire.PlanDoc {
	p := wire.PlanDoc{Key: key, Version: 1, Root: root, Blocks: []wire.PlanBlock{}}
	var heading string
	var body []string
	seen := map[string]int{}
	flush := func() {
		md := strings.TrimSpace(strings.Join(body, "\n"))
		if heading == "" && md == "" {
			return
		}
		h := heading
		if h == "" {
			h = "Summary"
		}
		id := strings.Trim(slugDrop.ReplaceAllString(strings.ToLower(h), "-"), "-")
		if id == "" {
			id = "block"
		}
		if seen[id]++; seen[id] > 1 {
			id = fmt.Sprintf("%s-%d", id, seen[id])
		}
		p.Blocks = append(p.Blocks, wire.PlanBlock{ID: id, Heading: h, Markdown: md})
		if subitemHead.MatchString(h) {
			for _, l := range strings.Split(md, "\n") {
				m := subitemLine.FindStringSubmatch(l)
				if m == nil {
					continue
				}
				it := wire.PlanSubitem{Title: strings.TrimSpace(m[1])}
				if km := subitemKey.FindStringSubmatch(it.Title); km != nil {
					it.Key, it.Title = km[1], strings.TrimSpace(km[2])
				}
				p.Subitems = append(p.Subitems, it)
			}
		}
	}
	fence := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
		}
		switch {
		case !fence && strings.HasPrefix(l, "# ") && p.Title == "" && heading == "" && len(p.Blocks) == 0:
			p.Title = strings.TrimSpace(l[2:])
		case !fence && strings.HasPrefix(l, "## "):
			flush()
			heading, body = strings.TrimSpace(l[3:]), nil
		default:
			body = append(body, l)
		}
	}
	flush()
	return p
}
