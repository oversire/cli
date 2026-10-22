package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// Runner runs a child process (proc.Local).
type Runner interface {
	Run(ctx context.Context, exe string, args []string, stdin []byte) (stdout, stderr []byte, err error)
}

// macTailscale is the CLI in the Tailscale app on macOS. The app does not put `tailscale` on the PATH.
const macTailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

var tailnetRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// Tailnet finds the tailnet addresses and peers of this machine.
type Tailnet struct {
	run    Runner
	exe    string                     // empty when the CLI is not on the machine
	ifaces func() ([]net.Addr, error) // net.InterfaceAddrs
}

func NewTailnet(run Runner) *Tailnet {
	t := &Tailnet{run: run, ifaces: net.InterfaceAddrs}
	if exe, err := exec.LookPath("tailscale"); err == nil {
		t.exe = exe
	} else if _, err := os.Stat(macTailscale); err == nil {
		t.exe = macTailscale
	}
	return t
}

// Addrs gives the addresses of `tailscale ip`. Without the CLI or its addresses, it gives the interface addresses in the tailnet ranges.
// The app CLI can print "The Tailscale GUI failed to start" and exit 0, so an empty answer uses the interfaces too.
// `tailscale ip` prints IPv4 and IPv6; the flags -4 and -6 do not go together.
func (t *Tailnet) Addrs(ctx context.Context) ([]netip.Addr, error) {
	if t.exe != "" {
		out, _, err := t.run.Run(ctx, t.exe, []string{"ip"}, nil)
		if err == nil {
			var addrs []netip.Addr
			for _, f := range strings.Fields(string(out)) {
				if a, err := netip.ParseAddr(f); err == nil && inTailnet(a) {
					addrs = append(addrs, a)
				}
			}
			if len(addrs) > 0 {
				return addrs, nil
			}
		}
	}
	ifs, err := t.ifaces()
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	for _, ia := range ifs {
		p, err := netip.ParsePrefix(ia.String())
		if err != nil {
			continue
		}
		if a := p.Addr().Unmap(); inTailnet(a) && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	return addrs, nil
}

func inTailnet(a netip.Addr) bool {
	return slices.ContainsFunc(tailnetRanges, func(p netip.Prefix) bool { return p.Contains(a) })
}

// Peers gives the other nodes of `tailscale status --json`, sorted by name.
func (t *Tailnet) Peers(ctx context.Context) ([]Peer, error) {
	if t.exe == "" {
		return nil, errors.New("the tailscale CLI is not on this machine")
	}
	out, _, err := t.run.Run(ctx, t.exe, []string{"status", "--json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st struct {
		Peer map[string]struct {
			HostName     string
			DNSName      string
			TailscaleIPs []string
			Online       bool
			OS           string
		}
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	peers := make([]Peer, 0, len(st.Peer))
	for _, p := range st.Peer {
		peer := Peer{Name: p.HostName, DNS: strings.TrimSuffix(p.DNSName, "."), Online: p.Online, OS: p.OS}
		for _, s := range p.TailscaleIPs {
			if a, err := netip.ParseAddr(s); err == nil {
				peer.Addrs = append(peer.Addrs, a)
			}
		}
		peers = append(peers, peer)
	}
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return peers, nil
}
