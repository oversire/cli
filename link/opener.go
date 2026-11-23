package link

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Sockets are the WebSocket clients of the daemon (server.Server).
type Sockets interface {
	DeliverTo(match func(server.Caller) bool, msg any) int
	HasCaller(match func(server.Caller) bool) bool
	IsOwnAddr(a netip.Addr) bool
}

// Peers gives the other tailnet nodes (server.Tailnet).
type Peers interface {
	Peers(ctx context.Context) ([]server.Peer, error)
}

// Group gives the roster of this machine (group.Store).
type Group interface {
	Snapshot() (wire.Group, bool)
	Self() wire.GroupMember
}

// Events writes the rows of the daemon (capture.Writer).
type Events interface {
	AddFor(s wire.DaemonSession, d wire.EventDraft)
}

var (
	ErrNoApp          = errors.New("No OverSire app is open.")
	ErrUnknownMachine = errors.New("No machine of the group has this name.")
)

// NotRunningError is a Mac of the group whose app is not connected.
type NotRunningError struct{ Machine string }

func (e *NotRunningError) Error() string { return "OverSire on " + e.Machine + " is not running." }

// NeedsUpdateError is an app that is connected but sends no presence: it is too old to open links.
type NeedsUpdateError struct{ Machine string }

func (e *NeedsUpdateError) Error() string { return "Update OverSire on " + e.Machine + "." }

const peersKeep = time.Minute

// Opener sends a route to the app of the Mac of the click.
type Opener struct {
	Log *slog.Logger

	sockets Sockets
	book    *Book
	peers   Peers
	group   Group
	events  Events
	now     func() time.Time

	mu       sync.Mutex
	peerList []server.Peer
	peersAt  time.Time
	gate     gate
}

func NewOpener(s Sockets, b *Book, p Peers, g Group, ev Events, now func() time.Time) *Opener {
	return &Opener{Log: slog.Default(), sockets: s, book: b, peers: p, group: g, events: ev, now: now, gate: newGate()}
}

// Open picks the app and sends it the route. click is the remote address of a web click (zero for a command).
func (o *Opener) Open(ctx context.Context, p Path, click netip.Addr, on string, src wire.OpenSource, agent string) (wire.OpenResult, error) {
	g, _ := o.group.Snapshot()
	self := o.group.Self().Machine
	live := slices.DeleteFunc(o.book.All(), func(pr Presence) bool { return !o.sockets.HasCaller(pr.Match) })
	target, how, err := o.pick(ctx, g, self, live, click, on)
	if err != nil {
		return wire.OpenResult{}, err
	}
	id := newID()
	frame := wire.OpenRouteFrame{Type: wire.FrameOpenRoute,
		Open: wire.OpenRoute{ID: id, Path: p.Route, Focus: p.Focus, Source: src, Daemon: self, Agent: agent}}
	if o.sockets.DeliverTo(target.Match, frame) == 0 {
		return wire.OpenResult{}, &NotRunningError{target.Machine}
	}
	idle := max(0, int(o.now().Sub(target.InputAt).Seconds()))
	o.Log.Info("link open", "id", id, "route", p.Route, "machine", target.Machine, "how", how, "source", src)
	o.write(id, p, target.Machine, how, src, agent)
	return wire.OpenResult{ID: id, Machine: target.Machine, How: how, Idle: &idle, Group: g.Name}, nil
}

func (o *Opener) pick(ctx context.Context, g wire.Group, self string, live []Presence, click netip.Addr, on string) (Presence, wire.OpenHow, error) {
	if on != "" {
		if !knows(g, self, on) {
			return Presence{}, "", ErrUnknownMachine
		}
		for _, pr := range live {
			if strings.EqualFold(pr.Machine, on) {
				return pr, wire.OpenOn, nil
			}
		}
		if o.sockets.HasCaller(func(c server.Caller) bool { return !c.IsDaemon() && strings.EqualFold(c.Machine, on) }) {
			return Presence{}, "", &NeedsUpdateError{on}
		}
		return Presence{}, "", &NotRunningError{on}
	}
	if click.IsValid() && !o.sockets.IsOwnAddr(click) {
		click = click.Unmap()
		addrs := []netip.Addr{click}
		var peer server.Peer
		for _, p := range o.peerNodes(ctx) {
			if slices.Contains(p.Addrs, click) {
				peer = p
				addrs = append(addrs, p.Addrs...)
			}
		}
		for _, pr := range live {
			if pr.Addr.IsValid() && slices.Contains(addrs, pr.Addr.Unmap()) {
				return pr, wire.OpenByClick, nil
			}
		}
		if peer.Name != "" {
			for _, pr := range live {
				if strings.EqualFold(pr.Machine, peer.Name) {
					return pr, wire.OpenByClick, nil
				}
			}
			if hasApp(g, peer.Name) {
				return Presence{}, "", &NotRunningError{peer.Name}
			}
		}
	}
	if len(live) == 0 {
		return Presence{}, "", ErrNoApp
	}
	return live[0], wire.OpenByInput, nil
}

