// Package orcawstest is a fake Orca runtime WebSocket for tests: the NaCl box handshake, then sealed requests.
package orcawstest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"golang.org/x/crypto/nacl/box"

	"github.com/oversire/cli/runtime/orca/orcaws"
)

// Request is one request of a client.
type Request struct {
	ID          string          `json:"id"`
	DeviceToken string          `json:"deviceToken"`
	Method      string          `json:"method"`
	Params      json.RawMessage `json:"params"`
}

// Server accepts the device token Token. Handle answers each request; nil answers status.get only.
type Server struct {
	URL    string
	Token  string
	Status orcaws.Status
	Handle func(p *Peer, r Request)

	srv  *httptest.Server
	pub  *[32]byte
	priv *[32]byte

	mu    sync.Mutex
	peers []*Peer
}

func New() *Server {
	pub, priv, _ := box.GenerateKey(rand.Reader)
	s := &Server{Token: "device-token", pub: pub, priv: priv,
		Status: orcaws.Status{ProtocolVersion: 3, MinClientVersion: 2, Capabilities: []string{"terminal.binary-stream.v1"}}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = "ws" + strings.TrimPrefix(s.srv.URL, "http")
	return s
}

func (s *Server) Close() {
	s.mu.Lock()
	for _, p := range s.peers {
		p.ws.CloseNow()
	}
	s.mu.Unlock()
	s.srv.Close()
}

// Offer is a runtime offer for this server.
func (s *Server) Offer() orcaws.Offer {
	return orcaws.Offer{V: orcaws.OfferVersion, Endpoint: s.URL, DeviceToken: s.Token,
		PublicKey: base64.StdEncoding.EncodeToString(s.pub[:]), Scope: "runtime"}
}

// Link is the orca://pair link of the offer.
func (s *Server) Link() string { return "orca://pair?code=" + s.Offer().Code() }

// Peers gives the sockets that passed the handshake.
func (s *Server) Peers() []*Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Peer(nil), s.peers...)
}

// Peer is one client socket.
type Peer struct {
	ws  *websocket.Conn
	key *[32]byte
	mu  sync.Mutex
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx := context.Background()
	_, data, err := ws.Read(ctx)
	if err != nil {
		return
	}
	var hello struct{ Type, PublicKeyB64 string }
	b, _ := base64.StdEncoding.DecodeString(func() string { _ = json.Unmarshal(data, &hello); return hello.PublicKeyB64 }())
	if hello.Type != "e2ee_hello" || len(b) != 32 {
		ws.Close(4001, "Invalid e2ee_hello")
		return
	}
	var client [32]byte
	copy(client[:], b)
	p := &Peer{ws: ws, key: new([32]byte)}
	box.Precompute(p.key, &client, s.priv)
	_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"e2ee_ready"}`))
	_, data, err = ws.Read(ctx)
	if err != nil {
		return
	}
	var auth struct{ Type, DeviceToken string }
	if plain, ok := p.open(data); !ok || json.Unmarshal(plain, &auth) != nil || auth.DeviceToken != s.Token {
		p.send(map[string]any{"type": "e2ee_error", "error": map[string]string{"code": "unauthorized"}})
		ws.Close(4001, "Unauthorized")
		return
	}
	p.send(map[string]string{"type": "e2ee_authenticated"})
	s.mu.Lock()
	s.peers = append(s.peers, p)
	s.mu.Unlock()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		plain, ok := p.open(data)
		var req Request
		if !ok || json.Unmarshal(plain, &req) != nil || req.DeviceToken != s.Token {
			ws.Close(4003, "bad request")
			return
		}
		switch {
		case req.Method == "status.get":
			p.Reply(req.ID, s.Status, false)
		case s.Handle != nil:
			go s.Handle(p, req)
		default:
			p.Fail(req.ID, "method_not_found", "no such method")
		}
	}
}

func (p *Peer) open(frame []byte) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(string(frame))
	if err != nil || len(b) < 24+box.Overhead {
		return nil, false
	}
	var nonce [24]byte
	copy(nonce[:], b)
	return box.OpenAfterPrecomputation(nil, b[24:], &nonce, p.key)
}

func (p *Peer) send(v any) {
	plain, _ := json.Marshal(v)
	var nonce [24]byte
	_, _ = rand.Read(nonce[:])
	out := box.SealAfterPrecomputation(nonce[:], plain, &nonce, p.key)
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.ws.Write(context.Background(), websocket.MessageText, []byte(base64.StdEncoding.EncodeToString(out)))
}

// Reply sends a result. A streaming reply keeps the request open.
func (p *Peer) Reply(id string, result any, streaming bool) {
	p.send(map[string]any{"id": id, "ok": true, "result": result, "streaming": streaming, "_meta": map[string]string{"runtimeId": "fake"}})
}

func (p *Peer) Fail(id, code, msg string) {
	p.send(map[string]any{"id": id, "ok": false, "error": map[string]string{"code": code, "message": msg}})
}

// Keepalive sends the keepalive frame of Orca.
func (p *Peer) Keepalive() { p.send(map[string]bool{"_keepalive": true}) }

// Drop closes the socket.
func (p *Peer) Drop() { p.ws.CloseNow() }
