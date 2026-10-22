package group

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// Limits of the join flows.
const (
	InviteTTL    = 10 * time.Minute
	RequestTTL   = 5 * time.Minute
	MaxOpen      = 3 // open join requests
	RateLimit    = 5 // calls of the public join routes per peer address and RateWindow
	RateWindow   = time.Minute
	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base 32
	codeLen      = 8
)

// Status values of a join request.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
	StatusExpired  = "expired"
)

// Pending is an open join request, for `oversire group requests` and the app.
type Pending struct {
	Request   string          `json:"request"`
	Machine   string          `json:"machine"`
	Host      string          `json:"host"`
	Kind      wire.DeviceKind `json:"kind"`
	Confirm   string          `json:"confirm"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

type request struct {
	wire.JoinRequest
	id, confirm string
	expires     time.Time
	state       string
	result      wire.JoinResult
}

// Joins runs the join flows: codes (invite, offer) and requests that wait for approval.
type Joins struct {
	store     *Store
	notify    func(any) // a frame to the loopback WebSocket clients
	clock     func() time.Time
	peers     peerClient
	pollEvery time.Duration

	mu       sync.Mutex
	invites  map[string]time.Time // codes that admit a machine to this group
	offers   map[string]time.Time // codes that let a member pull this machine into its group
	requests map[string]*request
	hits     map[string][]time.Time
}

func NewJoins(store *Store, notify func(any), clock func() time.Time) *Joins {
	if clock == nil {
		clock = time.Now
	}
	if notify == nil {
		notify = func(any) {}
	}
	return &Joins{store: store, notify: notify, clock: clock, peers: peerClient{&http.Client{Timeout: 15 * time.Second}},
		pollEvery: time.Second, invites: map[string]time.Time{}, offers: map[string]time.Time{},
		requests: map[string]*request{}, hits: map[string][]time.Time{}}
}

// Invite makes a one-use code for a new machine.
func (j *Joins) Invite() (wire.GroupInvite, error) {
	if _, ok := j.store.Snapshot(); !ok {
		return wire.GroupInvite{}, ErrNoGroup
	}
	return j.code(j.invites)
}

// Offer makes a one-use code that a member uses to add this machine (`oversire group request-code`).
func (j *Joins) Offer() (wire.GroupInvite, error) {
	if !j.store.Alone() {
		return wire.GroupInvite{}, ErrInGroup
	}
	return j.code(j.offers)
}

func (j *Joins) code(into map[string]time.Time) (wire.GroupInvite, error) {
	c := newCode()
	exp := j.clock().Add(InviteTTL).UTC()
	j.mu.Lock()
	into[c] = exp
	j.mu.Unlock()
	self := j.store.Self()
	return wire.GroupInvite{Code: FormatCode(c), Via: via(self.Host, self.Port), ExpiresAt: exp}, nil
}

// Join admits the machine or app of r when its code is a valid invite. The code is then used.
func (j *Joins) Join(r wire.JoinRequest) (wire.JoinResult, error) {
	if !j.take(j.invites, r.Code) {
		return wire.JoinResult{}, ErrBadCode
	}
	return j.store.Admit(r)
}

// take uses a code. It gives false for an unknown or expired code.
func (j *Joins) take(from map[string]time.Time, code string) bool {
	c := NormalizeCode(code)
	j.mu.Lock()
	defer j.mu.Unlock()
	exp, ok := from[c]
	delete(from, c)
	return ok && j.clock().Before(exp)
}

// Request opens a join request that waits for approval on this machine. The loopback clients get a joinRequest frame.
func (j *Joins) Request(r wire.JoinRequest) (wire.JoinPending, error) {
	if r.Machine == "" || len(r.Nonce) < 8 {
		return wire.JoinPending{}, ErrBadInput
	}
	if _, ok := j.store.Snapshot(); !ok {
		return wire.JoinPending{}, ErrNoGroup
	}
	j.mu.Lock()
	j.pruneLocked()
	open := 0
	for _, q := range j.requests {
		if q.state == StatusPending && j.clock().Before(q.expires) {
			open++
		}
	}
	if open >= MaxOpen {
		j.mu.Unlock()
		return wire.JoinPending{}, ErrBusy
	}
	if r.Kind == "" {
		r.Kind = wire.DeviceDaemon
	}
	id := make([]byte, 4)
	rand.Read(id)
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	q := &request{JoinRequest: r, id: "r-" + hex.EncodeToString(id), confirm: fmt.Sprintf("%04d", n.Int64()),
		expires: j.clock().Add(RequestTTL).UTC(), state: StatusPending}
	j.requests[q.id] = q
	j.mu.Unlock()
	j.notify(map[string]any{"type": "joinRequest", "request": q.id, "machine": r.Machine, "host": r.Host,
		"kind": r.Kind, "confirm": q.confirm, "expiresAt": q.expires})
	return wire.JoinPending{Request: q.id, Confirm: q.confirm, ExpiresAt: q.expires}, nil
}

// Pending lists the open join requests.
func (j *Joins) Pending() []Pending {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pruneLocked()
	out := []Pending{}
	for _, q := range j.requests {
		if q.state == StatusPending && j.clock().Before(q.expires) {
			out = append(out, Pending{Request: q.id, Machine: q.Machine, Host: q.Host, Kind: q.Kind, Confirm: q.confirm, ExpiresAt: q.expires})
		}
	}
	slices.SortFunc(out, func(a, b Pending) int { return a.ExpiresAt.Compare(b.ExpiresAt) })
	return out
}

// Decide approves or denies a join request.
func (j *Joins) Decide(id string, approve bool) error {
	j.mu.Lock()
	q, ok := j.requests[id]
	switch {
	case !ok:
		j.mu.Unlock()
		return ErrNotFound
	case !j.clock().Before(q.expires):
		j.mu.Unlock()
		return ErrExpired
	case q.state != StatusPending:
		j.mu.Unlock()
		return ErrDecided
	}
	if !approve {
		q.state = StatusDenied
		j.mu.Unlock()
		return nil
	}
	r := q.JoinRequest
	j.mu.Unlock()
	res, err := j.store.Admit(r)
	if err != nil {
		return err
	}
	j.mu.Lock()
	q.state, q.result = StatusApproved, res
	j.mu.Unlock()
	return nil
}

// Poll gives the result of a join request to the machine that made it. status is pending, approved, denied or expired.
func (j *Joins) Poll(id, nonce string) (wire.JoinResult, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	q, ok := j.requests[id]
	if !ok || subtle.ConstantTimeCompare([]byte(q.Nonce), []byte(nonce)) != 1 {
		return wire.JoinResult{}, "", ErrNotFound
	}
	if q.state == StatusApproved {
		return q.result, StatusApproved, nil
	}
	if !j.clock().Before(q.expires) {
		return wire.JoinResult{}, StatusExpired, nil
	}
	return wire.JoinResult{}, q.state, nil
}

// pruneLocked drops requests some time after they expire, so that a late poll still reads "expired".
func (j *Joins) pruneLocked() {
	now := j.clock()
	for id, q := range j.requests {
		if now.Sub(q.expires) > RequestTTL {
			delete(j.requests, id)
		}
	}
	for _, m := range []map[string]time.Time{j.invites, j.offers} {
		for c, exp := range m {
			if !now.Before(exp) {
				delete(m, c)
			}
		}
	}
}

// allow counts a call of a public join route from addr.
func (j *Joins) allow(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	now := j.clock()
	j.mu.Lock()
	defer j.mu.Unlock()
	hits := slices.DeleteFunc(j.hits[host], func(t time.Time) bool { return now.Sub(t) >= RateWindow })
	if len(hits) >= RateLimit {
		j.hits[host] = hits
		return false
	}
	j.hits[host] = append(hits, now)
	return true
}

// selfRequest is the join request of this daemon.
func (j *Joins) selfRequest(code string) wire.JoinRequest {
	m := j.store.Self()
	n := make([]byte, 8)
	rand.Read(n)
	return wire.JoinRequest{Code: code, Machine: m.Machine, Host: m.Host, Addrs: m.Addrs, Port: m.Port, Roles: m.Roles,
		Kind: wire.DeviceDaemon, Nonce: hex.EncodeToString(n)}
}

// Enter joins the group of the member at host with a code from that member (`oversire join`).
func (j *Joins) Enter(ctx context.Context, host, code string) (wire.Group, error) {
	if !j.store.Alone() {
		return wire.Group{}, ErrInGroup
	}
	var res wire.JoinResult
	base := Endpoint(host, j.store.Self().Port)
	if _, err := j.peers.do(ctx, http.MethodPost, base+"/v1/group/join", "", j.selfRequest(code), &res); err != nil {
		return wire.Group{}, fmt.Errorf("join %s: %w", host, err)
	}
	if err := j.store.Adopt(res); err != nil {
		return wire.Group{}, err
	}
	g, _ := j.store.Snapshot()
	return g, nil
}

// Ask sends a join request to the member at host, and polls for the decision in the background.
// The loopback clients get a joinResult frame with the status at the end.
func (j *Joins) Ask(ctx context.Context, host string) (wire.JoinPending, error) {
	if !j.store.Alone() {
		return wire.JoinPending{}, ErrInGroup
	}
	r := j.selfRequest("")
	base := Endpoint(host, j.store.Self().Port)
	var p wire.JoinPending
	if _, err := j.peers.do(ctx, http.MethodPost, base+"/v1/group/requests", "", r, &p); err != nil {
		return p, fmt.Errorf("ask %s: %w", host, err)
	}
	go j.await(base, host, p, r.Nonce)
	return p, nil
}

func (j *Joins) await(base, host string, p wire.JoinPending, nonce string) {
	ctx, cancel := context.WithTimeout(context.Background(), p.ExpiresAt.Sub(j.clock())+30*time.Second)
	defer cancel()
	status, err := j.poll(ctx, base, p.Request, nonce)
	msg := map[string]any{"type": "joinResult", "host": host, "request": p.Request, "status": status}
	if err != nil {
		msg["error"] = err.Error()
	}
	j.notify(msg)
}

// poll asks for the decision each pollEvery. With an approval, it adopts the group.
func (j *Joins) poll(ctx context.Context, base, id, nonce string) (string, error) {
	t := time.NewTicker(j.pollEvery)
	defer t.Stop()
	for {
		var res wire.JoinResult
		_, err := j.peers.do(ctx, http.MethodGet, base+"/v1/group/requests/"+id+"?nonce="+nonce, "", nil, &res)
		var re *RemoteError
		switch {
		case err == nil && res.Group.ID != "":
			return StatusApproved, j.store.Adopt(res)
		case errors.As(err, &re) && re.Code == http.StatusForbidden:
			return StatusDenied, nil
		case errors.As(err, &re) && (re.Code == http.StatusGone || re.Code == http.StatusNotFound):
			return StatusExpired, nil
		}
		select {
		case <-ctx.Done():
			return StatusExpired, ctx.Err()
		case <-t.C:
		}
	}
}

// TakeOffer runs on the machine without a group: a member sent the code of `group request-code` and its address.
// This machine then joins the group of that member by code.
func (j *Joins) TakeOffer(ctx context.Context, inv wire.GroupInvite) (wire.Group, error) {
	if !j.store.Alone() {
		return wire.Group{}, ErrInGroup
	}
	if inv.Via == "" || !j.take(j.offers, inv.Code) {
		return wire.Group{}, ErrBadCode
	}
	return j.Enter(ctx, inv.Via, inv.Code)
}

// Admit adds the machine at host that showed code (`oversire group admit`). The code becomes an invite of this
// group, and the machine joins with it.
func (j *Joins) Admit(ctx context.Context, host, code string) error {
	if _, ok := j.store.Snapshot(); !ok {
		return ErrNoGroup
	}
	c := NormalizeCode(code)
	if len(c) != codeLen {
		return ErrBadCode
	}
	exp := j.clock().Add(InviteTTL).UTC()
	j.mu.Lock()
	j.invites[c] = exp
	j.mu.Unlock()
	self := j.store.Self()
	inv := wire.GroupInvite{Code: FormatCode(c), Via: via(self.Host, self.Port), ExpiresAt: exp}
	if _, err := j.peers.do(ctx, http.MethodPost, Endpoint(host, self.Port)+"/v1/group/offer", "", inv, nil); err != nil {
		j.mu.Lock()
		delete(j.invites, c)
		j.mu.Unlock()
		return fmt.Errorf("admit %s: %w", host, err)
	}
	return nil
}

// Routes gives the join routes.
func (j *Joins) Routes() []server.Route {
	return []server.Route{
		{Pattern: "POST /v1/group/invites", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			inv, err := j.Invite()
			reply(w, inv, err)
		}},
		{Pattern: "POST /v1/group/offers", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			inv, err := j.Offer()
			reply(w, inv, err)
		}},
		{Pattern: "POST /v1/group/requests", Access: server.Public, Handle: j.limited(func(w http.ResponseWriter, r *http.Request) {
			var req wire.JoinRequest
			if err := readJSON(r, &req); err != nil {
				writeErr(w, err)
				return
			}
			if req.Code != "" {
				writeErr(w, ErrBadInput)
				return
			}
			p, err := j.Request(req)
			reply(w, p, err)
		})},
		{Pattern: "GET /v1/group/requests", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, j.Pending())
		}},
		{Pattern: "GET /v1/group/requests/{id}", Access: server.Public, Handle: func(w http.ResponseWriter, r *http.Request) {
			res, status, err := j.Poll(r.PathValue("id"), r.URL.Query().Get("nonce"))
			switch {
			case err != nil:
				writeErr(w, err)
			case status == StatusApproved:
				writeJSON(w, http.StatusOK, res)
			case status == StatusDenied:
				writeJSON(w, http.StatusForbidden, errorBody{Status: status, Error: ErrDenied.Error()})
			case status == StatusExpired:
				writeJSON(w, http.StatusGone, errorBody{Status: status, Error: ErrExpired.Error()})
			default:
				writeJSON(w, http.StatusAccepted, errorBody{Status: status})
			}
		}},
		{Pattern: "POST /v1/group/requests/{id}/approve", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, map[string]bool{"ok": true}, j.Decide(r.PathValue("id"), true))
		}},
		{Pattern: "POST /v1/group/requests/{id}/deny", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			reply(w, map[string]bool{"ok": true}, j.Decide(r.PathValue("id"), false))
		}},
		{Pattern: "POST /v1/group/join", Access: server.Public, Handle: j.limited(func(w http.ResponseWriter, r *http.Request) {
			var req wire.JoinRequest
			if err := readJSON(r, &req); err != nil {
				writeErr(w, err)
				return
			}
			res, err := j.Join(req)
			reply(w, res, err)
		})},
		{Pattern: "POST /v1/group/offer", Access: server.Public, Handle: j.limited(func(w http.ResponseWriter, r *http.Request) {
			var inv wire.GroupInvite
			if err := readJSON(r, &inv); err != nil {
				writeErr(w, err)
				return
			}
			g, err := j.TakeOffer(r.Context(), inv)
			reply(w, g, err)
		})},
		{Pattern: "POST /v1/group/enter", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Host, Code string }
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			g, err := j.Enter(r.Context(), in.Host, in.Code)
			reply(w, g, err)
		}},
		{Pattern: "POST /v1/group/ask", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Host string }
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			p, err := j.Ask(r.Context(), in.Host)
			reply(w, p, err)
		}},
		{Pattern: "POST /v1/group/admit", Access: server.LoopbackOnly, Handle: func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Host, Code string }
			if err := readJSON(r, &in); err != nil {
				writeErr(w, err)
				return
			}
			reply(w, map[string]bool{"ok": true}, j.Admit(r.Context(), in.Host, in.Code))
		}},
	}
}

// limited applies the rate limit of the public join routes.
func (j *Joins) limited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !j.allow(r.RemoteAddr) {
			writeErr(w, ErrRate)
			return
		}
		h(w, r)
	}
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) {
			writeJSON(w, http.StatusBadGateway, errorBody{Error: err.Error(), Status: re.Status})
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func newCode() string {
	b := make([]byte, codeLen)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		b[i] = codeAlphabet[n.Int64()]
	}
	return string(b)
}

// NormalizeCode removes dashes and spaces, uses upper case, and maps O to 0 and I and L to 1.
func NormalizeCode(c string) string {
	c = strings.ToUpper(c)
	c = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(c)
	return c
}

// FormatCode shows a code as XXXX-XXXX.
func FormatCode(c string) string {
	if len(c) != codeLen {
		return c
	}
	return c[:4] + "-" + c[4:]
}
