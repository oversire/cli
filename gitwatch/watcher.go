// Package gitwatch makes event rows from git with no git hooks (OVERSIRE-381): worktrees, commits, rebases, resets,
// merges and pushes. It reads the reflogs of the branches. kqueue tells when a ref folder changes.
package gitwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/wire"
)

const (
	debounce = 2 * time.Second
	rescan   = time.Minute
	maxRead  = 1 << 20
)

// Git runs git in a folder (review.Git).
type Git interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// Sessions gives the sessions of the book. A working session in a worktree makes its git rows agent rows.
type Sessions interface {
	All() []wire.DaemonSession
}

// Adder takes the rows (capture.Writer).
type Adder interface {
	Add(session string, d wire.EventDraft)
}

// Items gives the item key of a place (events.Resolver).
type Items interface {
	Item(h events.Hint) string
}

type notifier interface {
	Changed() <-chan struct{}
	Watch(dirs []string)
	Run(ctx context.Context)
}

// state is kept in <data>/gitwatch.json.
type state struct {
	Offsets map[string]int64      `json:"offsets"` // reflog file → the bytes read
	Repos   map[string][]worktree `json:"repos"`   // common git folder → its worktrees
}

type Watcher struct {
	git      Git
	sessions Sessions
	out      Adder
	items    Items
	roots    func(ctx context.Context) []string
	path     string
	log      *slog.Logger
	clock    func() time.Time
	notify   notifier

	mu    sync.Mutex
	state state
	paths []string // the checkouts of the last read of the roots
}

// NewWatcher reads the cursors in path. roots gives the checkouts and the folders that hold checkouts.
func NewWatcher(git Git, sessions Sessions, out Adder, items Items, roots func(context.Context) []string, path string, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	w := &Watcher{git: git, sessions: sessions, out: out, items: items, roots: roots, path: path, log: log, clock: time.Now,
		state: state{Offsets: map[string]int64{}, Repos: map[string][]worktree{}}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &w.state); err != nil {
			log.Warn("gitwatch: read the cursors", "err", err)
		}
	}
	if w.state.Offsets == nil {
		w.state.Offsets = map[string]int64{}
	}
	if w.state.Repos == nil {
		w.state.Repos = map[string][]worktree{}
	}
	return w
}

// Run reads at the start, 2 s after a ref folder changes, and each minute.
func (w *Watcher) Run(ctx context.Context) error {
	n, err := newNotifier()
	if err != nil {
		w.log.Warn("gitwatch: no file events; a read each 2 s", "err", err)
		n = &pollNotifier{ch: make(chan struct{}, 1)}
	}
	w.notify = n
	go n.Run(ctx)
	w.Refresh(ctx)
	w.Scan(ctx)
	tick := time.NewTicker(rescan)
	defer tick.Stop()
	var later <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-n.Changed():
			if later == nil {
				later = time.After(debounce)
			}
		case <-later:
			later = nil
			w.Scan(ctx)
		case <-tick.C:
			w.Refresh(ctx)
			w.Scan(ctx)
		}
	}
}

// Refresh reads the roots again. A root can call a runtime CLI, so this runs only each minute.
func (w *Watcher) Refresh(ctx context.Context) {
	paths := w.checkouts(ctx)
	w.mu.Lock()
	w.paths = paths
	w.mu.Unlock()
}

// Scan reads the new reflog lines and the worktree changes of each repository and writes their rows.
// The first read of a repository only sets the cursors.
func (w *Watcher) Scan(ctx context.Context) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	rows, changed := 0, false
	var dirs []string
	seen := map[string]bool{}
	if w.paths == nil {
		w.paths = w.checkouts(ctx)
	}
	for _, path := range w.paths {
		r, ok := openRepo(path)
		if !ok || seen[r.gitdir] {
			continue
		}
		seen[r.gitdir] = true
		dirs = append(dirs, r.dirs()...)
		old, known := w.state.Repos[r.gitdir]
		if known {
			rows += w.worktreeRows(old, r.worktrees)
		}
		if !known || !sameWorktrees(old, r.worktrees) {
			w.state.Repos[r.gitdir] = append([]worktree(nil), r.worktrees...)
			changed = true
		}
		for ref, file := range r.logs() {
			n, moved := w.readLog(ctx, r, ref, file, known)
			rows += n
			changed = changed || moved
		}
	}
	for file := range w.state.Offsets {
		if _, err := os.Stat(file); err != nil {
			delete(w.state.Offsets, file)
			changed = true
		}
	}
	if w.notify != nil {
		w.notify.Watch(dirs)
	}
	if changed {
		w.save()
	}
	return rows
}

