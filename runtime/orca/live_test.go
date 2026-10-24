package orca

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/runtime/orca/orcaws"
	"github.com/oversire/cli/runtime/orca/orcaws/orcawstest"
	"github.com/oversire/cli/term"
	"github.com/oversire/cli/wire"
)

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Set(_ context.Context, d, t string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[d] = t
	return nil
}

func (s *memSecrets) Get(_ context.Context, d string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.m[d]; ok {
		return t, nil
	}
	return "", errors.New("no token")
}

func (s *memSecrets) Delete(_ context.Context, d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, d)
}

// fakeTerminal is one Orca terminal behind the fake socket.
type fakeTerminal struct {
	mu      sync.Mutex
	subs    []*orcawstest.Peer
	subIDs  []string
	sends   []map[string]any
	resizes []map[string]any
}

func (f *fakeTerminal) handle(p *orcawstest.Peer, r orcawstest.Request) {
	var params map[string]any
	_ = json.Unmarshal(r.Params, &params)
	if params["terminal"] != "t1" {
		p.Fail(r.ID, "terminal_not_found", "no such terminal")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case "terminal.subscribe":
		f.subs, f.subIDs = append(f.subs, p), append(f.subIDs, r.ID)
		p.Reply(r.ID, map[string]any{"type": "scrollback", "lines": []string{"a"}, "serialized": "\x1b[1mhi\x1b[0m", "cols": 80, "rows": 24}, true)
	case "terminal.send":
		f.sends = append(f.sends, params)
		p.Reply(r.ID, map[string]any{"send": map[string]any{"handle": "t1", "accepted": params["text"] != "locked"}}, false)
	case "terminal.resizeForClient":
		f.resizes = append(f.resizes, params)
		p.Reply(r.ID, map[string]any{"terminal": map[string]any{"handle": "t1"}}, false)
		ev := map[string]any{"type": "fit-override-changed", "mode": "desktop-fit", "cols": 120, "rows": 40}
		if params["mode"] == "mobile-fit" {
			ev = map[string]any{"type": "fit-override-changed", "mode": "mobile-fit", "cols": params["cols"], "rows": params["rows"]}
		}
		for i, s := range f.subs {
			s.Reply(f.subIDs[i], ev, true)
		}
	default:
		p.Fail(r.ID, "method_not_found", r.Method)
	}
}

// emit sends a stream event to each subscriber.
func (f *fakeTerminal) emit(ev map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.subs {
		s.Reply(f.subIDs[i], ev, true)
	}
}

func pairedOrca(t *testing.T) (*Orca, *orcawstest.Server, *fakeTerminal) {
	t.Helper()
	srv := orcawstest.New()
	t.Cleanup(srv.Close)
	ft := &fakeTerminal{}
	srv.Handle = ft.handle
	o := New("orca", nil).WithPairing(NewPairing(&memSecrets{}))
	if err := o.PairStream(context.Background(), srv.Link()); err != nil {
		t.Fatal(err)
	}
	return o, srv, ft
}

func recv(t *testing.T, ch <-chan term.Chunk) term.Chunk {
	t.Helper()
	select {
	case c, ok := <-ch:
		if !ok {
			t.Fatal("the stream closed")
		}
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no chunk")
	}
	return term.Chunk{}
}

