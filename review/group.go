package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

// MaxCommits is the number of commits in a ReviewRepo.
const MaxCommits = 50

var (
	keyPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]*$`)
	itemLine   = regexp.MustCompile(`(?m)^Item:\s*(.+)$`)
	textKey    = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*-\d+\b`)
	branchKey  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9_]*-\d+\b`)
)

// validKey tells if key is a review key: upper case, at most 60 characters.
func validKey(key string) bool { return len(key) <= 60 && keyPattern.MatchString(key) }

// group is a worktree group <root>/.wt/<name>. A repo of the group is a git worktree (its .git is a file).
// A link is a symlink, for example to a shared file of the root. Other entries do not count.
type group struct {
	Root, Name string
	Repos      []string
	Links      []string
}

func (g group) Key() string { return strings.ToUpper(g.Name) }
func (g group) Dir() string { return filepath.Join(g.Root, ".wt", g.Name) }
func (g group) Rel() string { return ".wt/" + g.Name }

// worktree is the folder of the repo name in the group. primary is the main checkout of it in the root.
func (g group) worktree(name string) string { return filepath.Join(g.Dir(), name) }
func (g group) primary(name string) string  { return filepath.Join(g.Root, name) }

// readGroup reads <root>/.wt/<name>. ok is false when it is not a folder.
func readGroup(root, name string) (group, bool) {
	g := group{Root: root, Name: name}
	entries, err := os.ReadDir(g.Dir())
	if err != nil {
		return g, false
	}
	for _, e := range entries {
		switch {
		case e.Type()&os.ModeSymlink != 0:
			g.Links = append(g.Links, e.Name())
		case e.IsDir():
			if st, err := os.Lstat(filepath.Join(g.Dir(), e.Name(), ".git")); err == nil && st.Mode().IsRegular() {
				g.Repos = append(g.Repos, e.Name())
			}
		}
	}
	return g, true
}

// scanGroups gives the groups of each root whose name is a valid key in upper case.
func scanGroups(roots []string) []group {
	var out []group
	for _, root := range roots {
		entries, err := os.ReadDir(filepath.Join(root, ".wt"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !validKey(strings.ToUpper(e.Name())) {
				continue
			}
			if g, ok := readGroup(root, e.Name()); ok {
				out = append(out, g)
			}
		}
	}
	return out
}

// findGroup gives the group of key in the first root that has it.
func findGroup(roots []string, key string) (group, bool) {
	for _, root := range roots {
		if g, ok := readGroup(root, strings.ToLower(key)); ok {
			return g, true
		}
	}
	return group{}, false
}

// repo gives the state of the worktree name of g, and the time of its newest commit.
func (gt Git) repo(ctx context.Context, g group, name string) (wire.ReviewRepo, time.Time, error) {
	var at time.Time
	wt, primary := g.worktree(name), g.primary(name)
	r := wire.ReviewRepo{Name: name}
	var err error
	if r.Branch, err = gt.Run(ctx, wt, "branch", "--show-current"); err != nil {
		return r, at, err
	}
	if r.Branch == "" {
		return r, at, fmt.Errorf("the worktree %s has no branch", wt)
	}
	if r.Base, err = gt.Run(ctx, primary, "branch", "--show-current"); err != nil {
		return r, at, err
	}
	if r.Base == "" {
		r.Base = "HEAD" // a detached primary checkout; the merge refuses it
	}
	counts, err := gt.Run(ctx, primary, "rev-list", "--left-right", "--count", r.Base+"..."+r.Branch)
	if err != nil {
		return r, at, err
	}
	if f := strings.Fields(counts); len(f) == 2 {
		r.Behind, r.Ahead = atoi(f[0]), atoi(f[1])
	}
	stat, err := gt.Run(ctx, primary, "diff", "--numstat", r.Base+"..."+r.Branch)
	if err != nil {
		return r, at, err
	}
	for _, l := range lines(stat) {
		f := strings.SplitN(l, "\t", 3)
		if len(f) == 3 {
			r.Added += atoi(f[0])
			r.Removed += atoi(f[1])
			r.Files++
		}
	}
	status, err := gt.Run(ctx, wt, "status", "--porcelain")
	if err != nil {
		return r, at, err
	}
	r.Dirty = status != ""
	r.Merge = wire.MergeFastForward
	if r.Behind > 0 {
		out, code, err := gt.Exit(ctx, primary, "merge-tree", "--write-tree", "--name-only", "--no-messages", r.Base, r.Branch)
		switch {
		case code == 1:
			r.Merge = wire.MergeConflict
			if l := lines(out); len(l) > 1 {
				r.ConflictFiles = l[1:]
			}
		case err != nil:
			return r, at, err
		default:
			r.Merge = wire.MergeRebase
		}
	}
	if r.Ahead > 0 {
		log, err := gt.Run(ctx, primary, "log", "-n", strconv.Itoa(MaxCommits), "--format=%h%x1f%cI%x1f%s%x1f%b%x1e", r.Base+".."+r.Branch)
		if err != nil {
			return r, at, err
		}
		r.Commits, at = parseLog(log)
	}
	return r, at, nil
}

// parseLog reads the log records of repo, newest first. It gives the commits and the time of the newest.
func parseLog(log string) ([]wire.ReviewCommit, time.Time) {
	var out []wire.ReviewCommit
	var newest time.Time
	for _, rec := range strings.Split(log, "\x1e") {
		f := strings.SplitN(strings.TrimLeft(rec, "\n"), "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		if t, err := time.Parse(time.RFC3339, f[1]); err == nil && t.After(newest) {
			newest = t
		}
		out = append(out, wire.ReviewCommit{SHA: f[0], Subject: f[2], Items: itemKeys(f[3])})
	}
	return out, newest.UTC()
}

// itemKeys gives the keys of the Item: lines of a commit message. A line can name more keys, with commas.
func itemKeys(body string) []string {
	var out []string
	for _, m := range itemLine.FindAllStringSubmatch(body, -1) {
		for _, k := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' }) {
			if k = strings.ToUpper(strings.TrimSpace(k)); validKey(k) && !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// mergeBase is the commit where branch left base.
func (gt Git) mergeBase(ctx context.Context, g group, r wire.ReviewRepo) (string, error) {
	return gt.Run(ctx, g.primary(r.Name), "merge-base", r.Base, r.Branch)
}

// files gives the changed files of the repo r: the diff from the merge base to the work tree of the worktree,
// so the uncommitted changes of tracked files are in it, plus the untracked files.
func (gt Git) files(ctx context.Context, g group, r wire.ReviewRepo) ([]wire.ReviewFile, error) {
	wt := g.worktree(r.Name)
	mb, err := gt.mergeBase(ctx, g, r)
	if err != nil {
		return nil, err
	}
	status, err := gt.Run(ctx, wt, "diff", "-z", "--name-status", "-M", mb)
	if err != nil {
		return nil, err
	}
	stat, err := gt.Run(ctx, wt, "diff", "-z", "--numstat", "-M", mb)
	if err != nil {
		return nil, err
	}
	out := parseNameStatus(r.Name, status)
	type counts struct {
		add, del int
		binary   bool
	}
	byPath := map[string]counts{}
	tok := strings.Split(stat, "\x00")
	for i := 0; i < len(tok); i++ {
		f := strings.SplitN(tok[i], "\t", 3)
		if len(f) != 3 {
			continue
		}
		path := f[2]
		if path == "" && i+2 < len(tok) {
			path = tok[i+2] // a rename: "add\tdel\t", old path, new path
			i += 2
		}
		byPath[path] = counts{atoi(f[0]), atoi(f[1]), f[0] == "-"}
	}
	for i := range out {
		c := byPath[out[i].Path]
		out[i].Added, out[i].Removed, out[i].Binary = c.add, c.del, c.binary
	}
	untracked, err := gt.Run(ctx, wt, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" {
			continue
		}
		f := wire.ReviewFile{Repo: r.Name, Path: p, Status: wire.FileAdded, Untracked: true}
		if data, err := os.ReadFile(filepath.Join(wt, p)); err == nil {
			f.Binary = isBinary(data)
			if !f.Binary {
				f.Added = lineCount(data)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// parseNameStatus reads git diff -z --name-status. A copy counts as an added file, a type change as a change.
func parseNameStatus(repo, out string) []wire.ReviewFile {
	var files []wire.ReviewFile
	tok := strings.Split(out, "\x00")
	for i := 0; i+1 < len(tok); i++ {
		code := tok[i]
		if code == "" {
			continue
		}
		f := wire.ReviewFile{Repo: repo, Path: tok[i+1]}
		i++
		switch code[0] {
		case 'R', 'C':
			if i+1 >= len(tok) {
				continue
			}
			f.Path = tok[i+1]
			i++
			f.Status = wire.FileAdded
			if code[0] == 'R' {
				f.Status, f.OldPath = wire.FileRenamed, tok[i-1]
			}
		case 'A':
			f.Status = wire.FileAdded
		case 'D':
			f.Status = wire.FileDeleted
		default:
			f.Status = wire.FileModified
		}
		files = append(files, f)
	}
	return files
}

// diff gives the diff of the file f of the repo r. With full, it gives the whole file as one hunk and sets Full.
// A file over MaxFullDiffLines falls back to the -U3 diff.
func (gt Git) diff(ctx context.Context, g group, r wire.ReviewRepo, f wire.ReviewFile, full bool) (wire.FileDiff, error) {
	d := wire.FileDiff{Repo: r.Name, Path: f.Path, OldPath: f.OldPath, Status: f.Status, Binary: f.Binary, Hunks: []wire.DiffHunk{}}
	wt := g.worktree(r.Name)
	if f.Untracked {
		data, err := os.ReadFile(filepath.Join(wt, f.Path))
		if err != nil {
			return d, err
		}
		if full {
			fileDiff(data, &d, MaxFullDiffLines)
			if !d.Truncated {
				d.Full = true
				return d, nil
			}
			d.Truncated = false
		}
		fileDiff(data, &d, MaxDiffLines)
		return d, nil
	}
	mb, err := gt.mergeBase(ctx, g, r)
	if err != nil {
		return d, err
	}
	run := func(unified, limit int) error {
		args := []string{"diff", "--no-color", "--unified=" + strconv.Itoa(unified), "-M", mb, "--"}
		if f.OldPath != "" {
			args = append(args, f.OldPath)
		}
		out, err := gt.Run(ctx, wt, append(args, f.Path)...)
		if err != nil {
			return err
		}
		status, old := d.Status, d.OldPath
		d.Truncated = false
		parseDiff(out, &d, limit)
		d.Status, d.OldPath = status, old // the file list is the source of the status
		return nil
	}
	if full {
		if err := run(MaxFullDiffLines, MaxFullDiffLines); err != nil {
			return d, err
		}
		if !d.Truncated {
			d.Full = true
			return d, nil
		}
	}
	return d, run(3, MaxDiffLines)
}

// baseState gives the current branch of the primary checkout root/name and, when it has commits that its upstream
// does not have, the NotPushed row.
func (gt Git) baseState(ctx context.Context, root, name string) baseState {
	dir := filepath.Join(root, name)
	var st baseState
	base, err := gt.Run(ctx, dir, "branch", "--show-current")
	if err != nil || base == "" {
		return st
	}
	st.Base = base
	if st.Upstream, err = gt.Run(ctx, dir, "rev-parse", "--symbolic-full-name", base+"@{upstream}"); err != nil {
		st.Upstream = ""
		return st
	}
	n, err := gt.Run(ctx, dir, "rev-list", "--count", base+"@{upstream}.."+base)
	if err != nil || atoi(n) < 1 {
		return st
	}
	st.Ahead = true
	st.NotPushed = wire.NotPushed{Root: root, Repo: name, Base: base, Ahead: atoi(n), Items: gt.items(ctx, dir, base)}
	return st
}

// groupState gives the repos of g in merge order, computed at the same time, and the time of the newest commit.
func (gt Git) groupState(ctx context.Context, g group, order Order) groupState {
	names := order.Sort(g.Repos)
	repos := make([]wire.ReviewRepo, len(names))
	ats := make([]time.Time, len(names))
	errs := make([]error, len(names))
	parallel(len(names), func(i int) { repos[i], ats[i], errs[i] = gt.repo(ctx, g, names[i]) })
	st := groupState{Repos: repos}
	for i := range names {
		if errs[i] != nil {
			return groupState{Err: errs[i]}
		}
		if ats[i].After(st.Newest) {
			st.Newest = ats[i]
		}
	}
	return st
}

// items gives the item keys that the commits of base after its upstream name: in a message or in a merged branch.
func (gt Git) items(ctx context.Context, dir, base string) []string {
	rng := base + "@{upstream}.." + base
	var keys []string
	add := func(k string) {
		if k = strings.ToUpper(k); validKey(k) && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if body, err := gt.Run(ctx, dir, "log", "-n", "500", "--format=%B", rng); err == nil {
		for _, k := range textKey.FindAllString(body, -1) {
			add(k)
		}
	}
	// A fast-forward merge leaves no merge commit. The reflog of base names the branch.
	shas, _ := gt.Run(ctx, dir, "rev-list", "-n", "500", rng)
	ahead := strings.Fields(shas)
	reflog, _ := gt.Run(ctx, dir, "log", "-g", "-n", "500", "--format=%H %gs", base)
	for _, line := range strings.Split(reflog, "\n") {
		sha, msg, _ := strings.Cut(line, " ")
		branch, ok := strings.CutPrefix(msg, "merge ")
		if !ok || !slices.Contains(ahead, sha) {
			continue
		}
		branch, _, _ = strings.Cut(branch, ":")
		for _, k := range branchKey.FindAllString(branch, -1) {
			add(k)
		}
	}
	slices.Sort(keys)
	return keys
}

// primaries gives the folders of root whose .git is a folder: the main checkouts.
func primaries(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if st, err := os.Stat(filepath.Join(root, e.Name(), ".git")); err == nil && st.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
