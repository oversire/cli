package group

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

const (
	// SyncEvery is the time between two pulls of the roster of each member.
	SyncEvery = 60 * time.Second
	// ProbeTimeout is the time limit of one whoami probe in Discover.
	ProbeTimeout = time.Second
)

// Syncer keeps the roster the same on all members: it pulls GET /v1/group from each member, and pokes them after a local change.
type Syncer struct {
	store   *Store
	peers   peerClient
	tailnet func(context.Context) ([]server.Peer, error)
	log     *slog.Logger
	every   time.Duration
	pokes   chan string
	taker   HomeTaker
}

// NewSyncer makes a syncer. tailnet gives the peers for discovery (server.Tailnet.Peers); nil means no discovery.
func NewSyncer(store *Store, tailnet func(context.Context) ([]server.Peer, error), log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{store: store, peers: peerClient{&http.Client{Timeout: 10 * time.Second}}, tailnet: tailnet, log: log,
		every: SyncEvery, pokes: make(chan string, 16)}
}

// Run pulls from all members at the start and each SyncEvery, pokes the members after a local change,
// and pulls from a member that poked this machine. It returns when ctx is done.
func (s *Syncer) Run(ctx context.Context) error {
	t := time.NewTicker(s.every)
	defer t.Stop()
	s.round(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.round(ctx)
		case <-s.store.Changed():
			s.pushRekey(ctx)
			s.pokeAll(ctx)
		case machine := <-s.pokes:
			if m, ok := s.member(machine); ok {
				s.pullOne(ctx, m)
			}
		}
	}
}

// others gives the members other than this machine.
func (s *Syncer) others() []wire.GroupMember {
	g, ok := s.store.Snapshot()
	if !ok {
		return nil
	}
	self := s.store.Self().Machine
	return slices.DeleteFunc(g.Members, func(m wire.GroupMember) bool { return m.Machine == self })
}

func (s *Syncer) member(machine string) (wire.GroupMember, bool) {
	for _, m := range s.others() {
		if m.Machine == machine {
			return m, true
		}
	}
	return wire.GroupMember{}, false
}

// round pulls from each member. When each member that answers refuses the credential of this machine,
// the machine was removed: it resets to a new group of one.
func (s *Syncer) round(ctx context.Context) {
	s.pushRekey(ctx)
	others := s.others()
	answered, refused := 0, 0
	for _, m := range others {
		unauthorized, err := s.pull(ctx, m)
		switch {
		case errors.Is(err, ErrRemoved):
			s.reset("removed in the roster of " + m.Machine)
			return
		case unauthorized:
			answered++
			refused++
		case err == nil:
			answered++
		default:
			s.log.Debug("group pull failed", "member", m.Machine, "err", err)
		}
	}
	if refused > 0 && refused == answered {
		s.reset("all members refuse the credential")
	}
}

func (s *Syncer) pullOne(ctx context.Context, m wire.GroupMember) {
	if _, err := s.pull(ctx, m); errors.Is(err, ErrRemoved) {
		s.reset("removed in the roster of " + m.Machine)
	}
}

func (s *Syncer) reset(why string) {
	s.log.Warn("group reset", "why", why)
	if err := s.store.Reset(); err != nil {
		s.log.Error("group reset failed", "err", err)
	}
}

// pull gets the roster of m and merges it. unauthorized is true when m refuses each credential of this machine.
func (s *Syncer) pull(ctx context.Context, m wire.GroupMember) (unauthorized bool, err error) {
	base := memberURL(m.Addrs, m.Host, m.Port)
	for _, token := range s.store.Tokens() {
		var g wire.Group
		code, err := s.peers.do(ctx, http.MethodGet, base+"/v1/group", token, nil, &g)
		if code == http.StatusUnauthorized {
			unauthorized = true
			continue
		}
		if err != nil {
			return false, err
		}
		s.store.Seen(m.Machine)
		if _, err := s.store.Merge(m.Machine, g); err != nil && !errors.Is(err, ErrOtherGroup) {
			return false, err
		}
		return false, nil
	}
	return unauthorized, nil
}

// send calls each member with the first credential that it accepts.
func (s *Syncer) send(ctx context.Context, path string, body any) {
	for _, m := range s.others() {
		base := memberURL(m.Addrs, m.Host, m.Port)
		for _, token := range s.store.Tokens() {
			code, err := s.peers.do(ctx, http.MethodPost, base+path, token, body, nil)
			if code == http.StatusUnauthorized {
				continue
			}
			if err != nil {
				s.log.Debug("group call failed", "member", m.Machine, "path", path, "err", err)
			}
			break
		}
	}
}

func (s *Syncer) pokeAll(ctx context.Context) { s.send(ctx, "/v1/group/poke", map[string]string{}) }

// rekeyBody carries a new secret from member to member after a rotate. Only daemons send and get it.
type rekeyBody struct {
	Secret string `json:"secret"`
}

func (s *Syncer) pushRekey(ctx context.Context) {
	if secret, ok := s.store.Rekey(); ok {
		s.send(ctx, "/v1/group/rekey", rekeyBody{Secret: secret})
	}
}

// HomeTaker copies the state of the old home to this machine before this machine becomes home (store.Routes).
// offline is true when the old home does not answer; the move then goes on.
type HomeTaker interface {
	TakeHome(ctx context.Context, base, token string) (records int, offline bool, err error)
}

// WithHomeTaker sets the taker that POST /v1/group/home calls when this machine becomes home.
func (s *Syncer) WithHomeTaker(t HomeTaker) *Syncer {
	s.taker = t
	return s
}

