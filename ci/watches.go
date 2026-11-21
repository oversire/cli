package ci

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/mirror"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	defaultWatchTTL = 1800
	minWatchTTL     = 60
	maxWatchTTL     = 7200
	maxWatches      = 50
	watchSweep      = time.Minute
	watchBodyLimit  = 8 << 10
)

// ErrBadTTL is the refusal for a ttl outside the range.
var ErrBadTTL = server.Refuse(server.Invalid, "Give ttl as a number from %d to %d seconds.", minWatchTTL, maxWatchTTL)

// RollupIndex is the part of Index that the watches use.
type RollupIndex interface {
	Repo(repo string) (wire.CIRepoState, error)
	Resolve(repo string, r Ref) (Target, error)
	Rollup(repo string, t Target, workflows []string) (wire.CIRollup, error)
	Changed() <-chan struct{}
}

// SourceFinder gives the source app of the mirror (mirror.Store).
type SourceFinder interface {
	Source() (mirror.Source, bool)
}

// Watches is the watch book of the home daemon. The source app polls the commits in it. The book is in memory only.
type Watches struct {
	// Self names this machine for a request from loopback.
	Self func() string

	index   RollupIndex
	deliver func(match func(server.Caller) bool, msg any) int
	source  SourceFinder
	now     func() time.Time
	sweepAt time.Duration

	mu   sync.Mutex
	list map[string]wire.CIWatch
}

func NewWatches(index RollupIndex, deliver func(match func(server.Caller) bool, msg any) int, source SourceFinder, clock func() time.Time) *Watches {
	if clock == nil {
		clock = time.Now
	}
	return &Watches{index: index, deliver: deliver, source: source, now: clock, sweepAt: watchSweep, list: map[string]wire.CIWatch{}}
}

// Add adds or extends the watch of a ref. A rollup that has ended gives done and no watch.
func (w *Watches) Add(req wire.CIWatchRequest, machine string) (wire.CIWatchAnswer, error) {
	ref, err := ParseRef(req.Ref)
	if err != nil {
		return wire.CIWatchAnswer{}, invalid("Give the ref.")
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = defaultWatchTTL
	}
	if ttl < minWatchTTL || ttl > maxWatchTTL {
		return wire.CIWatchAnswer{}, ErrBadTTL
	}
	repo := strings.TrimSpace(req.Repo)
	if parts := strings.Split(repo, "/"); len(parts) < 2 || slicesHasEmpty(parts) {
		return wire.CIWatchAnswer{}, invalid("Give the repo as owner/name.")
	}
	st, err := w.index.Repo(repo)
	if err != nil {
		return wire.CIWatchAnswer{}, err
	}
	t, err := w.index.Resolve(st.Repo, ref)
	if err != nil {
		return wire.CIWatchAnswer{}, err
	}
	roll, err := w.index.Rollup(st.Repo, t, nil)
	if err != nil {
		return wire.CIWatchAnswer{}, err
	}
	roll.Ref = req.Ref
	if roll.Done {
		return wire.CIWatchAnswer{Rollup: &roll, Done: true}, nil
	}
	now := w.now().UTC()
	exp := now.Add(time.Duration(ttl) * time.Second)
	nw := wire.CIWatch{Repo: st.Repo, Ref: req.Ref, TTL: ttl, ExpiresAt: &exp, SHA: t.SHA, Branch: t.Branch, PR: t.PR, CreatedAt: &now, Machine: machine}
	if nw.SHA == "" && nw.Branch == "" {
		nw.Branch = req.Ref
	}
	nw.ID = watchID(nw)

	w.mu.Lock()
	if old, ok := w.list[nw.ID]; ok {
		nw = old
		if old.ExpiresAt == nil || exp.After(*old.ExpiresAt) {
			nw.ExpiresAt, nw.TTL = &exp, ttl
		}
	} else if len(w.list) >= maxWatches {
		w.dropFirstToExpire()
	}
	w.list[nw.ID] = nw
	frame := w.frame()
	w.mu.Unlock()
	w.send(frame)
	return wire.CIWatchAnswer{Watch: &nw, Rollup: &roll}, nil
}

func watchID(x wire.CIWatch) string {
	if x.SHA != "" {
		return x.Repo + "@" + x.SHA
	}
	return x.Repo + "@" + x.Branch
}

