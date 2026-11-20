package mirror

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeGroup struct {
	g    wire.Group
	ok   bool
	self string
}

func (f *fakeGroup) Snapshot() (wire.Group, bool) { return f.g, f.ok }
func (f *fakeGroup) Self() wire.GroupMember       { return wire.GroupMember{Machine: f.self} }

type fakeHolder struct{ l *wire.Lease }

func (f *fakeHolder) Get(name string) (wire.Lease, bool) {
	if f.l == nil || name != "automations" {
		return wire.Lease{}, false
	}
	return *f.l, true
}

type fixture struct {
	store  *Store
	holder *fakeHolder
	srv    *httptest.Server
	mu     sync.Mutex
	frames []any
	online func(Source) bool
	caller server.Caller
}

var (
	mbaApp  = server.Caller{DeviceID: "D-mba", Name: "OverSire on mba", Kind: string(wire.DeviceApp), Machine: "mba"}
	miniApp = server.Caller{DeviceID: "D-mini", Name: "OverSire on mini", Kind: string(wire.DeviceApp), Machine: "mini"}
	daemon  = server.Caller{DeviceID: "D-d", Name: "mba daemon", Kind: string(wire.DeviceDaemon), Machine: "mba"}
	local   = server.Caller{Loopback: true}
)

// newFixture serves the routes of a home daemon "mbp". The test sets f.caller before each call.
func newFixture(t *testing.T, g *fakeGroup) *fixture {
	t.Helper()
	s, _ := newStore(t)
	f := &fixture{store: s, holder: &fakeHolder{l: &wire.Lease{Name: "automations", Holder: "app-1", HolderName: "mbp", Epoch: 1, Until: time.Now().Add(time.Hour)}}, caller: local}
	if g == nil {
		g = &fakeGroup{g: wire.Group{ID: "g", Home: "mbp"}, ok: true, self: "mbp"}
	}
	routes := NewRoutes(s, g, f.holder, g.Self, func(src Source) bool { return f.online != nil && f.online(src) }, func(m any) {
		f.mu.Lock()
		f.frames = append(f.frames, m)
		f.mu.Unlock()
	})
	mux := http.NewServeMux()
	for _, r := range routes.Routes() {
		if r.Access != server.Member {
			t.Fatalf("%s: access %v", r.Pattern, r.Access)
		}
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mux.ServeHTTP(w, req.WithContext(server.WithCaller(req.Context(), f.caller)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) do(t *testing.T, method, path string, body any, header ...string) (int, []byte, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func (f *fixture) put(t *testing.T, docs ...wire.MirrorDoc) (int, []byte) {
	t.Helper()
	p := put(f.store, docs...)
	code, data, _ := f.do(t, http.MethodPut, "/v1/mirror/docs", p)
	return code, data
}

func TestPutAndReadEndToEnd(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	code, data := f.put(t, doc(wire.MirrorItem, "p/A-1", `{"n":1}`), doc(wire.MirrorCatalog, "home", `{}`))
	var res wire.MirrorPutResult
	if err := json.Unmarshal(data, &res); err != nil || code != 200 || res.Applied != 2 || res.Head.Head != 2 {
		t.Fatalf("put: %d %s", code, data)
	}
	f.caller = miniApp
	code, data, _ = f.do(t, "GET", "/v1/mirror/docs?after=1&epoch="+res.Head.Epoch, nil)
	var page wire.MirrorPage
	if err := json.Unmarshal(data, &page); err != nil || code != 200 || len(page.Docs) != 1 || page.Docs[0].ID != "home" {
		t.Fatalf("docs: %d %s", code, data)
	}
	code, data, _ = f.do(t, "GET", "/v1/mirror/index", nil)
	var idx wire.MirrorIndex
	if err := json.Unmarshal(data, &idx); err != nil || code != 200 || len(idx.Docs) != 2 {
		t.Fatalf("index: %d %s", code, data)
	}
	code, data, _ = f.do(t, "GET", "/v1/mirror", nil)
	var head wire.MirrorHead
	if err := json.Unmarshal(data, &head); err != nil || code != 200 || head.Head != 2 || head.SourceMachine != "mba" || head.SourceName != "mbp" {
		t.Fatalf("head: %d %s", code, data)
	}
}

func TestHeadShowsTheSourceOnline(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`))
	var got Source
	f.online = func(s Source) bool { got = s; return true }
	_, data, _ := f.do(t, "GET", "/v1/mirror", nil)
	var head wire.MirrorHead
	_ = json.Unmarshal(data, &head)
	if !head.SourceOnline || got != (Source{App: "app-1", Device: "D-mba", Name: "OverSire on mba", Machine: "mba"}) {
		t.Fatalf("%s %+v", data, got)
	}
	f.online = nil
	_, data, _ = f.do(t, "GET", "/v1/mirror", nil)
	if bytes.Contains(data, []byte("sourceOnline")) {
		t.Fatalf("an offline source: %s", data)
	}
}

func TestRoutesWhenNotHome(t *testing.T) {
	for name, g := range map[string]*fakeGroup{
		"another home": {g: wire.Group{ID: "g", Home: "mbp"}, ok: true, self: "mba"},
		// The cloud does not move the mirror: only home serves it.
		"home with cloud": {g: wire.Group{ID: "g", Home: "mbp", Cloud: &wire.GroupCloud{Hub: "h", Account: "a"}}, ok: true, self: "mba"},
	} {
		f := newFixture(t, g)
		f.caller = mbaApp
		for _, c := range [][2]string{{"GET", "/v1/mirror"}, {"GET", "/v1/mirror/docs"}, {"GET", "/v1/mirror/index"}, {"PUT", "/v1/mirror/docs"}} {
			code, data, _ := f.do(t, c[0], c[1], map[string]any{})
			var e struct{ Error, Home string }
			if code != 409 || json.Unmarshal(data, &e) != nil || e.Home != "mbp" || e.Error != "mbp is home" {
				t.Errorf("%s %s %s: %d %s", name, c[0], c[1], code, data)
			}
		}
	}
	f := newFixture(t, &fakeGroup{self: "mba"})
	if code, _, _ := f.do(t, "GET", "/v1/mirror", nil); code != 409 {
		t.Fatalf("no group: %d", code)
	}
}

func TestRoutesOnHomeWithCloud(t *testing.T) {
	f := newFixture(t, &fakeGroup{g: wire.Group{ID: "g", Home: "mbp", Cloud: &wire.GroupCloud{Hub: "h", Account: "a"}}, ok: true, self: "mbp"})
	f.caller = mbaApp
	if code, data := f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`)); code != 200 {
		t.Fatalf("%d %s", code, data)
	}
}

func TestPutNeedsLeaseHolder(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	p := put(f.store, doc(wire.MirrorItem, "p/A-1", `{}`))
	p.Source, p.SourceName = "app-2", "mba"
	code, data, _ := f.do(t, "PUT", "/v1/mirror/docs", p)
	if code != 403 || string(bytes.TrimSpace(data)) != `{"error":"mba does not hold the automations lease."}` {
		t.Fatalf("%d %s", code, data)
	}
	f.holder.l = nil
	if code, _ = f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`)); code != 403 {
		t.Fatalf("an expired lease: %d", code)
	}
	if f.store.Head().Head != 0 {
		t.Fatal("a refused put wrote")
	}
}

func TestPutRejectsDaemonCredential(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = daemon
	for _, c := range [][2]string{{"PUT", "/v1/mirror/docs"}, {"GET", "/v1/mirror/index"}} {
		code, data, _ := f.do(t, c[0], c[1], put(f.store, doc(wire.MirrorItem, "p/A-1", `{}`)))
		if code != 403 || string(bytes.TrimSpace(data)) != `{"error":"only an app writes the mirror"}` {
			t.Fatalf("%s: %d %s", c[1], code, data)
		}
	}
	if f.store.Head().Head != 0 {
		t.Fatal("a daemon wrote")
	}
	// A daemon reads the head and the docs, as the apps of its machine do.
	if code, _, _ := f.do(t, "GET", "/v1/mirror", nil); code != 200 {
		t.Fatalf("head: %d", code)
	}
	if code, _, _ := f.do(t, "GET", "/v1/mirror/docs", nil); code != 200 {
		t.Fatalf("docs: %d", code)
	}
}

func TestPutAcceptsLoopback(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = local
	if code, data := f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`)); code != 200 {
		t.Fatalf("%d %s", code, data)
	}
	src, _ := f.store.Source()
	if !src.Loopback || src.Machine != "mbp" || src.Device != "" {
		t.Fatalf("%+v", src)
	}
}