// checkouts expands the roots: a root with .git is a checkout, else each child folder with .git is one.
func (w *Watcher) checkouts(ctx context.Context) []string {
	var out []string
	for _, root := range w.roots(ctx) {
		if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
			out = append(out, root)
			continue
		}
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			p := filepath.Join(root, e.Name())
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// readLog writes a row for each new line of one reflog. A new file of a known repository is read from the start.
func (w *Watcher) readLog(ctx context.Context, r repo, ref, file string, known bool) (int, bool) {
	fi, err := os.Stat(file)
	if err != nil {
		return 0, false
	}
	off, has := w.state.Offsets[file]
	if !has && !known {
		w.state.Offsets[file] = fi.Size()
		return 0, true
	}
	if fi.Size() < off {
		off = 0 // git rewrote the reflog; the ids drop the rows that came before
	}
	if fi.Size() == off {
		if !has {
			w.state.Offsets[file] = off
			return 0, true
		}
		return 0, false
	}
	f, err := os.Open(file)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, off, fi.Size()-off), maxRead))
	if err != nil {
		return 0, false
	}
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		return 0, false
	}
	rows := 0
	for line := range bytes.SplitSeq(b[:end], []byte("\n")) {
		if e, ok := parseEntry(line); ok && w.row(ctx, r, ref, e) {
			rows++
		}
	}
	w.state.Offsets[file] = off + int64(end) + 1
	return rows, true
}

// row maps one reflog line to a row. Lines of other kinds (branch, fetch, checkout) give none.
func (w *Watcher) row(ctx context.Context, r repo, ref string, e entry) bool {
	var branch, remote string
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		branch = b
	} else if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		remote, branch, _ = strings.Cut(rest, "/")
	}
	if branch == "" || branch == "HEAD" || zero(e.new) {
		return false
	}
	msg := e.message
	p := map[string]any{"sha": e.new, "ref": ref}
	var kind, summary string
	switch {
	case remote != "":
		if !strings.HasPrefix(msg, "update by push") {
			return false
		}
		kind, summary = wire.KindGitPush, fmt.Sprintf("Pushed %s to %s", branch, remote)
		p["remote"] = remote
	case strings.HasPrefix(msg, "commit (merge)"):
		kind, summary = wire.KindGitMerge, ""
		p["mode"] = "merge"
	case strings.HasPrefix(msg, "commit") || strings.HasPrefix(msg, "cherry-pick") || strings.HasPrefix(msg, "revert"):
		kind = wire.KindGitCommit
	case strings.HasPrefix(msg, "rebase") && strings.Contains(msg, "(finish)"):
		kind, summary = wire.KindGitRebase, "Rebased "+branch
		if _, onto, ok := strings.Cut(msg, " onto "); ok {
			p["onto"] = strings.TrimSpace(onto)
			summary += " onto " + short(strings.TrimSpace(onto))
		}
	case strings.HasPrefix(msg, "merge ") || strings.HasPrefix(msg, "pull"):
		kind = wire.KindGitMerge
		from := "pull"
		if f, _, ok := strings.Cut(strings.TrimPrefix(msg, "merge "), ":"); ok && strings.HasPrefix(msg, "merge ") {
			from = f
		}
		p["branch"] = from
		p["mode"] = "merge"
		if strings.Contains(msg, "Fast-forward") {
			p["mode"] = "fastForward"
		} else if strings.Contains(msg, "rebase") {
			p["mode"] = "rebase"
		}
		summary = fmt.Sprintf("Merged %s into %s", from, branch)
	case strings.HasPrefix(msg, "reset: moving to "):
		to := strings.TrimPrefix(msg, "reset: moving to ")
		kind, summary = wire.KindGitReset, fmt.Sprintf("Reset %s to %s", branch, short(to))
		p["to"] = to
	default:
		return false
	}
	wt := r.worktreeOf(branch)
	dir := cmpOr(wt, r.gitdir)
	body, _ := w.git.Run(ctx, dir, "log", "-1", "--format=%B", e.new)
	subject, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if subject != "" {
		p["subject"] = subject
	}
	if summary == "" {
		summary = cmpOr(subject, "Commit on "+branch)
	}
	if wt != "" {
		p["worktree"] = wt
	}
	w.numstat(ctx, dir, e, p)
	if (kind == wire.KindGitMerge || kind == wire.KindGitPush) && !zero(e.old) {
		if n, err := w.git.Run(ctx, dir, "rev-list", "--count", e.old+".."+e.new); err == nil {
			if c, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				p["commits"] = c
			}
		}
	}
	d := wire.EventDraft{ID: "git:" + e.new + ":" + ref, At: e.at, Kind: kind, ActorType: wire.ActorHuman, Actor: e.email, Via: "git", Summary: summary}
	if d.At.IsZero() {
		d.At = w.clock()
	}
	var sess wire.DaemonSession
	if s, ok := w.agent(wt); ok {
		sess = s
		d.ActorType, d.Actor, d.Via = wire.ActorAgent, "", "git"
	}
	d.Payload, _ = json.Marshal(p)
	if w.items != nil {
		h := events.Hint{SessionKey: sess.Key, Path: wt, Branch: branch, Message: body}
		if from, _ := p["branch"].(string); kind == wire.KindGitMerge && from != "" && from != "pull" {
			h.Branch = from // the merged branch names the item, not the base
		}
		d.Item = w.items.Item(h)
	}
	w.out.Add(sess.ID, d)
	return true
}