// homeMove is the answer of POST /v1/group/home.
type homeMove struct {
	OK      bool `json:"ok"`
	Records int  `json:"records,omitempty"`
	Offline bool `json:"offline,omitempty"`
}

// takeHome runs the taker when machine is this machine, the group has no cloud, and the old home is another member.
func (s *Syncer) takeHome(ctx context.Context, machine string) (homeMove, error) {
	out := homeMove{OK: true}
	g, found := s.store.Snapshot()
	if s.taker == nil || !found || g.Cloud != nil || machine != s.store.Self().Machine || g.Home == machine {
		return out, nil
	}
	old, isMember := s.member(g.Home)
	if !isMember {
		return out, nil
	}
	token := ""
	if t := s.store.Tokens(); len(t) > 0 {
		token = t[0]
	}
	n, offline, err := s.taker.TakeHome(ctx, memberURL(old.Addrs, old.Host, old.Port), token)
	if err != nil {
		return out, fmt.Errorf("group: copy the store of %s: %w", old.Machine, err)
	}
	out.Records, out.Offline = n, offline
	return out, nil
}

// Discover probes GET /v1/whoami on each online peer, with a time limit of ProbeTimeout. It gives the daemons that answer.
func (s *Syncer) Discover(ctx context.Context, peers []server.Peer) []wire.DaemonInfo {
	port := s.store.Self().Port
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := []wire.DaemonInfo{}
	for _, p := range peers {
		if !p.Online {
			continue
		}
		host := p.DNS
		if len(p.Addrs) > 0 {
			host = p.Addrs[0].String()
		}
		if host == "" {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
			defer cancel()
			var info wire.DaemonInfo
			if _, err := s.peers.do(ctx, http.MethodGet, Endpoint(host, port)+"/v1/whoami", "", nil, &info); err != nil || info.Machine == "" {
				return
			}
			mu.Lock()
			out = append(out, info)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b wire.DaemonInfo) int { return cmp.Compare(a.Machine, b.Machine) })
	return out
}

// daemonCaller gives the caller when it is a daemon of the group on Tailscale.
func daemonCaller(r *http.Request) (server.Caller, bool) {
	c, ok := server.CallerFrom(r.Context())
	return c, ok && !c.Loopback && c.Kind == string(wire.DeviceDaemon) && c.Machine != ""
}

var errDaemonOnly = errors.New("group: only a daemon of the group can call this route")

// Routes gives the roster routes.
func (s *Syncer) Routes() []server.Route {
	ok := map[string]bool{"ok": true}
	return []server.Route{
		{Pattern: "GET /v1/group", Access: server.Member, Handle: func(w http.ResponseWriter, r *http.Request) {
			g, found := s.store.Snapshot()
			if !found {
				writeErr(w, ErrNoGroup)
				return
			}
			writeJSON(w, http.StatusOK, g)
		}},
		{Pattern: "POST /v1/group/poke", Access: server.Member, Handle: func(w http.ResponseWriter, r *http.Request) {
			c, isDaemon := daemonCaller(r)
			if !isDaemon {
				writeJSON(w, http.StatusForbidden, errorBody{Error: errDaemonOnly.Error()})
				return
			}
			select {
			case s.pokes <- c.Machine:
			default:
			}
			writeJSON(w, http.StatusOK, ok)
		}},
		{Pattern: "POST /v1/group/rekey", Access: server.Member, Handle: func(w http.ResponseWriter, r *http.Request) {
			c, isDaemon := daemonCaller(r)
			if !isDaemon {
				writeJSON(w, http.StatusForbidden, errorBody{Error: errDaemonOnly.Error()})
				return
			}
			var in rekeyBody
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			reply(w, ok, s.store.TakeRekey(c.Machine, in.Secret))
		}},
		{Pattern: "POST /v1/group/renew", Access: server.Member, Handle: func(w http.ResponseWriter, r *http.Request) {
			c, found := server.CallerFrom(r.Context())
			if !found || c.Loopback {
				writeErr(w, ErrNotFound)
				return
			}
			cred, err := s.store.Credential(c.DeviceID)
			reply(w, cred, err)
		}},
		{Pattern: "POST /v1/group/devices", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Name string          `json:"name"`
				Kind wire.DeviceKind `json:"kind"`
			}
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			if in.Kind != "" && in.Kind != wire.DeviceApp {
				writeErr(w, ErrBadInput)
				return
			}
			cred, err := s.store.Issue(wire.GroupDevice{Name: in.Name, Kind: wire.DeviceApp})
			reply(w, cred, err)
		}},
		{Pattern: "DELETE /v1/group/members/{machine}", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, ok, s.store.Remove(r.PathValue("machine")))
		}},
		{Pattern: "DELETE /v1/group/devices/{id}", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, ok, s.store.Remove(r.PathValue("id")))
		}},
		{Pattern: "POST /v1/group/home", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Machine string `json:"machine"`
			}
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			out, err := s.takeHome(r.Context(), in.Machine)
			if err != nil {
				writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error()})
				return
			}
			reply(w, out, s.store.SetHome(in.Machine))
		}},
		{Pattern: "POST /v1/group/rotate", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, ok, s.store.Rotate())
		}},
		{Pattern: "GET /v1/group/peers", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			if s.tailnet == nil {
				writeJSON(w, http.StatusOK, []wire.DaemonInfo{})
				return
			}
			peers, err := s.tailnet(r.Context())
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "tailscale: " + err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, s.Discover(r.Context(), peers))
		}},
	}
}
