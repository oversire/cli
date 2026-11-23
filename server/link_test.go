package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"testing"

	"github.com/oversire/cli/wire"
)

func TestCallerAddrOnTailnet(t *testing.T) {
	f := newFixture(t, true)
	f.s.Handle(Route{Pattern: "GET /v1/test-addr", Access: Public, Handle: func(w http.ResponseWriter, r *http.Request) {
		c, _ := CallerFrom(r.Context())
		WriteJSON(w, http.StatusOK, map[string]string{"addr": c.Addr.String()})
	}})
	_, b := call(t, "GET", f.net.URL+"/v1/test-addr", "", "Authorization", "Bearer good")
	if string(b) != `{"addr":"127.0.0.1"}`+"\n" {
		t.Fatalf("with a credential: %s", b)
	}
	// With no credential there is no caller, so no address.
	if _, b = call(t, "GET", f.net.URL+"/v1/test-addr", ""); string(b) != `{"addr":"invalid IP"}`+"\n" {
		t.Fatalf("no credential: %s", b)
	}
}

func TestIsOwnAddr(t *testing.T) {
	f := newFixture(t, true)
	own := netip.MustParseAddr("100.97.81.23")
	f.s.mu.Lock()
	f.s.nets[own] = nil
	f.s.mu.Unlock()
	for a, want := range map[string]bool{"127.0.0.1": true, "::1": true, "::ffff:100.97.81.23": true, "100.97.81.23": true, "100.70.1.2": false} {
		if got := f.s.IsOwnAddr(netip.MustParseAddr(a)); got != want {
			t.Errorf("%s: got %v", a, got)
		}
	}
}

func TestWhoamiListenWeb(t *testing.T) {
	f := newFixture(t, true)
	_, b := call(t, "GET", f.loop.URL+"/v1/whoami", "")
	var d wire.DaemonInfo
	if err := json.Unmarshal(b, &d); err != nil || d.Listen.Web != "" {
		t.Fatalf("no web: %s", b)
	}
	f.s.Web = func() string { return "http://mbp.tail8ed2.ts.net:7690" }
	_, b = call(t, "GET", f.loop.URL+"/v1/whoami", "")
	if err := json.Unmarshal(b, &d); err != nil || d.Listen.Web != "http://mbp.tail8ed2.ts.net:7690" {
		t.Fatalf("web: %s", b)
	}
}

func TestTailnetSelf(t *testing.T) {
	tn := Tailnet{exe: "tailscale", run: fakeRunner{"status --json": statusJSON}}
	p, err := tn.Self(context.Background())
	if err != nil || p.Name != "mbp" || p.DNS != "mbp.tail8ed2.ts.net" || len(p.Addrs) != 1 || p.Addrs[0] != netip.MustParseAddr("100.97.81.23") {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := (&Tailnet{}).Self(context.Background()); err == nil {
		t.Fatal("no error without the CLI")
	}
}
