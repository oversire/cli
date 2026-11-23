package server

import (
	"cmp"
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
	"time"
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

// ErrTailscaleDown is a Tailscale CLI that gives text in place of JSON, for example "The Tailscale GUI failed to start".
var ErrTailscaleDown = errors.New("Tailscale does not answer")

// Tailnet finds the tailnet addresses and peers of this machine.
type Tailnet struct {
	run    Runner
	exe    string                     // empty when the CLI is not on the machine
	ifaces func() ([]net.Addr, error) // net.InterfaceAddrs
	retry  time.Duration              // the wait before the second try of a status that is not JSON
}

func NewTailnet(run Runner) *Tailnet {
	t := &Tailnet{run: run, ifaces: net.InterfaceAddrs, retry: time.Second}
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

func jsonObject(b []byte) bool { return strings.HasPrefix(strings.TrimSpace(string(b)), "{") }

func inTailnet(a netip.Addr) bool {
	return slices.ContainsFunc(tailnetRanges, func(p netip.Prefix) bool { return p.Contains(a) })
}

type statusNode struct {
	HostName     string
	DNSName      string
	TailscaleIPs []string
	Online       bool
	OS           string
}

func (n statusNode) peer() Peer {
	p := Peer{Name: n.HostName, DNS: strings.TrimSuffix(n.DNSName, "."), Online: n.Online, OS: n.OS}
	for _, s := range n.TailscaleIPs {
		if a, err := netip.ParseAddr(s); err == nil {
			p.Addrs = append(p.Addrs, a)
		}
	}
	return p
}

type status struct {
	Self *statusNode
	Peer map[string]statusNode
}

// status runs `tailscale status --json`.
// The app CLI can print text and exit 0 while the app starts. Then it tries again one time, and gives ErrTailscaleDown.
func (t *Tailnet) status(ctx context.Context) (status, error) {
	var st status
	if t.exe == "" {
		return st, errors.New("the tailscale CLI is not on this machine")
	}
	out, _, err := t.run.Run(ctx, t.exe, []string{"status", "--json"}, nil)
	if err == nil && !jsonObject(out) {
		select {
		case <-time.After(t.retry):
		case <-ctx.Done():
			return st, ctx.Err()
		}
		out, _, err = t.run.Run(ctx, t.exe, []string{"status", "--json"}, nil)
	}
	if err != nil {
		return st, fmt.Errorf("tailscale status: %w", err)
	}
	if !jsonObject(out) {
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return st, fmt.Errorf("%w: %s", ErrTailscaleDown, cmp.Or(line, "no output"))
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return st, fmt.Errorf("tailscale status: %w", err)
	}
	return st, nil
}

// Peers gives the other nodes of `tailscale status --json`, sorted by name.
func (t *Tailnet) Peers(ctx context.Context) ([]Peer, error) {
	st, err := t.status(ctx)
	if err != nil {
		return nil, err
	}
	peers := make([]Peer, 0, len(st.Peer))
	for _, p := range st.Peer {
		peers = append(peers, p.peer())
	}
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Name, b.Name) })
	return peers, nil
}

// Self gives this node of `tailscale status --json`.
func (t *Tailnet) Self(ctx context.Context) (Peer, error) {
	st, err := t.status(ctx)
	if err != nil {
		return Peer{}, err
	}
	if st.Self == nil {
		return Peer{}, errors.New("tailscale status: no Self")
	}
	return st.Self.peer(), nil
}
