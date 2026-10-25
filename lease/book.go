// Package lease keeps the named leases of the home daemon. The apps of the group ask the home for the
// automations lease, so one app runs the automatic actions (OVERSIRE-319).
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

var (
	ErrHeld      = errors.New("another holder has the lease")
	ErrNotHolder = errors.New("the caller does not hold the lease")
	ErrBadInput  = errors.New("the lease request is not valid")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Book holds one lease for each name. It saves to a file, so a restart keeps the holder until Until.
// A released or expired lease stays in the book with its epoch, so the epoch never goes back.
type Book struct {
	path   string
	clock  func() time.Time
	mu     sync.Mutex
	byName map[string]wire.Lease

	// OnChange gets the lease after a new holder or a release. A renew does not call it.
	OnChange func(wire.Lease)
}

// NewBook reads the book at path. A missing or bad file gives an empty book.
func NewBook(path string, clock func() time.Time) *Book {
	b := &Book{path: path, clock: clock, byName: map[string]wire.Lease{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &b.byName)
	}
	return b
}

// Acquire gives or renews the lease. ErrHeld comes with the current lease when another holder has it and r.Take is false.
// A renew by the holder keeps the epoch. A new holder increases it.
func (b *Book) Acquire(name string, r wire.LeaseRequest) (wire.Lease, error) {
	if err := check(name, r); err != nil {
		return wire.Lease{}, err
	}
	b.mu.Lock()
	now := b.clock()
	cur, ok := b.byName[name]
	if ok && cur.Holder != r.Holder && now.Before(cur.Until) && !r.Take {
		b.mu.Unlock()
		return cur, ErrHeld
	}
	next := wire.Lease{Name: name, Holder: r.Holder, HolderName: r.HolderName, Epoch: cur.Epoch, Until: now.Add(time.Duration(r.TTLSeconds) * time.Second).UTC()}
	moved := !ok || cur.Holder != r.Holder
	if moved {
		next.Epoch++
	}
	err := b.setLocked(next)
	b.mu.Unlock()
	if err != nil {
		return wire.Lease{}, err
	}
	if moved {
		b.changed(next)
	}
	return next, nil
}

// Release ends the lease of holder at once. A lease that is free or expired gives no error.
func (b *Book) Release(name, holder string) error {
	b.mu.Lock()
	now := b.clock()
	cur, ok := b.byName[name]
	if !ok || !now.Before(cur.Until) {
		b.mu.Unlock()
		return nil
	}
	if cur.Holder != holder {
		b.mu.Unlock()
		return ErrNotHolder
	}
	cur.Until = now.UTC()
	err := b.setLocked(cur)
	b.mu.Unlock()
	if err == nil {
		b.changed(cur)
	}
	return err
}

// Get gives the lease when a holder has it now.
func (b *Book) Get(name string) (wire.Lease, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.byName[name]
	return l, ok && b.clock().Before(l.Until)
}

func (b *Book) setLocked(l wire.Lease) error {
	b.byName[l.Name] = l
	return config.WriteJSON(b.path, b.byName)
}

func (b *Book) changed(l wire.Lease) {
	if b.OnChange != nil {
		b.OnChange(l)
	}
}

func check(name string, r wire.LeaseRequest) error {
	switch {
	case !namePattern.MatchString(name):
		return fmt.Errorf("%w: name %q", ErrBadInput, name)
	case r.Holder == "" || len(r.Holder) > 200 || r.HolderName == "" || len(r.HolderName) > 200:
		return fmt.Errorf("%w: holder and holderName need 1 to 200 characters", ErrBadInput)
	case r.TTLSeconds < wire.LeaseMinTTL || r.TTLSeconds > wire.LeaseMaxTTL:
		return fmt.Errorf("%w: ttlSeconds must be %d to %d", ErrBadInput, wire.LeaseMinTTL, wire.LeaseMaxTTL)
	}
	return nil
}