// dropFirstToExpire removes the watch with the earliest end. The caller holds mu.
func (w *Watches) dropFirstToExpire() {
	var first string
	for id, x := range w.list {
		if first == "" || x.ExpiresAt.Before(*w.list[first].ExpiresAt) {
			first = id
		}
	}
	delete(w.list, first)
}

// List gives the live watches, sorted by id.
func (w *Watches) List() []wire.CIWatch {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live()
}

// live gives the watches that did not expire. The caller holds mu.
func (w *Watches) live() []wire.CIWatch {
	now := w.now()
	out := make([]wire.CIWatch, 0, len(w.list))
	for _, x := range w.list {
		if x.ExpiresAt.After(now) {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (w *Watches) frame() wire.CIFrame {
	return wire.CIFrame{Type: "ci", Event: "watches", Watches: w.live()}
}

// send gives the full list to the source app. Without an app it does nothing: the app reads the list when it connects.
func (w *Watches) send(f wire.CIFrame) {
	if src, ok := w.source.Source(); ok {
		w.deliver(src.Match, f)
	}
}

// sweep drops the expired watches. It sends the list when the list changed.
func (w *Watches) sweep() {
	w.mu.Lock()
	changed := false
	now := w.now()
	for id, x := range w.list {
		if !x.ExpiresAt.After(now) {
			delete(w.list, id)
			changed = true
		}
	}
	frame := w.frame()
	w.mu.Unlock()
	if changed {
		w.send(frame)
	}
}

// dropEnded drops the watches whose rollup has ended. It sends the list when the list changed.
func (w *Watches) dropEnded() {
	w.mu.Lock()
	snapshot := make([]wire.CIWatch, 0, len(w.list))
	for _, x := range w.list {
		snapshot = append(snapshot, x)
	}
	w.mu.Unlock()
	var ended []string
	for _, x := range snapshot {
		if w.ended(x) {
			ended = append(ended, x.ID)
		}
	}
	if len(ended) == 0 {
		return
	}
	w.mu.Lock()
	for _, id := range ended {
		delete(w.list, id)
	}
	frame := w.frame()
	w.mu.Unlock()
	w.send(frame)
}

// ended tells if the rollup of a watch has ended. An error keeps the watch.
func (w *Watches) ended(x wire.CIWatch) bool {
	name := x.SHA
	if name == "" {
		name = x.Branch
	}
	ref, err := ParseRef(name)
	if err != nil {
		return false
	}
	t, err := w.index.Resolve(x.Repo, ref)
	if err != nil {
		return false
	}
	roll, err := w.index.Rollup(x.Repo, t, nil)
	return err == nil && roll.Done
}

// Run drops ended and expired watches until ctx ends.
func (w *Watches) Run(ctx context.Context) error {
	tick := time.NewTicker(w.sweepAt)
	defer tick.Stop()
	for {
		// Take the channel before the check, so a change during the check wakes the next round.
		changed := w.index.Changed()
		w.dropEnded()
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-tick.C:
			w.sweep()
		}
	}
}

// Routes gives POST and GET /v1/ci/watches. A member daemon sends each request to home.
func (w *Watches) Routes(fwd *Forward) []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/ci/watches", Access: server.Member, Handle: fwd.Wrap(w.post)},
		{Pattern: "GET /v1/ci/watches", Access: server.Member, Handle: fwd.Wrap(w.get)},
	}
}

func (w *Watches) post(rw http.ResponseWriter, req *http.Request) {
	var body wire.CIWatchRequest
	if !server.Decode(rw, req, watchBodyLimit, "Give a JSON body: ", &body) {
		return
	}
	ans, err := w.Add(body, w.machineOf(req))
	if err != nil {
		failCI(rw, err)
		return
	}
	server.WriteJSON(rw, http.StatusOK, ans)
}

func (w *Watches) get(rw http.ResponseWriter, _ *http.Request) {
	server.WriteJSON(rw, http.StatusOK, w.List())
}

// machineOf names the machine of the caller. A caller on loopback is this machine.
func (w *Watches) machineOf(req *http.Request) string {
	if c, ok := server.CallerFrom(req.Context()); ok {
		if c.Machine != "" {
			return c.Machine
		}
		if !c.Loopback && c.Name != "" {
			return c.Name
		}
	}
	if w.Self != nil {
		return w.Self()
	}
	return ""
}

// failCI writes the reply for an error of a watch call.
func failCI(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUntracked):
		server.WriteError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNoCIData):
		server.WriteError(w, http.StatusServiceUnavailable, err.Error())
	default:
		server.Fail(w, err)
	}
}
