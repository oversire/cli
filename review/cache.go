package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

const (
	pollEvery  = 5 * time.Second
	settle     = time.Minute     // the wait after the last change of the git files of an entry
	settleMax  = 3 * time.Minute // the longest wait after the first change
	sweepEvery = 10 * time.Minute
	liveLimit  = time.Minute
	workers    = 8
)

// Cache keeps the git state of the groups and of the primary checkouts (OVERSIRE-422).
// Each entry keeps a print of the git files that its state comes from: HEAD, index, refs, logs.
// A read computes an entry live when its print changed, else it gives the kept state.
// Run computes a changed entry again after the settle time, and each entry after sweepEvery.
// The sweep finds edits of the work tree, which change no git file.
type Cache struct {
	git   Git
	order Order
	clock func() time.Time

	mu     sync.Mutex
	groups map[string]*groupSlot // by group folder
	bases  map[string]*baseSlot  // by primary checkout folder
}

// groupState is the git part of a code review summary.
type groupState struct {
	Repos  []wire.ReviewRepo
	Newest time.Time
	Err    error
}

// baseState is the state of the current branch of a primary checkout against its upstream.
type baseState struct {
	NotPushed wire.NotPushed
	Ahead     bool
	Base      string
	Upstream  string // full ref name, for example refs/remotes/origin/main
}

// mark is the change state of an entry. print is the print at the compute, seen the last print that a check saw.
type mark struct {
	print, seen string
	stale, last time.Time // first and last change after the compute; stale is zero when the state is fresh
	at          time.Time // the start of the compute
}

type groupSlot struct {
	mark
	g     group
	state groupState
}

type baseSlot struct {
	mark
	root, name string
	state      baseState
}

func NewCache(git Git, order Order, clock func() time.Time) *Cache {
	return &Cache{git: git, order: order, clock: clock, groups: map[string]*groupSlot{}, bases: map[string]*baseSlot{}}
}

// Groups gives the state of each group. It computes the new and the changed groups live and forgets the gone groups.
func (c *Cache) Groups(ctx context.Context, gs []group) map[string]groupState {
	c.mu.Lock()
	keep := map[string]bool{}
	var todo []group
	for _, g := range gs {
		keep[g.Dir()] = true
		s := c.groups[g.Dir()]
		if s == nil || !slices.Equal(s.g.Repos, g.Repos) || s.observe(c.clock(), s.paths()) {
			todo = append(todo, g)
		}
	}
	for dir := range c.groups {
		if !keep[dir] {
			delete(c.groups, dir)
		}
	}
	c.mu.Unlock()
	c.computeGroups(ctx, todo)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]groupState{}
	for _, g := range gs {
		if s := c.groups[g.Dir()]; s != nil {
			out[g.Dir()] = s.state
		}
	}
	return out
}

// Group computes the state of g live and keeps it.
func (c *Cache) Group(ctx context.Context, g group) groupState {
	c.computeGroups(ctx, []group{g})
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.groups[g.Dir()]; s != nil {
		return s.state
	}
	return groupState{Err: fmt.Errorf("the group %s has no state", g.Rel())}
}

// NotPushed gives each base of the primary checkouts of the roots that is ahead of its upstream.
func (c *Cache) NotPushed(ctx context.Context, roots []string) []wire.NotPushed {
	type place struct{ root, name string }
	var all []place
	for _, root := range roots {
		for _, name := range primaries(root) {
			all = append(all, place{root, name})
		}
	}
	c.mu.Lock()
	keep := map[string]bool{}
	var todo []*baseSlot
	for _, p := range all {
		dir := filepath.Join(p.root, p.name)
		keep[dir] = true
		s := c.bases[dir]
		if s == nil {
			s = &baseSlot{root: p.root, name: p.name}
			c.bases[dir] = s
			todo = append(todo, s)
		} else if s.at.IsZero() || s.observe(c.clock(), s.paths()) {
			todo = append(todo, s)
		}
	}
	for dir := range c.bases {
		if !keep[dir] {
			delete(c.bases, dir)
		}
	}
	c.mu.Unlock()
	c.computeBases(ctx, todo)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []wire.NotPushed{}
	for _, p := range all {
		if s := c.bases[filepath.Join(p.root, p.name)]; s != nil && s.state.Ahead {
			out = append(out, s.state.NotPushed)
		}
	}
	return out
}

// Run checks the prints each pollEvery and computes the due entries again. changed gets the key of each group
// whose state changed.
func (c *Cache) Run(ctx context.Context, changed func(key string)) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.step(ctx, changed)
		}
	}
}

// step computes the due entries again and tells the groups whose state changed.
func (c *Cache) step(ctx context.Context, changed func(key string)) {
	gs, bs := c.due()
	old := map[string]groupState{}
	c.mu.Lock()
	for _, g := range gs {
		if s := c.groups[g.Dir()]; s != nil {
			old[g.Dir()] = s.state
		}
	}
	c.mu.Unlock()
	c.computeGroups(ctx, gs)
	c.computeBases(ctx, bs)
	c.mu.Lock()
	var keys []string
	for _, g := range gs {
		if s := c.groups[g.Dir()]; s != nil && !reflect.DeepEqual(old[g.Dir()], s.state) {
			keys = append(keys, g.Key())
		}
	}
	c.mu.Unlock()
	for _, k := range keys {
		changed(k)
	}
}

