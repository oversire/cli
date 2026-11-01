package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

var t0 = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "events.db"), func() time.Time { return t0.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func draft(id, kind, item string) wire.EventDraft {
	return wire.EventDraft{ID: id, At: t0, Kind: kind, Item: item, ActorType: wire.ActorAgent, Actor: "claude/opus-5-5",
		Via: "hook", Summary: kind + " " + item, Payload: json.RawMessage(`{"turn":1}`)}
}

func TestAppendDropsARepeat(t *testing.T) {
	s, ctx := open(t), context.Background()
	rows, err := s.Append(ctx, []wire.EventDraft{draft("hook:a", "turn.ended", "oversire-374"), draft("hook:a", "turn.ended", "OVERSIRE-374")})
	if err != nil || len(rows) != 1 || rows[0].Item != "OVERSIRE-374" || rows[0].Seq != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	rows, _ = s.Append(ctx, []wire.EventDraft{draft("hook:a", "turn.ended", "OVERSIRE-374")})
	if len(rows) != 0 {
		t.Fatalf("a repeat is new: %+v", rows)
	}
	if h, _ := s.Head(ctx); h != 1 {
		t.Fatalf("head %d", h)
	}
}

func TestNormalize(t *testing.T) {
	d := draft("hook:b", "turn.ended", "X-1")
	d.Summary = strings.Repeat("é", 300)
	d.Payload = json.RawMessage(`{"a":"` + strings.Repeat("x", MaxEventPayloadForTest) + `"}`)
	n, err := Normalize(d)
	if err != nil || len([]rune(n.Summary)) != wire.MaxEventSummary || string(n.Payload) != "{}" {
		t.Fatalf("%d %s %v", len([]rune(n.Summary)), n.Payload, err)
	}
	for _, bad := range []func(*wire.EventDraft){
		func(d *wire.EventDraft) { d.ID = "nosource" },
		func(d *wire.EventDraft) { d.Kind = "Turn" },
		func(d *wire.EventDraft) { d.ActorType = "robot" },
		func(d *wire.EventDraft) { d.Via = "" },
	} {
		d := draft("hook:c", "turn.ended", "X-1")
		bad(&d)
		if _, err := Normalize(d); err == nil {
			t.Fatalf("no error for %+v", d)
		}
	}
}

const MaxEventPayloadForTest = wire.MaxEventPayload

func TestQueryByItemAndTree(t *testing.T) {
	s, ctx := open(t), context.Background()
	parent := draft("hook:p", "plan.started", "OVERSIRE-371")
	child := draft("hook:c", "turn.ended", "OVERSIRE-374")
	child.ParentItem = "OVERSIRE-371"
	late := draft("git:1", "git.commit", "OVERSIRE-375") // the link comes later
	other := draft("hook:o", "turn.ended", "OVERSIRE-9")
	if _, err := s.Append(ctx, []wire.EventDraft{parent, child, late, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLinks(ctx, []wire.ItemLink{{Item: "OVERSIRE-375", Parent: "OVERSIRE-371"}}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(ctx, wire.EventQuery{Item: "oversire-371"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	page, _ = s.Query(ctx, wire.EventQuery{Item: "OVERSIRE-371", Tree: true})
	if ids(page) != "hook:p hook:c git:1" || page.Events[2].ParentItem != "OVERSIRE-371" {
		t.Fatalf("tree %s %+v", ids(page), page.Events)
	}
	page, _ = s.Query(ctx, wire.EventQuery{Item: "OVERSIRE-371", Tree: true, Kinds: []string{"git.*", "plan.started"}})
	if ids(page) != "hook:p git:1" {
		t.Fatalf("kinds %s", ids(page))
	}
	page, _ = s.Query(ctx, wire.EventQuery{Item: "OVERSIRE-371", Tree: true, Limit: 1})
	if ids(page) != "hook:p" || !page.More || page.Next != 1 || page.Head != 4 {
		t.Fatalf("limit %+v", page)
	}
	page, _ = s.Query(ctx, wire.EventQuery{Item: "OVERSIRE-371", Tree: true, After: page.Next})
	if ids(page) != "hook:c git:1" || page.More {
		t.Fatalf("after %+v", page)
	}
	// A new row of a linked item gets its parent at write time.
	rows, _ := s.Append(ctx, []wire.EventDraft{draft("git:2", "git.push", "OVERSIRE-375")})
	if rows[0].ParentItem != "OVERSIRE-371" {
		t.Fatalf("%+v", rows[0])
	}
	// A move to another parent changes the old rows.
	s.SetLinks(ctx, []wire.ItemLink{{Item: "OVERSIRE-375", Parent: "OVERSIRE-300"}})
	page, _ = s.Query(ctx, wire.EventQuery{Item: "OVERSIRE-300", Tree: true})
	if ids(page) != "git:1 git:2" {
		t.Fatalf("moved %s", ids(page))
	}
}

func ids(p wire.EventPage) string {
	var out []string
	for _, e := range p.Events {
		out = append(out, e.ID)
	}
	return strings.Join(out, " ")
}

func TestRetentionKeepsTheSummary(t *testing.T) {
	s, ctx := open(t), context.Background()
	old := draft("hook:old", "turn.ended", "X-1")
	prompt := draft("hook:prompt", "prompt.submitted", "X-1")
	prompt.ActorType, prompt.Private, prompt.Summary = wire.ActorHuman, true, "fix the build"
	fresh := draft("hook:new", "turn.ended", "X-1")
	fresh.At = t0.Add(80 * 24 * time.Hour)
	s.Append(ctx, []wire.EventDraft{old, prompt, fresh})
	n, err := s.Prune(ctx, t0.Add(91*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	page, _ := s.Query(ctx, wire.EventQuery{Item: "X-1"})
	if e := page.Events[0]; e.Payload != nil || e.Summary != "turn.ended X-1" {
		t.Fatalf("old %+v", e)
	}
	if e := page.Events[1]; e.Payload != nil || e.Summary != "" {
		t.Fatalf("prompt %+v", e)
	}
	if e := page.Events[2]; string(e.Payload) != `{"turn":1}` {
		t.Fatalf("fresh %+v", e)
	}
}

func TestSpoolDrain(t *testing.T) {
	s, ctx := open(t), context.Background()
	sp := NewSpool(filepath.Join(t.TempDir(), "events.spool"))
	if n, err := sp.Drain(ctx, s); n != 0 || err != nil {
		t.Fatalf("empty %d %v", n, err)
	}
	sp.Write([]wire.EventDraft{draft("hook:1", "turn.ended", "X-1"), draft("hook:1", "turn.ended", "X-1")})
	f, _ := os.OpenFile(sp.path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	sp.Write([]wire.EventDraft{draft("hook:2", "turn.ended", "X-1")})
	n, err := sp.Drain(ctx, s)
	if n != 2 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := os.Stat(sp.path + ".draining"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the spool stays")
	}
}

type home struct {
	base string
	self bool
}

func (h *home) HomeEndpoint() (string, string, bool) { return h.base, "tok", h.self }

// flaky fails the first n requests.
type flaky struct {
	mu   sync.Mutex
	n    int
	next http.RoundTripper
	got  []string
}

func (f *flaky) Do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r.Header.Get("Authorization"))
	if f.n > 0 {
		f.n--
		return nil, errors.New("down")
	}
	return f.next.RoundTrip(r)
}

func serve(t *testing.T, s *Store) *httptest.Server {
	mux := http.NewServeMux()
	for _, r := range NewRoutes(s, nil, nil).Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOutboxSendsOnceAfterAFailure(t *testing.T) {
	ctx := context.Background()
	homeStore, member := open(t), open(t)
	srv := serve(t, homeStore)
	client := &flaky{n: 1, next: http.DefaultTransport}
	h := &home{base: srv.URL}
	o := NewOutbox(member, h, client, nil)
	at := draft("hook:1", "turn.ended", "X-1")
	member.Append(ctx, []wire.EventDraft{at, draft("hook:2", "wait.started", "X-1")})
	if p, _ := member.Pending(ctx); p != 2 {
		t.Fatalf("pending %d", p)
	}
	if _, err := o.Flush(ctx); err == nil {
		t.Fatal("no error when the home is down")
	}
	if n, err := o.Flush(ctx); n != 2 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := o.Flush(ctx); n != 0 {
		t.Fatalf("again %d", n)
	}
	// A second send of the same rows is a no-op on the home.
	o.post(ctx, srv.URL, "tok", []wire.Event{{EventDraft: at}})
	page, _ := homeStore.Tail(ctx, 0, 0)
	if ids(page) != "hook:1 hook:2" || !page.Events[0].At.Equal(t0) {
		t.Fatalf("%+v", page)
	}
	if client.got[0] != "Bearer tok" {
		t.Fatalf("%v", client.got)
	}
	// The home itself does not queue.
	h.self = true
	homeStore.QueueWhen(o.Member)
	member.Append(ctx, []wire.EventDraft{draft("hook:3", "turn.ended", "X-1")})
	if p, _ := member.Pending(ctx); p != 0 {
		t.Fatalf("home pending %d", p)
	}
}

func TestRoutes(t *testing.T) {
	s := open(t)
	srv := serve(t, s)
	post := func(path, body string) (int, string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	d, _ := json.Marshal(wire.EventAppend{Events: []wire.EventDraft{draft("app:1", "item.viewed", "x-2")}})
	if code, body := post("/v1/events", string(d)); code != 200 || !strings.Contains(body, `"accepted":1`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := post("/v1/events", `{"events":[{"id":"bad"}]}`); code != 400 {
		t.Fatalf("bad draft %d", code)
	}
	if code, _ := post("/v1/events", `{"events":[],"x":1}`); code != 400 {
		t.Fatalf("unknown field %d", code)
	}
	if code, body := post("/v1/events/links", `{"links":[{"item":"X-2","parent":"X-1"}]}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	resp, _ := http.Get(srv.URL + "/v1/items/X-1/events?tree=1&kinds=item.*")
	var page wire.EventPage
	json.NewDecoder(resp.Body).Decode(&page)
	resp.Body.Close()
	if ids(page) != "app:1" || page.Events[0].ParentItem != "X-1" {
		t.Fatalf("%+v", page)
	}
	if resp, _ := http.Get(srv.URL + "/v1/events/rows?limit=5000"); resp.StatusCode != 400 {
		t.Fatalf("limit %d", resp.StatusCode)
	}
}

func TestTakeHomeCopiesTheRows(t *testing.T) {
	ctx := context.Background()
	old, next := open(t), open(t)
	var drafts []wire.EventDraft
	for i := range 1500 {
		drafts = append(drafts, draft("hook:"+strings.Repeat("x", 1)+string(rune('a'+i%26))+time.Duration(i).String(), "turn.ended", "X-1"))
	}
	old.Append(ctx, drafts)
	srv := serve(t, old)
	next.QueueWhen(func() bool { return true })
	next.Append(ctx, []wire.EventDraft{drafts[0]})
	n, offline, err := NewRoutes(next, nil, nil).TakeHome(ctx, srv.URL, "")
	if err != nil || offline || n != 1499 {
		t.Fatalf("%d %v %v", n, offline, err)
	}
	if p, _ := next.Pending(ctx); p != 1 {
		t.Fatalf("the copy went to the outbox: %d", p)
	}
	srv.Close()
	if _, offline, err := NewRoutes(next, nil, nil).TakeHome(ctx, srv.URL, ""); !offline || err != nil {
		t.Fatalf("%v %v", offline, err)
	}
}

func TestResolver(t *testing.T) {
	s, ctx := open(t), context.Background()
	s.SetLinks(ctx, []wire.ItemLink{{Item: "OVERSIRE-374", Parent: "OVERSIRE-371"}})
	r := NewResolver(s, func(run string) string {
		if run == "r1" {
			return "oversire-374"
		}
		return ""
	})
	for h, want := range map[Hint]string{
		{SessionKey: "work:OVERSIRE-374"}:                            "OVERSIRE-374",
		{SessionKey: "plan:oversire-12"}:                             "OVERSIRE-12",
		{SessionKey: "runbook:r1.build.0"}:                           "OVERSIRE-374",
		{Path: "/Users/x/ws/.wt/oversire-377/cli"}:                   "OVERSIRE-377",
		{Branch: "oversire-381"}:                                     "OVERSIRE-381",
		{Branch: "fritz/towertail-2-fix"}:                            "TOWERTAIL-2",
		{Message: "Fix the build\n\nItem: OVERSIRE-380\n"}:           "OVERSIRE-380",
		{Branch: "main", Path: "/Users/x/repo"}:                      "",
		{SessionKey: "runbook:zz.build.0", Path: "/a/.wt/sling-3/x"}: "SLING-3",
	} {
		if got := r.Item(h); got != want {
			t.Errorf("%+v: %q, want %q", h, got, want)
		}
	}
	var item, parent string
	r.Fill(ctx, &item, &parent, Hint{SessionKey: "work:OVERSIRE-374"})
	if item != "OVERSIRE-374" || parent != "OVERSIRE-371" {
		t.Fatalf("%s %s", item, parent)
	}
}
