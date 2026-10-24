package orca

import (
	"context"
	"errors"
	"sync"

	"github.com/oversire/cli/runtime/orca/orcaws"
	"github.com/oversire/cli/wire"
)

// pairAccount is the keychain account of the Orca pairing.
const pairAccount = "orca"

// liveFailures in a row turn the live view off until a retry or a new pairing.
const liveFailures = 3

// UnpairedReason tells the user how to get the live view.
const UnpairedReason = "Pair Orca to see the live terminal: in Orca, open Settings and make a runtime pairing link for this computer only."

// Secrets keeps the pairing code. The code holds the device token.
type Secrets interface {
	Set(ctx context.Context, device, token string) error
	Get(ctx context.Context, device string) (string, error)
	Delete(ctx context.Context, device string)
}

// Pairing is the paired device of the daemon in the local Orca.
type Pairing struct {
	secrets Secrets
	dial    func(context.Context, orcaws.Offer) (*orcaws.Conn, error)

	mu       sync.Mutex
	loaded   bool
	offer    *orcaws.Offer
	failures int
	reason   string // the last failure
}

func NewPairing(s Secrets) *Pairing {
	return &Pairing{secrets: s, dial: orcaws.Dial}
}

func (p *Pairing) load(ctx context.Context) *orcaws.Offer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		p.loaded = true
		if code, err := p.secrets.Get(ctx, pairAccount); err == nil {
			if o, err := orcaws.ParseCode(code); err == nil {
				p.offer = &o
			}
		}
	}
	return p.offer
}

// Pair checks the link against Orca, then keeps it.
func (p *Pairing) Pair(ctx context.Context, link string) error {
	o, err := orcaws.ParseLink(link)
	if err != nil {
		return err
	}
	c, err := p.connect(ctx, o)
	if err != nil {
		return err
	}
	c.Close()
	if err := p.secrets.Set(ctx, pairAccount, o.Code()); err != nil {
		return err
	}
	p.mu.Lock()
	p.loaded, p.offer, p.failures, p.reason = true, &o, 0, ""
	p.mu.Unlock()
	return nil
}

// Unpair forgets the pairing. The device stays in Orca › Settings until the user removes it.
func (p *Pairing) Unpair(ctx context.Context) {
	p.secrets.Delete(ctx, pairAccount)
	p.mu.Lock()
	p.loaded, p.offer, p.failures, p.reason = true, nil, 0, ""
	p.mu.Unlock()
}

// connect dials and checks the protocol version.
func (p *Pairing) connect(ctx context.Context, o orcaws.Offer) (*orcaws.Conn, error) {
	c, err := p.dial(ctx, o)
	if err != nil {
		return nil, err
	}
	var st orcaws.Status
	if err = c.Call(ctx, "status.get", nil, &st); err == nil {
		err = st.Check()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Dial opens a checked socket with the kept pairing, and counts the result.
func (p *Pairing) Dial(ctx context.Context) (*orcaws.Conn, error) {
	o := p.load(ctx)
	if o == nil {
		return nil, errors.New("Orca is not paired.")
	}
	c, err := p.connect(ctx, *o)
	p.note(err)
	return c, err
}

func (p *Pairing) note(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.failures, p.reason = 0, ""
		return
	}
	p.failures++
	p.reason = err.Error()
}

// Live tells if a viewer gets the live view.
func (p *Pairing) Live(ctx context.Context) bool {
	if p.load(ctx) == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures < liveFailures
}

// Retry clears the failures and dials once.
func (p *Pairing) Retry(ctx context.Context) {
	p.mu.Lock()
	p.failures = 0
	p.mu.Unlock()
	if c, err := p.Dial(ctx); err == nil {
		c.Close()
	}
}

// State is the stream state for GET /v1/runtimes.
func (p *Pairing) State(ctx context.Context) wire.RuntimeStream {
	paired := p.load(ctx) != nil
	st := wire.RuntimeStream{State: wire.StreamTextOnly, Pairable: true, Paired: paired}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !paired:
		st.Reason = UnpairedReason
	case p.failures >= liveFailures:
		st.Reason = "Pairing failed: " + p.reason
	default:
		st.State = wire.StreamLive
	}
	return st
}
