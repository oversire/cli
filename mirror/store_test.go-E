package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newStore(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	s, err := Open(filepath.Join(t.TempDir(), "mirror.db"), clock.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock
}

func doc(kind wire.MirrorKind, id, body string) wire.MirrorDoc {
	return wire.MirrorDoc{Kind: kind, ID: id, V: 1, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Body: json.RawMessage(body)}
}

func tomb(kind wire.MirrorKind, id string) wire.MirrorDoc {
	return wire.MirrorDoc{Kind: kind, ID: id, V: 1, At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), Deleted: true}
}

func put(s *Store, docs ...wire.MirrorDoc) wire.MirrorPut {
	return wire.MirrorPut{Epoch: s.Head().Epoch, Source: "app-1", SourceName: "mbp", AppVersion: "1.0", Docs: docs}
}

func TestPutSameHashIsNoop(t *testing.T) {
	s, _ := newStore(t)
	d := doc(wire.MirrorItem, "p/A-1", `{"a": 1}`)
	res, err := s.Put(put(s, d), Source{})
	if err != nil || res.Applied != 1 || res.Same != 0 || res.Head.Head != 1 {
		t.Fatalf("first: %+v %v", res, err)
	}
	// The whitespace of the body does not change the hash.
	d.Body = json.RawMessage(`{"a":1}`)
	res, err = s.Put(put(s, d), Source{})
	if err != nil || res.Applied != 0 || res.Same != 1 || res.Head.Head != 1 {
		t.Fatalf("second: %+v %v", res, err)
	}
	d.V = 2
	if res, _ = s.Put(put(s, d), Source{}); res.Applied != 1 || res.Head.Head != 2 {
		t.Fatalf("a new v: %+v", res)
	}
	d.Body = json.RawMessage(`{"a":2}`)
	if res, _ = s.Put(put(s, d), Source{}); res.Applied != 1 || res.Head.Head != 3 {
		t.Fatalf("a new body: %+v", res)
	}
	idx := s.Index()
	if len(idx.Docs) != 1 || len(idx.Docs[0].Hash) != 64 {
		t.Fatalf("index: %+v", idx)
	}
}

func TestPutRecordsTheSource(t *testing.T) {
	s, _ := newStore(t)
	if _, ok := s.Source(); ok || s.Head().Ready || s.Head().PublishedAt != nil {
		t.Fatal("a source before the first put")
	}
	p := put(s)
	p.Ready = true
	res, err := s.Put(p, Source{Device: "D1", Name: "OverSire on mbp", Machine: "mbp"})
	if err != nil || res.Applied != 0 || !res.Head.Ready || res.Head.Source != "app-1" || res.Head.SourceName != "mbp" ||
		res.Head.SourceMachine != "mbp" || res.Head.AppVersion != "1.0" || res.Head.PublishedAt == nil {
		t.Fatalf("%+v %v", res, err)
	}
	src, ok := s.Source()
	if !ok || src != (Source{App: "app-1", Device: "D1", Name: "OverSire on mbp", Machine: "mbp"}) {
		t.Fatalf("%+v", src)
	}
	// Ready stays for the same source, and goes for a new source.
	p.Ready = false
	if res, _ = s.Put(p, Source{}); !res.Head.Ready {
		t.Fatal("ready went away")
	}
	p.Source = "app-2"
	if res, _ = s.Put(p, Source{}); res.Head.Ready {
		t.Fatal("a new source is ready before it says so")
	}
}

func TestPutEpoch(t *testing.T) {
	s, _ := newStore(t)
	p := put(s, doc(wire.MirrorItem, "p/A-1", `{}`))
	p.Epoch = "other"
	if _, err := s.Put(p, Source{}); !errors.Is(err, ErrEpoch) {
		t.Fatal(err)
	}
	if s.Head().Head != 0 {
		t.Fatal("a refused put wrote")
	}
}

