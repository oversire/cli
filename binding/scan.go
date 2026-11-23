package binding

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/gitwatch"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Lister gives the linked git worktrees of the machine. *gitwatch.Watcher is the real one.
type Lister interface{ Worktrees() []gitwatch.Worktree }

// Git runs git in a folder. review.Git is the real one.
type Git interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// Sessions gives the live sessions. *sessions.Book is the real one.
type Sessions interface{ All() []wire.DaemonSession }

const scanTTL = 30 * time.Second

var (
	textKey = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*-\d+\b`)
	nameKey = regexp.MustCompile(`(?i)\b[a-z][a-z0-9_]*-\d+\b`)
)

// Scanner reads the state of each linked worktree for GET /v1/worktrees. It keeps the last read for 30 s.
type Scanner struct {
	list     Lister
	git      Git
	sessions Sessions
	clock    func() time.Time

	mu   sync.Mutex
	page wire.WorktreesPage
}

func NewScanner(list Lister, git Git, sessions Sessions, clock func() time.Time) *Scanner {
	return &Scanner{list: list, git: git, sessions: sessions, clock: clock}
}

// Worktrees gives the worktrees. changed drops the clean ones. dir keeps only the worktrees in that folder.
func (s *Scanner) Worktrees(ctx context.Context, changed bool, dir string) wire.WorktreesPage {
	page := s.read(ctx)
	out := wire.WorktreesPage{Worktrees: []wire.WorktreeInfo{}, At: page.At}
	dir = filepath.Clean(dir)
	for _, w := range page.Worktrees {
		if changed && w.Uncommitted == 0 && w.Ahead == 0 {
			continue
		}
		if dir != "." && w.Path != dir && !strings.HasPrefix(w.Path, dir+string(filepath.Separator)) {
			continue
		}
		out.Worktrees = append(out.Worktrees, w)
	}
	return out
}

func (s *Scanner) read(ctx context.Context) wire.WorktreesPage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if !s.page.At.IsZero() && now.Sub(s.page.At) < scanTTL {
		return s.page
	}
	live := s.sessions.All()
	page := wire.WorktreesPage{Worktrees: []wire.WorktreeInfo{}, At: now}
	for _, wt := range s.list.Worktrees() {
		info, err := s.info(ctx, wt)
		if err != nil {
			continue
		}
		info.Session, info.Guess = guess(wt, live)
		page.Worktrees = append(page.Worktrees, info)
	}
	s.page = page
	return page
}

func (s *Scanner) info(ctx context.Context, wt gitwatch.Worktree) (wire.WorktreeInfo, error) {
	info := wire.WorktreeInfo{Path: wt.Path, Repo: wt.Repo, Branch: wt.Branch, Base: wt.Base}
	status, err := s.git.Run(ctx, wt.Path, "status", "--porcelain")
	if err != nil {
		return info, err
	}
	if status != "" {
		info.Uncommitted = strings.Count(status, "\n") + 1
	}
	if wt.Base != "" {
		n, err := s.git.Run(ctx, wt.Path, "rev-list", "--count", wt.Base+"..HEAD")
		if err != nil {
			return info, err
		}
		info.Ahead, _ = strconv.Atoi(strings.TrimSpace(n))
	}
	return info, nil
}

// guess gives the session in the worktree and the likely item: the session key or prompt, then the path, then the branch.
func guess(wt gitwatch.Worktree, live []wire.DaemonSession) (string, *wire.WorktreeGuess) {
	session := ""
	for _, ss := range live {
		if ss.Cwd != wt.Path && !strings.HasPrefix(ss.Cwd, wt.Path+string(filepath.Separator)) {
			continue
		}
		if session == "" {
			session = ss.ID
		}
		if k := textKey.FindString(ss.Key); k != "" {
			return ss.ID, &wire.WorktreeGuess{Key: k, From: "prompt"}
		}
		if keys := slices.Compact(slices.Sorted(slices.Values(textKey.FindAllString(ss.Prompt, -1)))); len(keys) == 1 {
			return ss.ID, &wire.WorktreeGuess{Key: keys[0], From: "prompt"}
		}
	}
	// Only the last two parts of the path: a parent folder such as /tmp/claude-501 names no item.
	if k := nameKey.FindString(filepath.Base(filepath.Dir(wt.Path)) + "/" + filepath.Base(wt.Path)); k != "" {
		return session, &wire.WorktreeGuess{Key: strings.ToUpper(k), From: "cwd"}
	}
	if k := nameKey.FindString(wt.Branch); k != "" {
		return session, &wire.WorktreeGuess{Key: strings.ToUpper(k), From: "branch"}
	}
	return session, nil
}

// Routes gives GET /v1/worktrees.
func (s *Scanner) Routes() []server.Route {
	return []server.Route{{Pattern: "GET /v1/worktrees", Access: server.Member, Handle: s.serve}}
}

func (s *Scanner) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir := q.Get("dir")
	if dir != "" && !filepath.IsAbs(dir) {
		server.WriteError(w, http.StatusBadRequest, "The dir must be an absolute path.")
		return
	}
	server.WriteJSON(w, http.StatusOK, s.Worktrees(r.Context(), q.Get("changed") == "1", dir))
}