func TestLiveStream(t *testing.T) {
	o, _, ft := pairedOrca(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if st := o.StreamState(); st.State != wire.StreamLive || !st.Paired || !st.Pairable {
		t.Fatal(st)
	}
	src, err := o.Terminal(ctx, wire.DaemonSession{ID: "tab:leaf", Terminal: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if c := src.Caps(); c.Source != wire.SourceStream || !c.Writable || !c.Resizable {
		t.Fatal(c)
	}
	snap, chunks, err := src.Attach(ctx, term.Viewport{})
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Data) != "\x1b[1mhi\x1b[0m" || snap.Size != (term.Viewport{Cols: 80, Rows: 24}) {
		t.Fatal(snap)
	}
	ft.emit(map[string]any{"type": "data", "chunk": "out\r\n"})
	if c := recv(t, chunks); string(c.Output) != "out\r\n" {
		t.Fatal(c)
	}

	// Keys: Return and Ctrl+C become flags.
	if err := src.Write(ctx, []byte("ls\r\x03")); err != nil {
		t.Fatal(err)
	}
	ft.mu.Lock()
	sends, _ := json.Marshal(ft.sends)
	ft.mu.Unlock()
	if string(sends) != `[{"enter":true,"terminal":"t1","text":"ls"},{"interrupt":true,"terminal":"t1"}]` {
		t.Fatal(string(sends))
	}
	if err := src.Write(ctx, []byte("locked")); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatal(err)
	}

	// Take control, then Orca takes the size back.
	if err := src.Control(ctx, &term.Viewport{Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	if c := recv(t, chunks); c.Resized == nil || *c.Resized != (term.Viewport{Cols: 100, Rows: 30}) {
		t.Fatal(c)
	}
	ft.emit(map[string]any{"type": "fit-override-changed", "mode": "desktop-fit", "cols": 120, "rows": 40})
	if c := recv(t, chunks); c.Resized == nil || c.Resized.Cols != 120 {
		t.Fatal(c)
	}
	if c := recv(t, chunks); !c.TakeBack {
		t.Fatal("no take back", c)
	}
	// Release gives the size back.
	if err := src.Control(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if c := recv(t, chunks); c.Resized == nil || c.TakeBack {
		t.Fatal(c)
	}
	ft.mu.Lock()
	modes := []any{ft.resizes[0]["mode"], ft.resizes[1]["mode"], ft.resizes[0]["clientId"] == ft.resizes[1]["clientId"]}
	ft.mu.Unlock()
	if modes[0] != "mobile-fit" || modes[1] != "restore" || modes[2] != true {
		t.Fatal(modes)
	}

	ft.emit(map[string]any{"type": "end"})
	if c := recv(t, chunks); c.Ended != "exited" {
		t.Fatal(c)
	}
	select {
	case _, ok := <-chunks:
		if ok {
			t.Fatal("the stream must close after the end")
		}
	case <-time.After(time.Second):
		t.Fatal("the stream did not close")
	}
}

func TestLiveDropEndsView(t *testing.T) {
	o, srv, _ := pairedOrca(t)
	ctx := context.Background()
	src, _ := o.Terminal(ctx, wire.DaemonSession{Terminal: "t1"})
	_, chunks, err := src.Attach(ctx, term.Viewport{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range srv.Peers() {
		p.Drop()
	}
	if c := recv(t, chunks); c.Ended != "closed" {
		t.Fatal(c)
	}
}

func TestLiveGoneTerminal(t *testing.T) {
	o, _, _ := pairedOrca(t)
	src, _ := o.Terminal(context.Background(), wire.DaemonSession{Terminal: "gone"})
	if _, _, err := src.Attach(context.Background(), term.Viewport{}); err == nil || !strings.Contains(err.Error(), "terminal_not_found") {
		t.Fatal(err)
	}
}

func TestPairingStates(t *testing.T) {
	ctx := context.Background()
	srv := orcawstest.New()
	defer srv.Close()
	secrets := &memSecrets{}
	o := New("orca", nil).WithPairing(NewPairing(secrets))
	if st := o.StreamState(); st.State != wire.StreamTextOnly || st.Paired || !st.Pairable || st.Reason != UnpairedReason {
		t.Fatal(st)
	}
	src, _ := o.Terminal(ctx, wire.DaemonSession{Terminal: "t1"})
	if c := src.Caps(); c.Source != wire.SourcePoll || c.Reason != UnpairedReason {
		t.Fatal(c)
	}

	// A bad token or an old Orca does not pair, and keeps nothing.
	bad := srv.Offer()
	bad.DeviceToken = "nope"
	if err := o.PairStream(ctx, bad.Code()); err == nil {
		t.Fatal("a bad token paired")
	}
	srv.Status = orcaws.Status{ProtocolVersion: 4, MinClientVersion: 4}
	if err := o.PairStream(ctx, srv.Link()); err == nil || !strings.Contains(err.Error(), "newer client") {
		t.Fatal(err)
	}
	if len(secrets.m) != 0 {
		t.Fatal(secrets.m)
	}

	srv.Status = orcaws.Status{ProtocolVersion: 3, MinClientVersion: 2}
	if err := o.PairStream(ctx, srv.Link()); err != nil {
		t.Fatal(err)
	}
	if code := secrets.m[pairAccount]; code != srv.Offer().Code() {
		t.Fatal("the keychain must keep the code", code)
	}
	// A new daemon reads the pairing from the keychain.
	o2 := New("orca", nil).WithPairing(NewPairing(secrets))
	if st := o2.StreamState(); st.State != wire.StreamLive {
		t.Fatal(st)
	}

	// Three failed dials in a row: text only, with the reason, until a retry.
	srv.Status = orcaws.Status{ProtocolVersion: 1}
	for range liveFailures {
		src, _ := o2.Terminal(ctx, wire.DaemonSession{Terminal: "t1"})
		if _, _, err := src.Attach(ctx, term.Viewport{}); err == nil {
			t.Fatal("an old Orca must fail")
		}
	}
	st := o2.StreamState()
	if st.State != wire.StreamTextOnly || !st.Paired || !strings.HasPrefix(st.Reason, "Pairing failed: This Orca is too old") {
		t.Fatal(st)
	}
	if src, _ := o2.Terminal(ctx, wire.DaemonSession{Terminal: "t1"}); src.Caps().Source != wire.SourcePoll || !strings.HasPrefix(src.Caps().Reason, "Pairing failed") {
		t.Fatal(src.Caps())
	}
	srv.Status = orcaws.Status{ProtocolVersion: 3, MinClientVersion: 2}
	o2.RetryStream(ctx)
	if st := o2.StreamState(); st.State != wire.StreamLive {
		t.Fatal(st)
	}

	o2.UnpairStream(ctx)
	if st := o2.StreamState(); st.Paired || st.State != wire.StreamTextOnly || len(secrets.m) != 0 {
		t.Fatal(st, secrets.m)
	}
}

func TestNoPairingStore(t *testing.T) {
	o := New("orca", nil)
	if st := o.StreamState(); st.State != wire.StreamTextOnly || st.Pairable || st.Reason != PollReason {
		t.Fatal(st)
	}
	if err := o.PairStream(context.Background(), "x"); err == nil {
		t.Fatal("pair with no store")
	}
}
