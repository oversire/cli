package gitwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// repo is the common git folder of one repository and its worktrees.
type repo struct {
	gitdir    string
	worktrees []worktree
}

type worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	at     time.Time
}

// openRepo finds the common git folder of a checkout or a worktree. ok is false when path is not a git checkout.
func openRepo(path string) (repo, bool) {
	dot := filepath.Join(path, ".git")
	fi, err := os.Stat(dot)
	if err != nil {
		return repo{}, false
	}
	gitdir := dot
	if !fi.IsDir() {
		b, err := os.ReadFile(dot)
		if err != nil {
			return repo{}, false
		}
		dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if !ok {
			return repo{}, false
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(path, dir)
		}
		gitdir = dir
		if c, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
			common := strings.TrimSpace(string(c))
			if !filepath.IsAbs(common) {
				common = filepath.Join(dir, common)
			}
			gitdir = filepath.Clean(common)
		}
	}
	r := repo{gitdir: gitdir}
	r.read()
	return r, true
}

// read lists the main worktree and the linked worktrees with the branch of each.
func (r *repo) read() {
	r.worktrees = r.worktrees[:0]
	if b := headBranch(filepath.Join(r.gitdir, "HEAD")); filepath.Base(r.gitdir) == ".git" {
		r.worktrees = append(r.worktrees, worktree{Path: filepath.Dir(r.gitdir), Branch: b})
	}
	entries, _ := os.ReadDir(filepath.Join(r.gitdir, "worktrees"))
	for _, e := range entries {
		dir := filepath.Join(r.gitdir, "worktrees", e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "gitdir"))
		if err != nil {
			continue
		}
		w := worktree{Path: filepath.Dir(strings.TrimSpace(string(b))), Branch: headBranch(filepath.Join(dir, "HEAD"))}
		if fi, err := os.Stat(filepath.Join(dir, "gitdir")); err == nil {
			w.at = fi.ModTime()
		}
		r.worktrees = append(r.worktrees, w)
	}
}

// worktreeOf gives the worktree that has the branch, else "".
func (r repo) worktreeOf(branch string) string {
	for _, w := range r.worktrees {
		if w.Branch == branch {
			return w.Path
		}
	}
	return ""
}

func headBranch(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return ref
}

// logs gives the reflog files of the local and the remote branches: ref name → file.
func (r repo) logs() map[string]string {
	out := map[string]string{}
	for _, top := range []string{"refs/heads", "refs/remotes"} {
		base := filepath.Join(r.gitdir, "logs")
		filepath.WalkDir(filepath.Join(base, top), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if rel, err := filepath.Rel(base, p); err == nil {
					out[filepath.ToSlash(rel)] = p
				}
			}
			return nil
		})
	}
	return out
}

// dirs gives the folders whose entries change when a ref or a worktree changes. Git writes a ref through a lock file
// and a rename in the folder of the ref.
func (r repo) dirs() []string {
	out := []string{r.gitdir, filepath.Join(r.gitdir, "worktrees")}
	for _, top := range []string{"refs/heads", "refs/remotes"} {
		filepath.WalkDir(filepath.Join(r.gitdir, top), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

// entry is one line of a reflog: "<old> <new> <name> <<email>> <unix> <tz>\t<message>".
type entry struct {
	old, new string
	email    string
	at       time.Time
	message  string
}

func parseEntry(line []byte) (entry, bool) {
	head, msg, ok := bytes.Cut(line, []byte("\t"))
	if !ok {
		return entry{}, false
	}
	f := strings.Fields(string(head))
	if len(f) < 4 {
		return entry{}, false
	}
	e := entry{old: f[0], new: f[1], message: string(msg)}
	if i, j := bytes.IndexByte(head, '<'), bytes.IndexByte(head, '>'); i >= 0 && j > i {
		e.email = string(head[i+1 : j])
	}
	if sec, err := strconv.ParseInt(f[len(f)-2], 10, 64); err == nil {
		e.at = time.Unix(sec, 0).UTC()
	}
	return e, true
}

// zero tells if a sha is the null sha of a new ref.
func zero(sha string) bool { return strings.Trim(sha, "0") == "" }
