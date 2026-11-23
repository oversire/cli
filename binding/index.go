// Package binding keeps the worktree bindings of the home store in memory (OVERSIRE-587).
// The app writes them. The review engine and the runbooks read them to find the group of an item.
package binding

import (
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"os"
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
// byKey comes from the home store. pushed comes from PUT /v1/worktree-bindings: the app sends it when the
// home store is not on this daemon (a cloud home). A binding of the home store wins.
type Index struct {
	log     *slog.Logger
	machine string
	path    string // the file of pushed; "" keeps it in memory

	mu     sync.RWMutex
	byKey  map[string]wire.WorktreeBinding
	pushed map[string]wire.WorktreeBinding
}

func NewIndex(machine string, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	return &Index{log: log, machine: machine, byKey: map[string]wire.WorktreeBinding{}, pushed: map[string]wire.WorktreeBinding{}}
}

// Keep loads the pushed bindings from path, and writes them there on each PUT.
func (x *Index) Keep(path string) {
	x.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var page wire.WorktreeBindingsPage
	if err := json.Unmarshal(data, &page); err != nil {
		x.log.Warn("worktree bindings file", "path", path, "err", err)
		return
	}
	x.mu.Lock()
	x.pushed = x.valid(page.Bindings)
	x.mu.Unlock()
}

// Put replaces the pushed bindings.
func (x *Index) Put(bs []wire.WorktreeBinding) error {
	next := x.valid(bs)
	x.mu.Lock()
	x.pushed = next
	x.mu.Unlock()
	if x.path == "" {
		return nil
	}
	page := wire.WorktreeBindingsPage{Bindings: sortByKey(slices.Collect(maps.Values(next)))}
	data, err := json.Marshal(page)
	if err != nil {
		return err
	}
	tmp := x.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, x.path)
}

func (x *Index) valid(bs []wire.WorktreeBinding) map[string]wire.WorktreeBinding {
	out := map[string]wire.WorktreeBinding{}
	for _, b := range bs {
		raw, _ := json.Marshal(b)
		if d, ok := x.decode(b.Key, raw); ok {
			out[d.Key] = d
		}
	}
	return out
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
	key = strings.ToUpper(key)
	if b, ok := x.byKey[key]; ok {
		return b, true
	}
	b, ok := x.pushed[key]
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
	all := make(map[string]wire.WorktreeBinding, len(x.byKey)+len(x.pushed))
	for k, b := range x.pushed {
		all[k] = b
	}
	for k, b := range x.byKey {
		all[k] = b
	}
	x.mu.RUnlock()
	out := []wire.WorktreeBinding{}
	for _, b := range all {
		if keep(b) {
			out = append(out, b)
		}
	}
	return sortByKey(out)
}

func sortByKey(bs []wire.WorktreeBinding) []wire.WorktreeBinding {
	slices.SortFunc(bs, func(a, b wire.WorktreeBinding) int { return strings.Compare(a.Key, b.Key) })
	return bs
}

// Routes serves GET /v1/worktree-bindings (the query key=, parent= or dir= narrows the page),
// and PUT /v1/worktree-bindings (the app sends all bindings).
func (x *Index) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /v1/worktree-bindings", Access: server.Member, Handle: x.list},
		{Pattern: "PUT /v1/worktree-bindings", Access: server.Member, Handle: x.put},
	}
}

func (x *Index) put(w http.ResponseWriter, r *http.Request) {
	var page wire.WorktreeBindingsPage
	if !server.Decode(w, r, 4<<20, "", &page) {
		return
	}
	if err := x.Put(page.Bindings); err != nil {
		server.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
