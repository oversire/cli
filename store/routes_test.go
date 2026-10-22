package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// fakeGroup is the roster of a test.
type fakeGroup struct {
	mu   sync.Mutex
	g    wire.Group
	self string
}

func (f *fakeGroup) Snapshot() (wire.Group, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.g, true
}

func (f *fakeGroup) Self() wire.GroupMember { return wire.GroupMember{Machine: f.self} }

func (f *fakeGroup) set(fn func(*wire.Group)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.g)
}

// home is one daemon with the store routes on a loopback test server.
type home struct {
	log    *OpLog
	group  *fakeGroup
	routes *Routes
	frames []any
	srv    *httptest.Server
}

func newHome(t *testing.T, machine, homeMachine string) *home {
	t.Helper()
	h := &home{log: openLog(t), group: &fakeGroup{g: wire.Group{ID: "g", Home: homeMachine}, self: machine}}
	h.routes = NewRoutes(h.log, h.group, func(m any) { h.frames = append(h.frames, m) }, nil)
	mux := http.NewServeMux()
	for _, r := range h.routes.Routes() {
		if r.Access != server.Member {
			t.Fatalf("%s: access %v", r.Pattern, r.Access)
		}
		mux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
	}
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *home) call(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestRoutesOnHome(t *testing.T) {
	h := newHome(t, "mbp", "mbp")
	put := storeOp(t, "put.json")
	code, data := h.call(t, http.MethodPost, "/v1/store/ops", wire.StoreOps{Ops: []wire.StoreOp{put}})
	var res wire.StoreApplyResult
	if code != http.StatusOK || json.Unmarshal(data, &res) != nil || len(res.Accepted) != 1 || res.Head != 1 {
		t.Fatalf("%d %s", code, data)
	}
	if len(h.frames) != 1 || fmt.Sprint(h.frames[0]) != fmt.Sprint(map[string]any{"type": "store", "head": 1}) {
		t.Fatalf("%v", h.frames)
	}
	// A write that loses sends no frame.
	h.call(t, http.MethodPost, "/v1/store/ops", wire.StoreOps{Ops: []wire.StoreOp{put}})
	if len(h.frames) != 1 {
		t.Fatalf("%v", h.frames)
	}

	code, data = h.call(t, http.MethodGet, "/v1/store/ops?after=0&limit=0", nil)
	var p wire.StorePull
	if code != http.StatusOK || json.Unmarshal(data, &p) != nil || len(p.Ops) != 1 || p.Cursor != 1 {
		t.Fatalf("%d %s", code, data)
	}
	if code, data := h.call(t, http.MethodGet, "/v1/store/ops?after=1", nil); code != http.StatusOK ||
		string(bytes.TrimSpace(data)) != `{"ops":[],"cursor":1,"more":false,"reset":false}` {
		t.Fatalf("%d %s", code, data)
	}

	hub := put
	hub.Collection = "billing"
	code, data = h.call(t, http.MethodPost, "/v1/store/ops", wire.StoreOps{Ops: []wire.StoreOp{hub}})
	if code != http.StatusBadRequest || string(bytes.TrimSpace(data)) != `{"error":"ops[0]: only the hub writes billing"}` {
		t.Fatalf("%d %s", code, data)
	}
}

func TestRoutesWhenNotHome(t *testing.T) {
	h := newHome(t, "mbp", "mba")
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		code, data := h.call(t, m, "/v1/store/ops", wire.StoreOps{Ops: []wire.StoreOp{}})
		if code != http.StatusConflict || string(bytes.TrimSpace(data)) != `{"error":"mba is home","home":"mba"}` {
			t.Fatalf("%s: %d %s", m, code, data)
		}
	}
	h.group.set(func(g *wire.Group) { g.Home, g.Cloud = "mbp", &wire.GroupCloud{Hub: "https://hub.oversire.dev", Account: "a1"} })
	code, data := h.call(t, http.MethodGet, "/v1/store/ops", nil)
	if code != http.StatusConflict ||
		string(bytes.TrimSpace(data)) != `{"error":"the cloud is home","cloud":{"hub":"https://hub.oversire.dev","account":"a1"}}` {
		t.Fatalf("%d %s", code, data)
	}
}

func TestTakeHome(t *testing.T) {
	old := newHome(t, "mba", "mba")
	put := storeOp(t, "put.json")
	var ops []wire.StoreOp
	for i := range PageLimit + 1 {
		op := put
		op.ID = fmt.Sprintf("p%d", i)
		ops = append(ops, op)
	}
	mustApply(t, old.log, ops...)
	del := storeOp(t, "delete.json")
	del.ID = "gone"
	mustApply(t, old.log, del)

	// The new home has an older and a newer value of two records.
	next := newHome(t, "mbp", "mba")
	older, newer := put, put
	older.ID, older.HLC = "p1", hlc(1, 0, "mac")
	newer.ID, newer.HLC = "p2", hlc(9_000_000_000_000, 0, "mac")
	mustApply(t, next.log, older, newer)

	n, offline, err := next.routes.TakeHome(t.Context(), old.srv.URL, "token")
	if err != nil || offline || n != PageLimit+2 {
		t.Fatalf("%d %v %v", n, offline, err)
	}
	all := mustPull(t, next.log, 0, PageLimit+10)
	if len(all.Ops) != PageLimit+2 {
		t.Fatal(len(all.Ops))
	}
	for _, op := range all.Ops {
		switch op.ID {
		case "p1":
			if op.HLC != put.HLC {
				t.Fatal("the newer hlc of the old home wins:", op.HLC)
			}
		case "p2":
			if op.HLC != newer.HLC {
				t.Fatal("the newer local value stays:", op.HLC)
			}
		case "gone":
			if op.Op != wire.StoreDelete || op.HLC != del.HLC {
				t.Fatal("the tombstone keeps its hlc:", op)
			}
		}
	}
	if len(next.frames) == 0 {
		t.Fatal("the clients of the new home get a store frame")
	}

	// An old home that does not answer.
	old.srv.Close()
	if n, offline, err := next.routes.TakeHome(t.Context(), old.srv.URL, "token"); err != nil || !offline || n != 0 {
		t.Fatalf("%d %v %v", n, offline, err)
	}
	// An old home that is not home: the move stops.
	other := newHome(t, "mba", "mbx")
	if _, _, err := next.routes.TakeHome(t.Context(), other.srv.URL, "token"); err == nil {
		t.Fatal("a 409 must stop the move")
	}
}
