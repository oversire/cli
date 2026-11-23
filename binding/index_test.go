package binding

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

type recs map[string]json.RawMessage

func (r recs) Records(string) (map[string]json.RawMessage, error) { return r, nil }

func bind(key, machine, group, parent string, mode wire.WorktreeMode) wire.WorktreeBinding {
	return wire.WorktreeBinding{Key: key, Machine: machine, Root: "/w", Group: group, Branch: "b", Parent: parent, Mode: mode,
		Source: "create", UpdatedAt: time.Unix(0, 0).UTC()}
}

func put(b wire.WorktreeBinding) wire.StoreOp {
	raw, _ := json.Marshal(b)
	return wire.StoreOp{Collection: wire.CollectionWorktreeBindings, ID: b.Key, Op: wire.StorePut, Record: raw}
}

func index(t *testing.T, bs ...wire.WorktreeBinding) *Index {
	t.Helper()
	r := recs{}
	for _, b := range bs {
		r[b.Key], _ = json.Marshal(b)
	}
	x := NewIndex("mbp", nil)
	if err := x.Load(r); err != nil {
		t.Fatal(err)
	}
	return x
}

func keys(bs []wire.WorktreeBinding) []string {
	out := []string{}
	for _, b := range bs {
		out = append(out, b.Key)
	}
	return out
}

func TestIndexChildren(t *testing.T) {
	x := index(t, bind("P-3", "mbp", ".wt/p-3", "P", wire.WorktreeOwn), bind("P-2", "mini", ".wt/p-2", "P", wire.WorktreeOwn),
		bind("Q-1", "mbp", ".wt/q-1", "", wire.WorktreeOwn))
	if got := keys(x.Children("p")); len(got) != 2 || got[0] != "P-2" || got[1] != "P-3" {
		t.Fatalf("children = %v", got)
	}
	if got := keys(x.Mine()); len(got) != 2 || got[0] != "P-3" || got[1] != "Q-1" {
		t.Fatalf("mine = %v", got)
	}
}

func TestIndexApplyDelete(t *testing.T) {
	x := index(t)
	x.Apply([]wire.StoreOp{put(bind("P-1", "mbp", ".wt/p-1", "P", wire.WorktreeOwn)), {Collection: "agents", ID: "P-2", Op: wire.StorePut}})
	if _, ok := x.Get("p-1"); !ok || len(x.Children("P")) != 1 {
		t.Fatal("the put is missing")
	}
	x.Apply([]wire.StoreOp{{Collection: wire.CollectionWorktreeBindings, ID: "P-1", Op: wire.StoreDelete}})
	if _, ok := x.Get("P-1"); ok || len(x.Children("P")) != 0 {
		t.Fatal("the delete did not drop the key")
	}
}

func TestIndexAt(t *testing.T) {
	x := index(t, bind("A-1", "mbp", ".wt/a-1", "", wire.WorktreeOwn), bind("A-2", "mbp", "/w/.wt/a-1/cli/sub", "", wire.WorktreeOwn),
		bind("A-3", "mini", "/w/other", "", wire.WorktreeOwn), bind("A-4", "mbp", ".wt/a-1", "A-1", wire.WorktreeParent))
	if b, ok := x.At("/w/.wt/a-1/oversire/Sources"); !ok || b.Key != "A-1" {
		t.Fatalf("at = %v %v", b.Key, ok)
	}
	if b, ok := x.At("/w/.wt/a-1/cli/sub/x"); !ok || b.Key != "A-2" {
		t.Fatalf("the longest folder wins: %v", b.Key)
	}
	if _, ok := x.At("/w/other"); ok {
		t.Fatal("a binding of another machine")
	}
	if _, ok := x.At("/w/.wt/a-10"); ok {
		t.Fatal("a folder with the same prefix")
	}
	if x.GroupPath("A-1") != ".wt/a-1" || x.GroupPath("A-3") != "" {
		t.Fatal("group path")
	}
}

func TestIndexSkipsBadGroup(t *testing.T) {
	x := index(t, bind("B-1", "mbp", "../x", "", wire.WorktreeOwn), bind("B-2", "mbp", ".wt/b-2", "", wire.WorktreeOwn))
	if _, ok := x.Get("B-1"); ok {
		t.Fatal("a .. group is in the index")
	}
	if _, ok := x.Get("B-2"); !ok {
		t.Fatal("a good record is missing")
	}
}

func TestBindingsRoute(t *testing.T) {
	x := index(t, bind("P-1", "mbp", ".wt/p-1", "P", wire.WorktreeOwn), bind("P-2", "mbp", ".wt/p-2", "P", wire.WorktreeOwn))
	get := func(q string) (int, wire.WorktreeBindingsPage) {
		w := httptest.NewRecorder()
		x.list(w, httptest.NewRequest(http.MethodGet, "/v1/worktree-bindings?"+q, nil))
		var p wire.WorktreeBindingsPage
		json.Unmarshal(w.Body.Bytes(), &p)
		return w.Code, p
	}
	if code, p := get("parent=P"); code != 200 || len(p.Bindings) != 2 {
		t.Fatalf("parent: %d %+v", code, p)
	}
	if code, p := get("dir=/nowhere"); code != 200 || p.Bindings == nil || len(p.Bindings) != 0 {
		t.Fatalf("dir: %d %+v", code, p)
	}
	if code, p := get("key=p-2"); code != 200 || len(p.Bindings) != 1 {
		t.Fatalf("key: %d %+v", code, p)
	}
	if code, _ := get("dir=rel"); code != 400 {
		t.Fatalf("a relative dir: %d", code)
	}
	if code, p := get(""); code != 200 || len(p.Bindings) != 2 {
		t.Fatalf("all: %d %+v", code, p)
	}
}
