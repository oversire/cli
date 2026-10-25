package lease

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeGroup struct {
	g    wire.Group
	ok   bool
	self string
}

func (f *fakeGroup) Snapshot() (wire.Group, bool) { return f.g, f.ok }
func (f *fakeGroup) Self() wire.GroupMember        { return wire.GroupMember{Machine: f.self} }

func serve(t *testing.T, g *fakeGroup) *httptest.Server {
	t.Helper()
	b, _, _ := newBook(t)
	mux := http.NewServeMux()
	for _, r := range NewRoutes(b, g).Routes() {
		if r.Access != server.Member {
			t.Fatalf("%s: access %v", r.Pattern, r.Access)
		}
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestRoutesOnHome(t *testing.T) {
	srv := serve(t, &fakeGroup{g: wire.Group{ID: "g", Home: "mbp", Cloud: &wire.GroupCloud{Hub: "h", Account: "a"}}, ok: true, self: "mbp"})
	const path = "/v1/leases/automations"

	if code, _ := call(t, srv, "GET", path, nil); code != 404 {
		t.Fatalf("free lease: %d", code)
	}
	code, data := call(t, srv, "POST", path, req("a", false))
	var l wire.Lease
	if code != 200 || json.Unmarshal(data, &l) != nil || l.Holder != "a" || l.Epoch != 1 {
		t.Fatalf("acquire: %d %s", code, data)
	}
	code, data = call(t, srv, "POST", path, req("b", false))
	if code != 409 || json.Unmarshal(data, &l) != nil || l.Holder != "a" {
		t.Fatalf("held: %d %s", code, data)
	}
	if code, data = call(t, srv, "POST", path, req("b", true)); code != 200 {
		t.Fatalf("take: %d %s", code, data)
	}
	if code, _ = call(t, srv, "DELETE", path+"?holder=a", nil); code != 403 {
		t.Fatalf("release by a non-holder: %d", code)
	}
	if code, _ = call(t, srv, "DELETE", path+"?holder=b", nil); code != 204 {
		t.Fatalf("release: %d", code)
	}
	if code, _ = call(t, srv, "POST", path, map[string]any{"holder": "a", "holderName": "x", "ttlSeconds": 90, "take": false, "extra": 1}); code != 400 {
		t.Fatalf("an unknown field: %d", code)
	}
	if code, _ = call(t, srv, "POST", path, wire.LeaseRequest{Holder: "a", HolderName: "x", TTLSeconds: 5}); code != 400 {
		t.Fatalf("a short ttl: %d", code)
	}
}

func TestRoutesNotHome(t *testing.T) {
	srv := serve(t, &fakeGroup{g: wire.Group{ID: "g", Home: "mbp"}, ok: true, self: "mba"})
	code, data := call(t, srv, "POST", "/v1/leases/automations", req("a", false))
	var e notHome
	if code != 409 || json.Unmarshal(data, &e) != nil || e.Home != "mbp" {
		t.Fatalf("not home: %d %s", code, data)
	}

	srv = serve(t, &fakeGroup{self: "mba"})
	if code, data = call(t, srv, "GET", "/v1/leases/automations", nil); code != 409 {
		t.Fatalf("no group: %d %s", code, data)
	}
}