// peerNodes gives the tailnet peers, kept for a minute.
func (o *Opener) peerNodes(ctx context.Context) []server.Peer {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.peerList != nil && o.now().Sub(o.peersAt) < peersKeep {
		return o.peerList
	}
	ps, err := o.peers.Peers(ctx)
	if err != nil {
		o.Log.Debug("link: tailnet peers", "err", err)
		return o.peerList
	}
	o.peerList, o.peersAt = ps, o.now()
	return ps
}

func (o *Opener) write(id string, p Path, machine string, how wire.OpenHow, src wire.OpenSource, agent string) {
	if o.events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"route": p.Route, "opened": machine, "source": src, "how": how})
	d := wire.EventDraft{ID: "link:" + id, At: o.now(), Kind: wire.KindLinkOpened, Item: p.Item(), ActorType: wire.ActorHuman,
		Via: string(src), Summary: fmt.Sprintf("Opened %s on %s", p.Route, machine), Payload: payload}
	if how == wire.OpenByClick {
		d.Machine = machine
	}
	if agent != "" {
		d.ActorType, d.Actor = wire.ActorAgent, agent
	}
	o.events.AddFor(wire.DaemonSession{}, d)
}

func knows(g wire.Group, self, machine string) bool {
	if strings.EqualFold(self, machine) {
		return true
	}
	return slices.ContainsFunc(g.Members, func(m wire.GroupMember) bool { return strings.EqualFold(m.Machine, machine) }) ||
		slices.ContainsFunc(g.Devices, func(d wire.GroupDevice) bool { return strings.EqualFold(d.Machine, machine) })
}

func hasApp(g wire.Group, machine string) bool {
	return slices.ContainsFunc(g.Devices, func(d wire.GroupDevice) bool {
		return d.Kind == wire.DeviceApp && strings.EqualFold(d.Machine, machine)
	})
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "o-" + hex.EncodeToString(b)
}

// Routes gives /go/ (Public), POST /v1/presence (an app) and POST /v1/open (loopback).
func (o *Opener) Routes() []server.Route {
	return []server.Route{
		{Pattern: "GET /go/{path...}", Access: server.Public, Handle: o.serveGo},
		{Pattern: "POST /v1/presence", Access: server.Member, Handle: server.RefuseDaemon("Only an app sends its presence.", o.servePresence)},
		{Pattern: "POST /v1/open", Access: server.LoopbackOnly, Handle: o.serveOpen},
	}
}

func (o *Opener) servePresence(w http.ResponseWriter, r *http.Request) {
	c, ok := server.CallerFrom(r.Context())
	if !ok || !c.Loopback && c.DeviceID == "" {
		server.WriteError(w, http.StatusForbidden, "Only an app sends its presence.")
		return
	}
	var p wire.AppPresence
	if !server.Decode(w, r, 1<<10, "presence: ", &p) {
		return
	}
	if p.Idle < 0 {
		server.WriteError(w, http.StatusBadRequest, "presence: idle must not be negative")
		return
	}
	o.book.Report(c, o.group.Self().Machine, time.Duration(p.Idle)*time.Second)
	w.WriteHeader(http.StatusNoContent)
}

func (o *Opener) serveOpen(w http.ResponseWriter, r *http.Request) {
	var req wire.OpenRequest
	if !server.Decode(w, r, 4<<10, "open: ", &req) {
		return
	}
	p, err := ParsePath(req.Path, req.Focus)
	if err != nil {
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := o.Open(r.Context(), p, netip.Addr{}, req.On, wire.OpenCLI, req.Agent)
	if err != nil {
		server.WriteError(w, openStatus(err), err.Error())
		return
	}
	server.WriteJSON(w, http.StatusOK, res)
}

func openStatus(err error) int {
	switch {
	case errors.Is(err, ErrUnknownMachine):
		return http.StatusNotFound
	case errors.As(err, new(*NeedsUpdateError)):
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}
