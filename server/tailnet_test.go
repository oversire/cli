package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
)

type fakeRunner map[string]string // args -> stdout; a missing entry fails

func (f fakeRunner) Run(_ context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
	out, ok := f[strings.Join(args, " ")]
	if !ok {
		return nil, []byte("not running"), errors.New("exit status 1")
	}
	return []byte(out), nil, nil
}

func ifaces(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				panic(err)
			}
			n.IP = ip
			out = append(out, n)
		}
		return out, nil
	}
}

func TestTailnetAddrs(t *testing.T) {
	lan := ifaces("192.168.1.20/24", "100.101.1.2/32", "fd7a:115c:a1e0::9/128", "fe80::1/64", "127.0.0.1/8")
	cases := []struct {
		name string
		t    Tailnet
		want string
	}{
		{"cli", Tailnet{exe: "tailscale", run: fakeRunner{"ip": "100.97.81.23\nfd7a:115c:a1e0:ab12::5117\n"}, ifaces: lan},
			"[100.97.81.23 fd7a:115c:a1e0:ab12::5117]"},
		{"cli drops other addresses", Tailnet{exe: "tailscale", run: fakeRunner{"ip": "10.0.0.1\n100.64.0.1\n"}, ifaces: lan},
			"[100.64.0.1]"},
		{"cli fails", Tailnet{exe: "tailscale", run: fakeRunner{}, ifaces: lan}, "[100.101.1.2 fd7a:115c:a1e0::9]"},
		{"no cli", Tailnet{ifaces: lan}, "[100.101.1.2 fd7a:115c:a1e0::9]"},
		{"no tailnet", Tailnet{ifaces: ifaces("192.168.1.20/24")}, "[]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.t.Addrs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != c.want {
				t.Fatalf("got %v, want %s", got, c.want)
			}
		})
	}
}

const statusJSON = `{
  "BackendState": "Running",
  "Self": {"HostName": "mbp", "DNSName": "mbp.tail8ed2.ts.net.", "TailscaleIPs": ["100.97.81.23"]},
  "Peer": {
    "nodekey:b": {"HostName": "mba", "DNSName": "mba.tail8ed2.ts.net.", "TailscaleIPs": ["100.70.1.2", "fd7a:115c:a1e0::2"], "Online": true, "OS": "macOS"},
    "nodekey:a": {"HostName": "box", "DNSName": "box.tail8ed2.ts.net.", "TailscaleIPs": ["100.70.1.3"], "Online": false, "OS": "linux"}
  }
}`

func TestTailnetPeers(t *testing.T) {
	tn := Tailnet{exe: "tailscale", run: fakeRunner{"status --json": statusJSON}}
	peers, err := tn.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Peer{
		{Name: "box", DNS: "box.tail8ed2.ts.net", Addrs: []netip.Addr{netip.MustParseAddr("100.70.1.3")}, OS: "linux"},
		{Name: "mba", DNS: "mba.tail8ed2.ts.net", Addrs: []netip.Addr{netip.MustParseAddr("100.70.1.2"), netip.MustParseAddr("fd7a:115c:a1e0::2")}, Online: true, OS: "macOS"},
	}
	if fmt.Sprint(peers) != fmt.Sprint(want) {
		t.Fatalf("got %+v", peers)
	}
	if _, err := (&Tailnet{}).Peers(context.Background()); err == nil {
		t.Fatal("no error without the CLI")
	}
	if _, err := (&Tailnet{exe: "tailscale", run: fakeRunner{"status --json": "{"}}).Peers(context.Background()); err == nil {
		t.Fatal("no error on bad JSON")
	}
}