func TestPageAfterCursor(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{"n":1}`), doc(wire.MirrorItem, "p/A-2", `{"n":2}`), doc(wire.MirrorRuns, "o/r", `{"n":3}`)), Source{}); err != nil {
		t.Fatal(err)
	}
	epoch := s.Head().Epoch
	p, err := s.Page(1, 500, epoch)
	if err != nil || p.Reset || p.More || len(p.Docs) != 2 || p.Docs[0].Seq != 2 || p.Docs[1].Seq != 3 || p.Next != 3 || p.Epoch != epoch {
		t.Fatalf("%+v %v", p, err)
	}
	if string(p.Docs[1].Body) != `{"n":3}` || p.Docs[1].Hash == "" {
		t.Fatalf("%+v", p.Docs[1])
	}
	p, _ = s.Page(0, 2, "")
	if p.Reset || !p.More || len(p.Docs) != 2 || p.Next != 2 {
		t.Fatalf("a first read in pages: %+v", p)
	}
	p, _ = s.Page(p.Next, 2, epoch)
	if p.Reset || p.More || len(p.Docs) != 1 || p.Next != 3 {
		t.Fatalf("the last page: %+v", p)
	}
	p, _ = s.Page(3, 500, epoch)
	if p.Reset || p.More || len(p.Docs) != 0 || p.Next != 3 {
		t.Fatalf("no news: %+v", p)
	}
}

func TestPageStopsAfterManyBytes(t *testing.T) {
	s, _ := newStore(t)
	big := `{"x":"` + strings.Repeat("a", MaxDocBytes-100) + `"}`
	var docs []wire.MirrorDoc
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		docs = append(docs, doc(wire.MirrorProject, id, big))
	}
	for _, chunk := range [][]wire.MirrorDoc{docs[:5], docs[5:]} {
		if _, err := s.Put(put(s, chunk...), Source{}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Page(0, 500, "")
	if err != nil || !p.More || len(p.Docs) != 8 || p.Next != 8 {
		t.Fatalf("%d docs, more %v, next %d, %v", len(p.Docs), p.More, p.Next, err)
	}
}

func TestPageResetBehindFloor(t *testing.T) {
	s, clock := newStore(t)
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{}`), doc(wire.MirrorItem, "p/A-2", `{}`)), Source{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(put(s, tomb(wire.MirrorItem, "p/A-1")), Source{}); err != nil {
		t.Fatal(err)
	}
	epoch := s.Head().Epoch
	clock.add(6 * 24 * time.Hour)
	if err := s.Prune(clock.now()); err != nil || s.Head().Floor != 0 {
		t.Fatalf("a young tombstone: %v floor %d", err, s.Head().Floor)
	}
	clock.add(2 * 24 * time.Hour)
	if err := s.Prune(clock.now()); err != nil {
		t.Fatal(err)
	}
	if h := s.Head(); h.Floor != 3 || h.Head != 3 {
		t.Fatalf("%+v", h)
	}
	// A reader that holds seq 1 missed the tombstone.
	p, _ := s.Page(1, 500, epoch)
	if !p.Reset || len(p.Docs) != 0 || p.Next != 0 {
		t.Fatalf("%+v", p)
	}
	// A reader that starts at 0 has no old docs, so it needs no reset. It gets the live docs.
	p, _ = s.Page(0, 500, epoch)
	if p.Reset || len(p.Docs) != 1 || p.Docs[0].ID != "p/A-2" {
		t.Fatalf("%+v", p)
	}
	if p, _ = s.Page(3, 500, epoch); p.Reset {
		t.Fatal("a reader at the floor continues")
	}
}

