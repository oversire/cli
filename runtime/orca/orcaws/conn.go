package orcaws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/nacl/box"
)

// The protocol rule of the Orca runtime.
const (
	ProtocolVersion  = 3 // the version that this client speaks
	MinServerVersion = 2 // the oldest server that this client takes
)

const (
	readLimit        = 8<<20 + 64 // the largest frame of Orca
	handshakeTimeout = 10 * time.Second
	writeTimeout     = 10 * time.Second
	streamBuffer     = 256
)

// Error is an error reply of Orca.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message + " (" + e.Code + ")" }

// Reply is one reply of Orca. A stream gives many replies with Streaming set.
type Reply struct {
	ID        string          `json:"id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result"`
	Error     *Error          `json:"error"`
	Streaming bool            `json:"streaming"`
	Keepalive bool            `json:"_keepalive"`
}

// Status is the part of status.get that the client reads.
type Status struct {
	ProtocolVersion  int      `json:"runtimeProtocolVersion"`
	MinClientVersion int      `json:"minCompatibleRuntimeClientVersion"`
	Capabilities     []string `json:"capabilities"`
}

// Check tells if this client and the server agree on the protocol. An absent field counts as 0.
func (s Status) Check() error {
	if s.MinClientVersion > ProtocolVersion {
		return fmt.Errorf("This Orca needs a newer client (protocol %d, the daemon speaks %d).", s.MinClientVersion, ProtocolVersion)
	}
	if s.ProtocolVersion < MinServerVersion {
		return fmt.Errorf("This Orca is too old (protocol %d, the daemon needs %d).", s.ProtocolVersion, MinServerVersion)
	}
	return nil
}

// Conn is one authenticated socket to Orca.
type Conn struct {
	ws    *websocket.Conn
	key   *[32]byte
	token string

	writeMu sync.Mutex
	next    atomic.Uint64

	mu      sync.Mutex
	pending map[string]chan Reply
	err     error
	done    chan struct{}
	quit    chan struct{}
	once    sync.Once
}

// Dial opens a socket and authenticates with the device token of the offer.
func Dial(ctx context.Context, o Offer) (*Conn, error) {
	host, err := o.hostKey()
	if err != nil {
		return nil, err
	}
	endpoint, err := o.endpoint()
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(hctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("Orca does not answer on %s: %w", endpoint, err)
	}
	ws.SetReadLimit(readLimit)
	c := &Conn{ws: ws, token: o.DeviceToken, pending: map[string]chan Reply{}, done: make(chan struct{}), quit: make(chan struct{})}
	if err := c.handshake(hctx, host); err != nil {
		ws.CloseNow()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

func (c *Conn) handshake(ctx context.Context, host *[32]byte) error {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	hello, _ := json.Marshal(map[string]string{"type": "e2ee_hello", "publicKeyB64": base64.StdEncoding.EncodeToString(pub[:])})
	if err := c.ws.Write(ctx, websocket.MessageText, hello); err != nil {
		return closeError(err)
	}
	var ready struct{ Type string }
	if _, data, err := c.ws.Read(ctx); err != nil {
		return closeError(err)
	} else if json.Unmarshal(data, &ready) != nil || ready.Type != "e2ee_ready" {
		return errors.New("Orca gave a bad handshake reply.")
	}
	c.key = new([32]byte)
	box.Precompute(c.key, host, priv)
	auth, _ := json.Marshal(map[string]string{"type": "e2ee_auth", "deviceToken": c.token})
	if err := c.ws.Write(ctx, websocket.MessageText, c.seal(auth)); err != nil {
		return closeError(err)
	}
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return closeError(err)
	}
	plain, ok := c.open(data)
	if !ok {
		return errors.New("Orca refused the key of the pairing link. Pair again.")
	}
	var r struct {
		Type  string
		Error *Error
	}
	if json.Unmarshal(plain, &r) != nil || r.Type != "e2ee_authenticated" {
		if r.Error != nil {
			return fmt.Errorf("Orca refused the pairing: %s. Pair again.", r.Error.Code)
		}
		return errors.New("Orca refused the pairing. Pair again.")
	}
	return nil
}

// closeError names the close reason of Orca, for example Unauthorized.
func closeError(err error) error {
	var ce websocket.CloseError
	if errors.As(err, &ce) && ce.Reason != "" {
		return fmt.Errorf("Orca closed the connection: %s", ce.Reason)
	}
	return err
}

func (c *Conn) seal(plain []byte) []byte {
	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	out := box.SealAfterPrecomputation(nonce[:], plain, &nonce, c.key)
	return []byte(base64.StdEncoding.EncodeToString(out))
}

func (c *Conn) open(frame []byte) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(string(frame))
	if err != nil || len(b) < 24+box.Overhead {
		return nil, false
	}
	var nonce [24]byte
	copy(nonce[:], b)
	return box.OpenAfterPrecomputation(nil, b[24:], &nonce, c.key)
}

func (c *Conn) readLoop() {
	err := c.read()
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) read() error {
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			return closeError(err)
		}
		if typ != websocket.MessageText {
			continue // a binary frame is a stream that this client does not ask for
		}
		plain, ok := c.open(data)
		if !ok {
			return errors.New("Orca sent a frame that does not open.")
		}
		var r Reply
		if err := json.Unmarshal(plain, &r); err != nil {
			return errors.New("Orca sent a bad reply.")
		}
		if r.Keepalive {
			continue
		}
		c.mu.Lock()
		ch := c.pending[r.ID]
		if ch != nil && !r.Streaming {
			delete(c.pending, r.ID)
		}
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- r: // a slow stream holds the socket, not the memory
		case <-c.quit:
			return errors.New("The Orca connection is closed.")
		}
		if !r.Streaming {
			close(ch)
		}
	}
}

// Done closes when the socket ends. Err then tells why.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.quit)
		c.ws.CloseNow()
	})
}

func (c *Conn) send(ctx context.Context, method string, params any, buf int) (string, chan Reply, error) {
	id := strconv.FormatUint(c.next.Add(1), 10)
	req, err := json.Marshal(map[string]any{"id": id, "deviceToken": c.token, "method": method, "params": params})
	if err != nil {
		return "", nil, err
	}
	ch := make(chan Reply, buf)
	c.mu.Lock()
	if c.err != nil || isDone(c.done) {
		c.mu.Unlock()
		return "", nil, errors.New("The Orca connection is closed.")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	c.writeMu.Lock()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	err = c.ws.Write(wctx, websocket.MessageText, c.seal(req))
	cancel()
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return "", nil, err
	}
	return id, ch, nil
}

func isDone(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Call sends one request and decodes the result into out.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	id, ch, err := c.send(ctx, method, params, 1)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case r, ok := <-ch:
		if !ok {
			if err := c.Err(); err != nil {
				return err
			}
			return errors.New("The Orca connection is closed.")
		}
		if !r.OK {
			if r.Error != nil {
				return r.Error
			}
			return errors.New("Orca gave an error with no text.")
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

// Stream sends a streaming request. The channel closes after a reply that is not streaming, or when the socket ends.
// Use one socket for each stream: a slow reader holds the socket.
func (c *Conn) Stream(ctx context.Context, method string, params any) (<-chan Reply, error) {
	_, ch, err := c.send(ctx, method, params, streamBuffer)
	return ch, err
}
