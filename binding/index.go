// Package binding keeps the worktree bindings of the home store in memory (OVERSIRE-587).
// The app writes them. The review engine and the runbooks read them to find the group of an item.
package binding

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Records reads all records of one collection (store.OpLog).
type Records interface {
	Records(collection string) (map[string]json.RawMessage, error)
}

// Index holds the bindings by key. machine is the machine of this daemon.
type Index struct {
	log     *slog.Logger
	machine string

	mu    sync.RWMutex
	byKey map[string]wire.WorktreeBinding
}

func NewIndex(machine string, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	return &Index{log: log, machine: machine, byKey: map[string]wire.WorktreeBinding{}}
}

// Load replaces the index with the records of r.
func (x *Index) Load(r Records) error {
	recs, err := r.Records(wire.CollectionWorktreeBindings)
	if err != nil {
		return err
	}
	next := map[string]wire.WorktreeBinding{}
	for id, raw := range recs {
		if b, ok := x.decode(id, raw); ok {
			next[b.Key] = b
		}
	}
	x.mu.Lock()
	x.byKey = next
	x.mu.Unlock()
	return nil
}

// Apply takes the accepted ops of the store. It skips the other collections.
func (x *Index) Apply(accepted []wire.StoreOp) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, op := range accepted {
		if op.Collection != wire.CollectionWorktreeBindings {
			continue
		}
		key := strings.ToUpper(op.ID)
		if op.Op == wire.StoreDelete {
			delete(x.byKey, key)
			continue
		}
		if b, ok := x.decode(op.ID, op.Record); ok {
			x.byKey[b.Key] = b
		} else {
			delete(x.byKey, key)
		}
	}
}

func (x *Index) decode(id string, raw json.RawMessage) (wire.WorktreeBinding, bool) {
	var b wire.WorktreeBinding
	if err := json.Unmarshal(raw, &b); err != nil || !b.Valid() {
		x.log.Warn("worktree binding skipped", "id", id, "err", err)
		return b, false
	}
	b.Key, b.Parent = strings.ToUpper(b.Key), strings.ToUpper(b.Parent)
	return b, true
}

// Get gives the binding of key, on any machine.
func (x *Index) Get(key string) (wire.WorktreeBinding, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	b, ok := x.byKey[strings.ToUpper(key)]
	return b, ok
}

// Children gives the bindings whose parent is parent, on all machines, sorted by key.
func (x *Index) Children(parent string) []wire.WorktreeBinding {
	return x.filter(func(b wire.WorktreeBinding) bool { return b.Parent != "" && b.Parent == strings.ToUpper(parent) })
}

// Mine gives the bindings of this machine, sorted by key.
func (x *Index) Mine() []wire.WorktreeBinding { return x.filter(x.mine) }

// All gives all bindings, sorted by key.
func (x *Index) All() []wire.WorktreeBinding {
	return x.filter(func(wire.WorktreeBinding) bool { return true })
}

// At gives the own-mode binding of this machine whose folder is dir or holds it. The longest folder wins.
func (x *Index) At(dir string) (wire.WorktreeBinding, bool) {
	dir = filepath.Clean(dir)
	var best wire.WorktreeBinding
	found := false
	for _, b := range x.Mine() {
		d := b.Dir()
		if b.Mode == wire.WorktreeParent || (dir != d && !strings.HasPrefix(dir, d+string(filepath.Separator))) {
			continue
		}
		if !found || len(d) > len(best.Dir()) {
			best, found = b, true
		}
	}
	return best, found
}

// GroupPath gives the group of the binding of key on this machine, or "" when there is none.
func (x *Index) GroupPath(key string) string {
	if b, ok := x.Get(key); ok && x.mine(b) {
		return b.Group
	}
	return ""
}

func (x *Index) mine(b wire.WorktreeBinding) bool { return strings.EqualFold(b.Machine, x.machine) }

func (x *Index) filter(keep func(wire.WorktreeBinding) bool) []wire.WorktreeBinding {
	x.mu.RLock()
	out := []wire.WorktreeBinding{}
	for _, b := range x.byKey {
		if keep(b) {
			out = append(out, b)
		}
	}
	x.mu.RUnlock()
	slices.SortFunc(out, func(a, b wire.WorktreeBinding) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Routes serves GET /v1/worktree-bindings. The query key=, parent= or dir= narrows the page.
func (x *Index) Routes() []server.Route {
	return []server.Route{{Pattern: "GET /v1/worktree-bindings", Access: server.Member, Handle: x.list}}
}

func (x *Index) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := wire.WorktreeBindingsPage{Bindings: []wire.WorktreeBinding{}}
	switch {
	case q.Get("key") != "":
		if b, ok := x.Get(q.Get("key")); ok {
			page.Bindings = append(page.Bindings, b)
		}
	case q.Get("parent") != "":
		page.Bindings = x.Children(q.Get("parent"))
	case q.Has("dir"):
		dir := q.Get("dir")
		if !filepath.IsAbs(dir) {
			server.WriteError(w, http.StatusBadRequest, "The dir must be an absolute path.")
			return
		}
		if b, ok := x.At(dir); ok {
			page.Bindings = append(page.Bindings, b)
		}
	default:
		page.Bindings = x.All()
	}
	server.WriteJSON(w, http.StatusOK, page)
}
