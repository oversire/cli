package link

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/server"
)

// SelfNode gives this tailnet node (server.Tailnet).
type SelfNode interface {
	Self(ctx context.Context) (server.Peer, error)
}

const webEvery = 5 * time.Minute

// WebBase is the base of the web links of this daemon: its tailnet DNS name, else its host name.
type WebBase struct {
	self     SelfNode
	port     int
	tailnet  bool
	hostname func() (string, error)

	mu   sync.Mutex
	base string
}

func NewWebBase(t SelfNode, port int, tailnet bool, hostname func() (string, error)) *WebBase {
	w := &WebBase{self: t, port: port, tailnet: tailnet, hostname: hostname}
	w.base = w.fallback()
	return w
}

// Get gives the last value. It does not block.
func (w *WebBase) Get() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.base
}

// Run reads the tailnet name at the start and each 5 minutes.
func (w *WebBase) Run(ctx context.Context) error {
	t := time.NewTicker(webEvery)
	defer t.Stop()
	for {
		w.refresh(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (w *WebBase) refresh(ctx context.Context) {
	base := w.fallback()
	if w.tailnet && w.self != nil {
		if p, err := w.self.Self(ctx); err == nil && p.DNS != "" {
			base = w.url(p.DNS)
		}
	}
	w.mu.Lock()
	w.base = base
	w.mu.Unlock()
}

func (w *WebBase) fallback() string {
	if w.tailnet {
		if h, err := w.hostname(); err == nil && h != "" {
			short, _, _ := strings.Cut(h, ".")
			return w.url(strings.ToLower(short))
		}
	}
	return w.url("127.0.0.1")
}

func (w *WebBase) url(host string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(w.port))
}