func TestPageResetOnOtherEpoch(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{}`)), Source{}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Page(0, 500, "other")
	if !p.Reset || len(p.Docs) != 0 || p.Epoch != s.Head().Epoch {
		t.Fatalf("%+v", p)
	}
	if p, _ = s.Page(1, 500, ""); !p.Reset {
		t.Fatal("a cursor with no epoch")
	}
	if p, _ = s.Page(9, 500, s.Head().Epoch); !p.Reset {
		t.Fatal("a cursor above the head")
	}
}

func TestPutLimits(t *testing.T) {
	s, _ := newStore(t)
	var docs []wire.MirrorDoc
	for i := 0; i < MaxDocs+1; i++ {
		docs = append(docs, doc(wire.MirrorItem, "p/"+string(rune('A'+i%26))+strings.Repeat("x", i/26), `{}`))
	}
	if _, err := s.Put(put(s, docs[:MaxDocs]...), Source{}); err != nil {
		t.Fatalf("500 docs: %v", err)
	}
	if _, err := s.Put(put(s, docs...), Source{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("501 docs: %v", err)
	}
	pad := func(n int) string { return `{"x":"` + strings.Repeat("a", n-8) + `"}` }
	if _, err := s.Put(put(s, doc(wire.MirrorProject, "ok", pad(MaxDocBytes))), Source{}); err != nil {
		t.Fatalf("1 MiB: %v", err)
	}
	if _, err := s.Put(put(s, doc(wire.MirrorProject, "big", pad(MaxDocBytes+1))), Source{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("1 MiB + 1: %v", err)
	}
	var many []wire.MirrorDoc
	for i := 0; i < 9; i++ {
		many = append(many, doc(wire.MirrorProject, string(rune('a'+i)), pad(MaxDocBytes)))
	}
	if _, err := s.Put(put(s, many...), Source{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("9 MiB: %v", err)
	}
}

func TestPutBadDoc(t *testing.T) {
	s, _ := newStore(t)
	for name, d := range map[string]wire.MirrorDoc{
		"kind":       doc("nope", "x", `{}`),
		"id":         doc(wire.MirrorItem, "", `{}`),
		"v":          {Kind: wire.MirrorItem, ID: "x", Body: json.RawMessage(`{}`)},
		"no body":    {Kind: wire.MirrorItem, ID: "x", V: 1},
		"array body": doc(wire.MirrorItem, "x", `[1]`),
		"tombstone":  {Kind: wire.MirrorItem, ID: "x", V: 1, Deleted: true, Body: json.RawMessage(`{}`)},
	} {
		if _, err := s.Put(put(s, d), Source{}); !errors.Is(err, ErrBadDoc) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// One bad doc rejects the batch.
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "ok", `{}`), doc("nope", "x", `{}`)), Source{}); !errors.Is(err, ErrBadDoc) || s.Head().Head != 0 {
		t.Fatalf("a batch with a bad doc: %v", err)
	}
}

func TestDeleteMakesATombstone(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(put(s, tomb(wire.MirrorItem, "p/none")), Source{}); err != nil || s.Head().Head != 0 {
		t.Fatalf("a delete of a doc that is not there: %v", err)
	}
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{}`)), Source{}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Put(put(s, tomb(wire.MirrorItem, "p/A-1")), Source{})
	if res.Applied != 1 {
		t.Fatalf("%+v", res)
	}
	if res, _ = s.Put(put(s, tomb(wire.MirrorItem, "p/A-1")), Source{}); res.Same != 1 {
		t.Fatalf("a second delete: %+v", res)
	}
	p, _ := s.Page(1, 500, s.Head().Epoch)
	if len(p.Docs) != 1 || !p.Docs[0].Deleted || p.Docs[0].Body != nil || p.Docs[0].Hash != "" {
		t.Fatalf("%+v", p)
	}
	if len(s.Index().Docs) != 0 {
		t.Fatal("a tombstone is in the index")
	}
	// The doc comes back with a new seq.
	if res, _ = s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{}`)), Source{}); res.Applied != 1 || res.Head.Head != 3 {
		t.Fatalf("%+v", res)
	}
}

func TestTakeHomeResets(t *testing.T) {
	s, _ := newStore(t)
	p := put(s, doc(wire.MirrorItem, "p/A-1", `{}`))
	p.Ready = true
	if _, err := s.Put(p, Source{Device: "D1"}); err != nil {
		t.Fatal(err)
	}
	s.Seen(wire.MirrorReader{Machine: "mba", Head: 1})
	old := s.Head().Epoch
	n, offline, err := s.TakeHome(context.Background(), "http://old", "token")
	if n != 0 || offline || err != nil {
		t.Fatalf("%d %v %v", n, offline, err)
	}
	h := s.Head()
	if h.Epoch == old || h.Head != 0 || h.Floor != 0 || h.Ready || h.Source != "" || len(h.Readers) != 0 {
		t.Fatalf("%+v", h)
	}
	if _, ok := s.Source(); ok {
		t.Fatal("a source after a reset")
	}
	if len(s.Index().Docs) != 0 {
		t.Fatal("docs after a reset")
	}
	// The old epoch does not fit any more.
	if _, err := s.Put(p, Source{}); !errors.Is(err, ErrEpoch) {
		t.Fatal(err)
	}
}

func TestReadersExpire(t *testing.T) {
	s, clock := newStore(t)
	s.Seen(wire.MirrorReader{Machine: "mba", Name: "a", Head: 5})
	s.Seen(wire.MirrorReader{Machine: "mba", Name: "a2", Head: -1})
	r := s.Head().Readers
	if len(r) != 1 || r[0].Head != 5 || r[0].Name != "a2" {
		t.Fatalf("a negative head keeps the head: %+v", r)
	}
	clock.add(ReaderTTL + time.Hour)
	if len(s.Head().Readers) != 0 {
		t.Fatal("an old reader is in the head")
	}
	if err := s.Prune(clock.now()); err != nil {
		t.Fatal(err)
	}
}

func TestStateSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(put(s, doc(wire.MirrorItem, "p/A-1", `{}`)), Source{}); err != nil {
		t.Fatal(err)
	}
	h := s.Head()
	s.Close()
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if g := s.Head(); g.Epoch != h.Epoch || g.Head != 1 {
		t.Fatalf("%+v", g)
	}
}
