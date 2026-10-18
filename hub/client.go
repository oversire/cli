// Package hub is the WebSocket channel to the OverSire hub. The runner dials out; the hub never connects in.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/oversire/runner/wire"
)

// ErrUnauthorized means the hub does not know the token. Pair the runner again.
var ErrUnauthorized = errors.New("The hub refused the token (401). Pair the runner again with `oversire-runner pair`.")

// RefusedError is a refused hello. Update the runner.
type RefusedError struct {
	Reason    string
	Contracts []int
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("The hub refused this runner: %s Update the runner (hub contracts %v, runner contract %d).",
		e.Reason, e.Contracts, wire.ContractVersion)
}

// Runtimes runs orders.
type Runtimes interface {
	IDs() []string
	Has(id string) bool
	Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error)
}

// Allow checks orders against the local allow list.
type Allow interface {
	Check(o wire.WorkOrder) error
	Summary() wire.AllowSummary
}

// Status receives the connection state.
type Status interface {
	Connected(since time.Time)
	Disconnected(err error)
	Order(id string)
}

type Client struct {
	hub      string
	token    string
	version  string
	runtimes Runtimes
	allow    Allow
	journal  *Journal
	status   Status

	HTTPClient  *http.Client
	Log         *slog.Logger
	OnUpdate    func(version string)
	Now         func() time.Time
	PingEvery   time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration

	mu      sync.Mutex
	conn    *websocket.Conn
	running map[string]bool
}

func NewClient(hub, token, version string, runtimes Runtimes, allow Allow, journal *Journal, status Status) *Client {
	return &Client{
		hub: strings.TrimRight(hub, "/"), token: token, version: version,
		runtimes: runtimes, allow: allow, journal: journal, status: status,
		HTTPClient: http.DefaultClient, Log: slog.Default(), Now: time.Now,
		PingEvery: 30 * time.Second, MinBackoff: time.Second, MaxBackoff: 5 * time.Minute, StableAfter: time.Minute,
		running: map[string]bool{},
	}
}

// ConnectURL is the WebSocket URL of a hub base URL.
func ConnectURL(hub string) string {
	u := strings.TrimRight(hub, "/") + "/v1/connect"
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// Run connects and reconnects with backoff until ctx ends, the token is refused or the hello is refused.
// Orders keep ctx, so a lost socket does not stop a launch.
func (c *Client) Run(ctx context.Context) error {
	wait := c.MinBackoff
	for {
		start := c.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			c.status.Disconnected(nil)
			return ctx.Err()
		}
		c.status.Disconnected(err)
		var refused *RefusedError
		if errors.Is(err, ErrUnauthorized) || errors.As(err, &refused) {
			return err
		}
		if c.Now().Sub(start) > c.StableAfter {
			wait = c.MinBackoff
		}
		jittered := time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64()))
		c.Log.Warn("hub connection lost", "err", err, "retry", jittered.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jittered):
		}
		wait = min(wait*2, c.MaxBackoff)
	}
}