// due checks the prints and gives the entries to compute again: settled changes and old states.
func (c *Cache) due() ([]group, []*baseSlot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	var gs []group
	for _, s := range c.groups {
		s.observe(now, s.paths())
		if s.due(now) {
			gs = append(gs, s.g)
		}
	}
	var bs []*baseSlot
	for _, s := range c.bases {
		s.observe(now, s.paths())
		if s.due(now) {
			bs = append(bs, s)
		}
	}
	return gs, bs
}

// computeGroups computes the groups at the same time. A client that stops does not stop the compute.
func (c *Cache) computeGroups(ctx context.Context, gs []group) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), liveLimit)
	defer cancel()
	parallel(len(gs), func(i int) {
		g := gs[i]
		at := c.clock()
		st := c.git.groupState(ctx, g, c.order)
		s := &groupSlot{g: g, state: st}
		s.done(at, s.paths())
		c.mu.Lock()
		if old := c.groups[g.Dir()]; old == nil || !old.at.After(at) {
			c.groups[g.Dir()] = s
		}
		c.mu.Unlock()
	})
}

func (c *Cache) computeBases(ctx context.Context, bs []*baseSlot) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), liveLimit)
	defer cancel()
	parallel(len(bs), func(i int) {
		old := bs[i]
		at := c.clock()
		s := &baseSlot{root: old.root, name: old.name, state: c.git.baseState(ctx, old.root, old.name)}
		s.done(at, s.paths())
		c.mu.Lock()
		dir := filepath.Join(old.root, old.name)
		if cur := c.bases[dir]; cur != nil && !cur.at.After(at) {
			c.bases[dir] = s
		}
		c.mu.Unlock()
	})
}

// observe compares the print of paths with the last one. It tells if the state is stale.
func (m *mark) observe(now time.Time, paths []string) bool {
	p := fingerprint(paths)
	if p != m.seen {
		m.seen, m.last = p, now
		if p == m.print {
			m.stale = time.Time{}
		} else if m.stale.IsZero() {
			m.stale = now
		}
	}
	return !m.stale.IsZero()
}

// due tells if the background loop computes the entry: a change settled, or the state is old.
func (m *mark) due(now time.Time) bool {
	if !m.stale.IsZero() && (now.Sub(m.last) >= settle || now.Sub(m.stale) >= settleMax) {
		return true
	}
	return now.Sub(m.at) >= sweepEvery
}

// done marks a compute that started at at. The print is taken after the compute, so it has the writes of git itself.
func (m *mark) done(at time.Time, paths []string) {
	m.at = at
	m.print = fingerprint(paths)
	m.seen, m.stale, m.last = m.print, time.Time{}, time.Time{}
}

// paths are the git files of the repos of the group: the HEAD, index and log of each worktree, and the HEAD,
// log, packed refs and the refs of the branch and the base of each primary checkout.
func (s *groupSlot) paths() []string {
	var out []string
	for _, name := range s.g.Repos {
		if dir := gitdir(s.g.worktree(name)); dir != "" {
			out = append(out, filepath.Join(dir, "HEAD"), filepath.Join(dir, "index"), filepath.Join(dir, "logs", "HEAD"))
		}
		common := filepath.Join(s.g.primary(name), ".git")
		out = append(out, filepath.Join(common, "HEAD"), filepath.Join(common, "packed-refs"), filepath.Join(common, "logs", "HEAD"))
		for _, r := range s.state.Repos {
			if r.Name == name {
				out = append(out, filepath.Join(common, "refs", "heads", r.Branch), filepath.Join(common, "refs", "heads", r.Base))
			}
		}
	}
	return out
}

// paths are the git files of the primary checkout: HEAD, its log, packed refs, the base ref and the upstream ref.
func (s *baseSlot) paths() []string {
	dir := filepath.Join(s.root, s.name, ".git")
	out := []string{filepath.Join(dir, "HEAD"), filepath.Join(dir, "logs", "HEAD"), filepath.Join(dir, "packed-refs")}
	if s.state.Base != "" {
		out = append(out, filepath.Join(dir, "refs", "heads", s.state.Base))
	}
	if s.state.Upstream != "" {
		out = append(out, filepath.Join(dir, filepath.FromSlash(s.state.Upstream)))
	}
	return out
}

// gitdir reads the .git file of the worktree wt.
func gitdir(wt string) string {
	b, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wt, dir)
	}
	return filepath.Clean(dir)
}

// fingerprint is the size and the change time of each path. A missing path counts too.
func fingerprint(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%d.%d;", st.Size(), st.ModTime().UnixNano())
		} else {
			b.WriteString("-;")
		}
	}
	return b.String()
}

// parallel runs f for 0..n-1, at most workers at the same time.
func parallel(n int, f func(i int)) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f(i)
		}()
	}
	wg.Wait()
}
