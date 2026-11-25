package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// frames records the frames that deliver gets, and tells whether the source app matched.
type frames struct {
	mu      sync.Mutex
	got     []wire.CIFrame
	matched bool
}

func (f *frames) deliver(match func(server.Caller) bool, msg any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.matched = match(server.Caller{Loopback: true}) && !match(server.Caller{DeviceID: "other"})
	f.got = append(f.got, msg.(wire.CIFrame))
	return 1
}

func (f *frames) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *frames) last() wire.CIFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

func newWatches(t *testing.T) (*api, *Watches, *frames) {
	t.Helper()
	a := seeded(t)
	a.put(mdoc(wire.MirrorPRs, "o/r", wire.CIPRsDoc{PRs: []wire.CIPullRef{{Number: 212, HeadRefName: "feat", HeadSha: sha1}}}))
	f := &frames{}
	w := NewWatches(a.x, f.deliver, a.feed, a.clk.now)
	w.Self = func() string { return "mbp" }
	return a, w, f
}

func TestWatchesAddResolves(t *testing.T) {
	_, w, f := newWatches(t)
	ans, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "#212"}, "mba")
	if err != nil {
		t.Fatal(err)
	}
	x := ans.Watch
	if ans.Done || x == nil || x.SHA != sha1 || x.PR != 212 || x.Branch != "feat" || x.TTL != 1800 || x.Machine != "mba" || x.ID != "o/r@"+sha1 ||
		!x.ExpiresAt.Equal(t0.Add(1800*time.Second)) || ans.Rollup == nil || ans.Rollup.Ref != "#212" {
		t.Fatalf("%+v", ans)
	}
	if f.count() != 1 || !f.matched || len(f.last().Watches) != 1 || f.last().Type != "ci" || f.last().Event != "watches" {
		t.Fatalf("frames: %+v", f.got)
	}
}

func TestWatchesDoneGivesNoWatch(t *testing.T) {
	a, w, f := newWatches(t)
	a.put(runsDoc("o/r", run("9001", "test", "completed", "success", sha1, "main")))
	ans, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main"}, "mba")
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Done || ans.Watch != nil || ans.Rollup == nil || ans.Rollup.State != wire.CISuccess || f.count() != 0 || len(w.List()) != 0 {
		t.Fatalf("%+v frames %d", ans, f.count())
	}
}

func TestWatchesUnknownRunGivesNoWatch(t *testing.T) {
	_, w, f := newWatches(t)
	ans, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "9002"}, "mba")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Done || ans.Watch != nil || ans.Rollup == nil || ans.Rollup.State != wire.CINone || ans.Rollup.Ref != "9002" || f.count() != 0 || len(w.List()) != 0 {
		t.Fatalf("%+v frames %d", ans, f.count())
	}
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/x", Ref: "9002"}, "mba"); !errors.Is(err, ErrUntracked) {
		t.Fatal(err)
	}
}

func TestWatchesDedupe(t *testing.T) {
	a, w, f := newWatches(t)
	first, _ := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "#212", TTL: 600}, "mba")
	a.clk.add(time.Minute)
	second, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: sha1[:10], TTL: 600}, "mini")
	if err != nil {
		t.Fatal(err)
	}
	list := w.List()
	if len(list) != 1 || !list[0].ExpiresAt.Equal(*second.Watch.ExpiresAt) || !list[0].ExpiresAt.After(*first.Watch.ExpiresAt) || list[0].Ref != "#212" {
		t.Fatalf("%+v", list)
	}
	// An earlier end does not shorten the watch.
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "#212", TTL: 60}, "mba"); err != nil {
		t.Fatal(err)
	}
	if got := w.List()[0]; !got.ExpiresAt.Equal(*second.Watch.ExpiresAt) || got.TTL != 600 || f.count() != 3 {
		t.Fatalf("%+v frames %d", got, f.count())
	}
}

func TestWatchesTTL(t *testing.T) {
	_, w, f := newWatches(t)
	for _, ttl := range []int{9000, 30, -5} {
		if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main", TTL: ttl}, "mba"); err != ErrBadTTL {
			t.Fatalf("ttl %d: %v", ttl, err)
		}
	}
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main", TTL: 7200}, "mba"); err != nil {
		t.Fatal(err)
	}
	if w.List()[0].TTL != 7200 {
		t.Fatalf("%+v", w.List())
	}
	w2 := NewWatches(w.index, f.deliver, w.source, w.now)
	if ans, _ := w2.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main"}, "mba"); ans.Watch.TTL != 1800 {
		t.Fatalf("%+v", ans.Watch)
	}
}

