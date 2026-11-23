package link

import (
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oversire/cli/server"
)

// Presence is the last report of one app: when the user last used its Mac.
type Presence struct {
	Machine, Name, Device string
	Loopback              bool
	Addr                  netip.Addr
	InputAt, SeenAt       time.Time
}

// Match finds the WebSocket of the app of this presence.
func (p Presence) Match(c server.Caller) bool {
	if p.Loopback {
		return c.Loopback
	}
	return c.DeviceID == p.Device && !c.IsDaemon()
}

// Book holds the presence of each app in memory.
type Book struct {
	now func() time.Time

	mu  sync.Mutex
	all map[string]Presence
}

func NewBook(now func() time.Time) *Book {
	return &Book{now: now, all: map[string]Presence{}}
}

// Report keeps the presence of the caller. self is the machine of this daemon, for a loopback caller.
func (b *Book) Report(c server.Caller, self string, idle time.Duration) {
	now := b.now()
	p := Presence{Machine: c.Machine, Name: c.Name, Device: c.DeviceID, Addr: c.Addr, InputAt: now.Add(-idle), SeenAt: now}
	key := c.DeviceID
	if c.Loopback {
		key, p.Loopback, p.Machine, p.Device = "loopback", true, self, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.all[key] = p
}

// All gives the presences, the latest input first.
func (b *Book) All() []Presence {
	b.mu.Lock()
	out := make([]Presence, 0, len(b.all))
	for _, p := range b.all {
		out = append(out, p)
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(a, b Presence) int {
		if c := b.InputAt.Compare(a.InputAt); c != 0 {
			return c
		}
		if c := b.SeenAt.Compare(a.SeenAt); c != 0 {
			return c
		}
		return strings.Compare(a.Machine, b.Machine)
	})
	return out
}