func TestPutErrors(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	p := put(f.store, doc(wire.MirrorItem, "p/A-1", `{}`))
	p.Epoch = "old"
	code, data, _ := f.do(t, "PUT", "/v1/mirror/docs", p)
	var e struct{ Error, Epoch string }
	if code != 409 || json.Unmarshal(data, &e) != nil || e.Epoch != f.store.Head().Epoch || e.Error != "The mirror changed. Send all again." {
		t.Fatalf("epoch: %d %s", code, data)
	}
	var many []wire.MirrorDoc
	for i := 0; i <= MaxDocs; i++ {
		many = append(many, doc(wire.MirrorItem, "p/"+time.Duration(i).String(), `{}`))
	}
	code, data = f.put(t, many...)
	if code != 413 || string(bytes.TrimSpace(data)) != `{"error":"The batch is too large."}` {
		t.Fatalf("too large: %d %s", code, data)
	}
	if code, _ = f.put(t, doc("nope", "x", `{}`)); code != 400 {
		t.Fatalf("bad doc: %d", code)
	}
	if code, _, _ = f.do(t, "PUT", "/v1/mirror/docs", map[string]any{"epoch": f.store.Head().Epoch, "extra": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	big := bytes.Repeat([]byte("a"), maxBody+10)
	req, _ := http.NewRequest("PUT", f.srv.URL+"/v1/mirror/docs", bytes.NewReader(big))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("body over the limit: %d", resp.StatusCode)
	}
}

func TestPutSendsFrame(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) != 1 {
		t.Fatalf("%d frames", len(f.frames))
	}
	b, _ := json.Marshal(f.frames[0])
	if string(b) != `{"type":"mirror","head":1,"epoch":"`+f.store.Head().Epoch+`"}` {
		t.Fatalf("%s", b)
	}
	f.mu.Unlock()
	f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`))
	f.mu.Lock()
	if len(f.frames) != 1 {
		t.Fatal("a put that changed nothing sent a frame")
	}
	f.mu.Unlock()
	p := put(f.store)
	p.Ready = true
	f.do(t, "PUT", "/v1/mirror/docs", p)
	f.mu.Lock()
	if len(f.frames) != 2 {
		t.Fatal("ready did not send a frame")
	}
}

func TestHeadRecordsReaders(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.do(t, "GET", "/v1/mirror", nil)
	f.caller = miniApp
	f.do(t, "GET", "/v1/mirror", nil)
	_, data, _ := f.do(t, "GET", "/v1/mirror", nil)
	var h wire.MirrorHead
	_ = json.Unmarshal(data, &h)
	if len(h.Readers) != 2 || h.Readers[0].Machine != "mba" || h.Readers[0].Device != "D-mba" || h.Readers[0].Name != "OverSire on mba" ||
		h.Readers[1].Machine != "mini" {
		t.Fatalf("%s", data)
	}
	f.caller = local
	f.do(t, "GET", "/v1/mirror", nil)
	if r := f.store.Head().Readers; len(r) != 3 || r[1].Machine != "mbp" || r[1].Device != "" {
		t.Fatalf("a loopback reader has the machine of this daemon: %+v", r)
	}
}

func TestPeekDoesNotRecordAReader(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.do(t, "GET", "/v1/mirror?peek=1", nil)
	f.caller = local
	_, data, _ := f.do(t, "GET", "/v1/mirror?peek=1", nil)
	var h wire.MirrorHead
	_ = json.Unmarshal(data, &h)
	if len(h.Readers) != 0 {
		t.Fatalf("a peek recorded a reader: %s", data)
	}
}

func TestDocsRecordTheReaderHead(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.put(t, doc(wire.MirrorItem, "p/A-1", `{}`), doc(wire.MirrorItem, "p/A-2", `{}`))
	epoch := f.store.Head().Epoch
	f.caller = miniApp
	f.do(t, "GET", "/v1/mirror/docs?limit=1", nil)
	if r := f.store.Head().Readers; len(r) != 1 || r[0].Head != 1 {
		t.Fatalf("%+v", r)
	}
	f.do(t, "GET", "/v1/mirror/docs?after=1&epoch="+epoch, nil)
	f.do(t, "GET", "/v1/mirror", nil)
	if r := f.store.Head().Readers; r[0].Head != 2 {
		t.Fatalf("the head route keeps the head: %+v", r)
	}
	f.do(t, "GET", "/v1/mirror/docs?after=2&epoch=other", nil)
	if r := f.store.Head().Readers; r[0].Head != 0 {
		t.Fatalf("a reset: %+v", r)
	}
}

func TestDocsGzip(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	f.put(t, doc(wire.MirrorItem, "p/A-1", `{"n":1}`))
	code, data, h := f.do(t, "GET", "/v1/mirror/docs", nil, "Accept-Encoding", "gzip")
	if code != 200 || h.Get("Content-Encoding") != "gzip" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", code, h)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	var page wire.MirrorPage
	if err := json.Unmarshal(plain, &page); err != nil || len(page.Docs) != 1 {
		t.Fatalf("%s", plain)
	}
	_, data, h = f.do(t, "GET", "/v1/mirror/docs", nil)
	if h.Get("Content-Encoding") != "" || !json.Valid(data) {
		t.Fatalf("no Accept-Encoding: %v", h)
	}
}

func TestDocsQueryRules(t *testing.T) {
	f := newFixture(t, nil)
	f.caller = mbaApp
	var docs []wire.MirrorDoc
	for _, id := range []string{"a", "b", "c"} {
		docs = append(docs, doc(wire.MirrorItem, id, `{}`))
	}
	f.put(t, docs...)
	for path, want := range map[string]int{
		"/v1/mirror/docs":                 3,
		"/v1/mirror/docs?limit=2":         2,
		"/v1/mirror/docs?limit=0":         3,
		"/v1/mirror/docs?limit=9999":      3,
		"/v1/mirror/docs?after=-4":        3,
		"/v1/mirror/docs?after=x&limit=y": 3,
	} {
		_, data, _ := f.do(t, "GET", path, nil)
		var p wire.MirrorPage
		if json.Unmarshal(data, &p) != nil || len(p.Docs) != want {
			t.Errorf("%s: %s", path, data)
		}
	}
}