func TestWatchesSweepSendsEmptyList(t *testing.T) {
	a, w, f := newWatches(t)
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main", TTL: 60}, "mba"); err != nil {
		t.Fatal(err)
	}
	n := f.count()
	w.sweep()
	if f.count() != n {
		t.Fatal("the sweep sent a frame with no change")
	}
	a.clk.add(61 * time.Second)
	w.sweep()
	if f.count() != n+1 || len(f.last().Watches) != 0 || len(w.List()) != 0 {
		t.Fatalf("%+v", f.last())
	}
	if b, _ := json.Marshal(f.last()); !strings.Contains(string(b), `"watches":[]`) {
		t.Fatalf("the empty list must encode as []: %s", b)
	}
}

func TestWatchesCap(t *testing.T) {
	a, w, _ := newWatches(t)
	for i := 0; i < maxWatches; i++ {
		a.clk.add(time.Second)
		sha := fmt.Sprintf("a%039x", i)
		if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: sha}, "mba"); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.List()) != maxWatches {
		t.Fatalf("%d watches", len(w.List()))
	}
	a.clk.add(time.Second)
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: strings.Repeat("f", 40)}, "mba"); err != nil {
		t.Fatal(err)
	}
	list := w.List()
	ids := map[string]bool{}
	for _, x := range list {
		ids[x.SHA] = true
	}
	if len(list) != maxWatches || ids[fmt.Sprintf("a%039x", 0)] || !ids[strings.Repeat("f", 40)] || !ids[fmt.Sprintf("a%039x", 1)] {
		t.Fatalf("%d watches, first kept: %v", len(list), ids[fmt.Sprintf("a%039x", 0)])
	}
}

func TestWatchesRunDropsEnded(t *testing.T) {
	a, w, f := newWatches(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := w.Add(wire.CIWatchRequest{Repo: "o/r", Ref: "main"}, "mba"); err != nil {
		t.Fatal(err)
	}
	if len(w.List()) != 1 {
		t.Fatal("the run dropped a live watch")
	}
	a.put(runsDoc("o/r", run("9001", "test", "completed", "success", sha1, "main")))
	deadline := time.Now().Add(5 * time.Second)
	for len(w.List()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch stayed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for f.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.last().Watches) != 0 {
		t.Fatalf("%+v", f.last())
	}
}

func TestWatchesRoutes(t *testing.T) {
	a, w, _ := newWatches(t)
	mux := http.NewServeMux()
	for _, r := range w.Routes(NewForward(homeIs{machine: "mbp", home: "mbp"}, http.DefaultClient)) {
		mux.HandleFunc(r.Pattern, func(rw http.ResponseWriter, req *http.Request) {
			// A member caller on a daemon credential.
			r.Handle(rw, req.WithContext(server.WithCaller(req.Context(), server.Caller{DeviceID: "d1", Kind: string(wire.DeviceDaemon), Machine: "mba"})))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	post := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/v1/ci/watches", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	code, body := post(`{"repo":"o/r","ref":"#212"}`)
	var ans wire.CIWatchAnswer
	if json.Unmarshal([]byte(body), &ans); code != 200 || ans.Watch == nil || ans.Watch.Machine != "mba" {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := post(`{"repo":"o/other","ref":"main"}`); code != 404 || !strings.Contains(body, "does not track") {
		t.Fatalf("untracked: %d %s", code, body)
	}
	if code, body := post(`{"repo":"o/r","ref":"#999"}`); code != 200 || strings.Contains(body, `"watch"`) {
		t.Fatalf("unknown ref: %d %s", code, body)
	}
	if code, _ := post(`{"repo":"o/r","ref":"main","ttl":9000}`); code != 400 {
		t.Fatalf("ttl: %d", code)
	}
	if code, _ := post(`{"repo":"o/r"`); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	resp, err := http.Get(srv.URL + "/v1/ci/watches")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []wire.CIWatch
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || len(list) != 1 || list[0].SHA != sha1 {
		t.Fatalf("%v %+v", err, list)
	}
	_ = a
}

func TestWatchesRoutesForwardBody(t *testing.T) {
	_, home, _ := newWatches(t)
	var s seen
	mux := http.NewServeMux()
	for _, r := range home.Routes(NewForward(homeIs{machine: "mbp", home: "mbp"}, http.DefaultClient)) {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	hs := httptest.NewServer(s.wrap(mux))
	t.Cleanup(hs.Close)
	mem := NewWatches(home.index, nil, home.source, home.now)
	m := httptest.NewServer(NewForward(homeIs{machine: "mba", home: "mbp", base: hs.URL, token: "tok"}, &http.Client{Timeout: 5 * time.Second}).Wrap(mem.post))
	t.Cleanup(m.Close)
	resp, err := http.Post(m.URL+"/v1/ci/watches", "application/json", strings.NewReader(`{"repo":"o/r","ref":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(home.List()) != 1 || len(mem.List()) != 0 {
		t.Fatalf("%d home %d member %d", resp.StatusCode, len(home.List()), len(mem.List()))
	}
	if r := s.reqs[0]; r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get(ForwardedHeader) != "1" {
		t.Fatalf("%s %v", r.Method, r.Header)
	}
}
