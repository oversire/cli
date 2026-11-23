package link

import (
	"context"
	"errors"
	"testing"

	"github.com/oversire/cli/server"
)

type fakeSelf struct {
	p   server.Peer
	err error
}

func (f fakeSelf) Self(context.Context) (server.Peer, error) { return f.p, f.err }

func TestWebBase(t *testing.T) {
	host := func() (string, error) { return "MBP.local", nil }
	w := NewWebBase(fakeSelf{p: server.Peer{DNS: "mbp.tail8ed2.ts.net"}}, 7690, true, host)
	if w.Get() != "http://mbp:7690" {
		t.Fatalf("before the first read: %s", w.Get())
	}
	w.refresh(context.Background())
	if w.Get() != "http://mbp.tail8ed2.ts.net:7690" {
		t.Fatalf("tailnet: %s", w.Get())
	}
	w = NewWebBase(fakeSelf{err: errors.New("down")}, 7691, true, host)
	if w.refresh(context.Background()); w.Get() != "http://mbp:7691" {
		t.Fatalf("down: %s", w.Get())
	}
	w = NewWebBase(fakeSelf{p: server.Peer{DNS: "mbp.tail8ed2.ts.net"}}, 7690, false, host)
	if w.refresh(context.Background()); w.Get() != "http://127.0.0.1:7690" {
		t.Fatalf("no tailnet: %s", w.Get())
	}
}