func (c *Client) session(ctx context.Context) error {
	conn, resp, err := websocket.Dial(ctx, ConnectURL(c.hub), &websocket.DialOptions{
		HTTPClient: c.HTTPClient,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.setConn(conn)
	defer c.setConn(nil)

	summary := c.allow.Summary()
	hello := wire.Hello{
		Version: c.version, Contract: wire.ContractVersion, Runtimes: nonNil(c.runtimes.IDs()),
		OS: runtime.GOOS + "/" + runtime.GOARCH, Allow: &summary,
	}
	if err := c.write(sctx, conn, hello); err != nil {
		return err
	}
	go c.ping(sctx, conn, cancel)

	for {
		kind, data, err := conn.Read(sctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText || string(data) == "pong" {
			continue
		}
		m, err := wire.DecodeMessage(data)
		if err != nil {
			c.badMessage(ctx, data, err)
			continue
		}
		switch m := m.(type) {
		case wire.Welcome:
			c.Log.Info("hub welcome", "device", m.Device, "contract", m.Contract)
			c.status.Connected(c.Now())
			c.flush(sctx, conn)
		case wire.Refused:
			conn.Close(websocket.StatusNormalClosure, "refused")
			return &RefusedError{Reason: m.Reason, Contracts: m.Contracts}
		case wire.Order:
			go c.handle(ctx, m.Order)
		case wire.Update:
			if c.OnUpdate != nil {
				c.OnUpdate(m.Version)
			}
		}
	}
}

func (c *Client) ping(ctx context.Context, conn *websocket.Conn, cancel func()) {
	t := time.NewTicker(c.PingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wctx, done := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, []byte("ping"))
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// badMessage fails an order that does not decode, so that the hub does not send it for ever.
func (c *Client) badMessage(ctx context.Context, data []byte, err error) {
	var loose struct {
		Type  string `json:"type"`
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if json.Unmarshal(data, &loose) != nil || loose.Type != "order" || loose.Order.ID == "" {
		c.Log.Warn("hub message ignored", "err", err)
		return
	}
	c.finish(ctx, wire.Result{Order: loose.Order.ID, State: wire.ResultFailed,
		Result: &wire.WorkOrderResult{Error: "This runner cannot read the order: " + err.Error()}})
}

// handle runs one order at most one time. A finished order only sends its result again.
func (c *Client) handle(ctx context.Context, o wire.WorkOrder) {
	c.mu.Lock()
	if c.running[o.ID] {
		c.mu.Unlock()
		return
	}
	// Under the lock: a finished order is in the journal before it leaves running.
	if r, ok := c.journal.Get(o.ID); ok {
		c.mu.Unlock()
		c.send(ctx, r)
		return
	}
	c.running[o.ID] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.running, o.ID)
		c.mu.Unlock()
	}()
	c.status.Order(o.ID)
	log := c.Log.With("order", o.ID, "op", o.Payload.Op(), "runtime", o.Runtime)

	if !o.ExpiresAt.After(c.Now()) {
		log.Info("order expired")
		c.finish(ctx, wire.Result{Order: o.ID, State: wire.ResultExpired})
		return
	}
	reason := ""
	if !c.runtimes.Has(o.Runtime) {
		reason = fmt.Sprintf("This runner has no runtime %s.", o.Runtime)
	} else if err := c.allow.Check(o); err != nil {
		reason = err.Error()
	}
	if reason != "" {
		log.Warn("order refused", "reason", reason)
		c.finish(ctx, wire.Result{Order: o.ID, State: wire.ResultFailed, Result: &wire.WorkOrderResult{Error: reason}})
		return
	}
	c.sendMessage(ctx, wire.Ack{Order: o.ID})
	log.Info("order started")
	res, err := c.runtimes.Do(ctx, o)
	if err != nil {
		log.Warn("order failed", "err", err)
		c.finish(ctx, wire.Result{Order: o.ID, State: wire.ResultFailed, Result: &wire.WorkOrderResult{Error: err.Error()}})
		return
	}
	log.Info("order done")
	c.finish(ctx, wire.Result{Order: o.ID, State: wire.ResultDone, Result: res})
}

func (c *Client) finish(ctx context.Context, r wire.Result) {
	c.journal.Put(r)
	c.send(ctx, r)
}

func (c *Client) send(ctx context.Context, r wire.Result) {
	if c.sendMessage(ctx, r) {
		c.journal.MarkSent(r.Order)
	}
}

// flush sends the results that a lost socket did not carry.
func (c *Client) flush(ctx context.Context, conn *websocket.Conn) {
	for _, r := range c.journal.Unsent() {
		if c.write(ctx, conn, r) == nil {
			c.journal.MarkSent(r.Order)
		}
	}
}

func (c *Client) sendMessage(ctx context.Context, m wire.Message) bool {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn != nil && c.write(ctx, conn, m) == nil
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, m wire.Message) error {
	data, err := wire.Encode(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
