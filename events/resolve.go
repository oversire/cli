package events

import (
	"context"
	"regexp"
	"strings"
)

// Hint holds what a source knows about the place of a change. Each field is optional.
type Hint struct {
	SessionKey string // work:KEY, plan:KEY, refine:KEY, runbook:<run>.<step>.<round>
	Path       string // a cwd or a worktree path: .wt/<key>/ names the item
	Branch     string
	Message    string // a commit message: the trailer Item: KEY
}

var (
	sessionKey = regexp.MustCompile(`^(?:work|plan|refine|enhance|fix|review):([A-Za-z0-9][A-Za-z0-9_]*-\d+)`)
	wtPath     = regexp.MustCompile(`/\.wt/([A-Za-z][A-Za-z0-9_]*-\d+)(?:/|$)`)
	branchKey  = regexp.MustCompile(`(?:^|[/_])([A-Za-z][A-Za-z0-9]*-\d+)(?:$|[-_/])`)
	trailer    = regexp.MustCompile(`(?m)^Item:\s*([A-Za-z][A-Za-z0-9_]*-\d+)\s*$`)
)

// Resolver gives the item and the parent of a row (OVERSIRE-377): the session key, then the worktree path,
// then the branch, then the commit trailer. The parent comes from item_links.
type Resolver struct {
	store *Store
	runs  func(run string) string // the item key of a runbook run
}

func NewResolver(store *Store, runs func(run string) string) *Resolver {
	return &Resolver{store: store, runs: runs}
}

// Item gives the item key of h, or "".
func (r *Resolver) Item(h Hint) string {
	if m := sessionKey.FindStringSubmatch(h.SessionKey); m != nil {
		return strings.ToUpper(m[1])
	}
	if run, ok := strings.CutPrefix(h.SessionKey, "runbook:"); ok && r != nil && r.runs != nil {
		if id, _, _ := strings.Cut(run, "."); id != "" {
			if k := r.runs(id); k != "" {
				return strings.ToUpper(k)
			}
		}
	}
	if m := wtPath.FindStringSubmatch(h.Path); m != nil {
		return strings.ToUpper(m[1])
	}
	if m := branchKey.FindStringSubmatch(h.Branch); m != nil {
		return strings.ToUpper(m[1])
	}
	if m := trailer.FindStringSubmatch(h.Message); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// Fill sets item and parent on a draft that has none.
func (r *Resolver) Fill(ctx context.Context, item, parent *string, h Hint) {
	if *item == "" {
		*item = r.Item(h)
	}
	if *item != "" && *parent == "" && r != nil && r.store != nil {
		*parent = r.store.Parent(ctx, *item)
	}
}