// numstat adds files, add and del: the change from old to new, or of the commit alone for a new ref.
func (w *Watcher) numstat(ctx context.Context, dir string, e entry, p map[string]any) {
	args := []string{"diff", "--numstat", e.old, e.new}
	if zero(e.old) {
		args = []string{"show", "--numstat", "--format=", e.new}
	}
	out, err := w.git.Run(ctx, dir, args...)
	if err != nil {
		return
	}
	files, add, del := 0, 0, 0
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		files++
		a, _ := strconv.Atoi(f[0]) // "-" for a binary file
		r, _ := strconv.Atoi(f[1])
		add, del = add+a, del+r
	}
	p["files"], p["add"], p["del"] = files, add, del
}

// agent gives a working session in the worktree.
func (w *Watcher) agent(wt string) (wire.DaemonSession, bool) {
	if wt == "" || w.sessions == nil {
		return wire.DaemonSession{}, false
	}
	for _, s := range w.sessions.All() {
		if s.State == wire.PhaseWorking && (s.Cwd == wt || strings.HasPrefix(s.Cwd, wt+string(filepath.Separator))) {
			return s, true
		}
	}
	return wire.DaemonSession{}, false
}

func (w *Watcher) worktreeRows(old, now []worktree) int {
	rows := 0
	had := map[string]worktree{}
	for _, t := range old {
		had[t.Path] = t
	}
	has := map[string]bool{}
	for _, t := range now {
		has[t.Path] = true
		if _, ok := had[t.Path]; ok {
			continue
		}
		at := t.at
		if at.IsZero() {
			at = w.clock()
		}
		w.worktreeRow(t, wire.KindWorktreeAdded, at, "Added worktree "+filepath.Base(t.Path))
		rows++
	}
	for _, t := range old {
		if !has[t.Path] {
			w.worktreeRow(t, wire.KindWorktreeRemoved, w.clock(), "Removed worktree "+filepath.Base(t.Path))
			rows++
		}
	}
	return rows
}

func (w *Watcher) worktreeRow(t worktree, kind string, at time.Time, summary string) {
	verb := strings.TrimPrefix(kind, "worktree.")
	d := wire.EventDraft{ID: "worktree:" + t.Path + ":" + verb + ":" + strconv.FormatInt(at.UnixMilli(), 10), At: at, Kind: kind,
		ActorType: wire.ActorHuman, Via: "git", Summary: summary}
	p := map[string]any{"worktree": t.Path}
	if t.Branch != "" {
		p["branch"] = t.Branch
		summary += " on " + t.Branch
	}
	d.Summary = summary
	d.Payload, _ = json.Marshal(p)
	if w.items != nil {
		d.Item = w.items.Item(events.Hint{Path: t.Path, Branch: t.Branch})
	}
	w.out.Add("", d)
}

func (w *Watcher) save() {
	b, err := json.Marshal(w.state)
	if err != nil {
		return
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		w.log.Warn("gitwatch: save the cursors", "err", err)
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		w.log.Warn("gitwatch: save the cursors", "err", err)
	}
}

func sameWorktrees(a, b []worktree) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Branch != b[i].Branch {
			return false
		}
	}
	return true
}

func short(sha string) string {
	if len(sha) > 7 && strings.Trim(sha, "0123456789abcdef") == "" {
		return sha[:7]
	}
	return sha
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
