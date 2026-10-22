package group

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// fakeTaker records the calls of POST /v1/group/home.
type fakeTaker struct {
	base, token string
	records     int
	offline     bool
	err         error
}

func (f *fakeTaker) TakeHome(_ context.Context, base, token string) (int, bool, error) {
	f.base, f.token = base, token
	return f.records, f.offline, f.err
}

func TestMoveHomeTakesTheStore(t *testing.T) {
	clock := newClock()
	a, b := newNode(t, "a", clock), newNode(t, "b", clock)
	var inv wire.GroupInvite
	a.local(t, http.MethodPost, "/v1/group/invites", map[string]any{}, &inv)
	clock.Add(time.Second)
	b.local(t, http.MethodPost, "/v1/group/enter", map[string]string{"host": inv.Via, "code": inv.Code}, nil)
	if b.group(t).Home != "a" {
		t.Fatal(b.group(t).Home)
	}

	taker := &fakeTaker{err: errors.New("boom")}
	b.syncer.WithHomeTaker(taker)
	code, err := (peerClient{http.DefaultClient}).do(t.Context(), http.MethodPost, b.loop.URL+"/v1/group/home", "", map[string]string{"machine": "b"}, nil)
	if code != http.StatusBadGateway || err == nil || b.group(t).Home != "a" {
		t.Fatalf("a failed copy must stop the move: %d %v %s", code, err, b.group(t).Home)
	}

	*taker = fakeTaker{records: 3}
	var out homeMove
	b.local(t, http.MethodPost, "/v1/group/home", map[string]string{"machine": "b"}, &out)
	if out != (homeMove{OK: true, Records: 3}) || b.group(t).Home != "b" {
		t.Fatalf("%+v %s", out, b.group(t).Home)
	}
	if taker.base != "http://"+a.addr() || taker.token == "" {
		t.Fatalf("%+v", taker)
	}

	// A move to another machine does not copy here: the new home copies.
	*taker = fakeTaker{}
	b.local(t, http.MethodPost, "/v1/group/home", map[string]string{"machine": "a"}, &out)
	if taker.base != "" || b.group(t).Home != "a" {
		t.Fatalf("%+v", taker)
	}

	// An old home that does not answer: the move goes on, and the answer tells it.
	*taker = fakeTaker{offline: true}
	b.local(t, http.MethodPost, "/v1/group/home", map[string]string{"machine": "b"}, &out)
	if !out.Offline || b.group(t).Home != "b" {
		t.Fatalf("%+v", out)
	}
}
