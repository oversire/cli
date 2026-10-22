// Package server serves the local-hub API of the daemon (role serve): /v1 on loopback without a token,
// and on the tailnet addresses with a group credential.
package server

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/oversire/cli/wire"
)

// Events is the event log (eventlog.Log).
type Events interface {
	Page(after, limit int) wire.EventsPage
	Subscribe() (<-chan struct{}, func()) // a poke for each append
}

// Sessions is the session book (sessions.Book).
type Sessions interface {
	All() []wire.DaemonSession
	Get(id string) (wire.DaemonSession, bool)
}

// HookSink takes the payload of `oversire hook <harness>` (hooks.Ingest).
type HookSink interface {
	Ingest(ctx context.Context, harness string, env wire.HookEnv, payload []byte) error
}

// Verifier checks group credentials (group.Store).
type Verifier interface {
	Verify(token string) (Caller, bool) // HMAC and revoked check, constant-time compare
	Revoked() <-chan string             // device ids, to close their sockets
}

// Caller is who made a request. A loopback caller has no credential.
type Caller struct {
	DeviceID, Name, Kind, Machine string
	Loopback                      bool
}

type Access int

const (
	Member       Access = iota // a credential on Tailscale
	Public                     // no credential: whoami and the join routes
	LoopbackOnly               // 404 on Tailscale
)

// Route is one entry of the route table. Pattern uses the net/http ServeMux syntax, for example "GET /v1/sessions/{id}".
type Route struct {
	Pattern string
	Access  Access
	Handle  http.HandlerFunc
}

// Peer is one node of `tailscale status --json`.
type Peer struct {
	Name   string // HostName
	DNS    string // DNSName without the last dot
	Addrs  []netip.Addr
	Online bool
	OS     string
}

type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom gives the caller of a request that the server let in.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
