package lease

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) add(d time.Duration)     { c.now = c.now.Add(d) }
func req(holder string, take bool) wire.LeaseRequest {
	return wire.LeaseRequest{Holder: holder, HolderName: "m-" + holder, TTLSeconds: 90, Take: take}
}

func newBook(t *testing.T) (*Book, *fakeClock, string) {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "leases.json")
	return NewBook(path, c.Now), c, path
}

func TestAcquireRenewAndHeld(t *testing.T) {
	b, c, _ := newBook(t)
	var changes []wire.Lease
	b.OnChange = func(l wire.Lease) { changes = append(changes, l) }

	l, err := b.Acquire("automations", req("a", false))
	if err != nil || l.Holder != "a" || l.Epoch != 1 || !l.Until.Equal(c.now.Add(90*time.Second)) {
		t.Fatalf("acquire: %+v %v", l, err)
	}
	c.add(30 * time.Second)
	if l, err = b.Acquire("automations", req("a", false)); err != nil || l.Epoch != 1 || !l.Until.Equal(c.now.Add(90*time.Second)) {
		t.Fatalf("renew: %+v %v", l, err)
	}
	if l, err = b.Acquire("automations", req("b", false)); !errors.Is(err, ErrHeld) || l.Holder != "a" {
		t.Fatalf("other holder: %+v %v", l, err)
	}
	if len(changes) != 1 {
		t.Fatalf("a renew must not notify: %d changes", len(changes))
	}
}

func TestExpiryAndTake(t *testing.T) {
	b, c, _ := newBook(t)
	_, _ = b.Acquire("automations", req("a", false))
	c.add(91 * time.Second)
	if _, ok := b.Get("automations"); ok {
		t.Fatal("an expired lease has no holder")
	}
	l, err := b.Acquire("automations", req("b", false))
	if err != nil || l.Holder != "b" || l.Epoch != 2 {
		t.Fatalf("after until: %+v %v", l, err)
	}
	if l, err = b.Acquire("automations", req("a", true)); err != nil || l.Holder != "a" || l.Epoch != 3 {
		t.Fatalf("take: %+v %v", l, err)
	}
}

func TestRelease(t *testing.T) {
	b, c, _ := newBook(t)
	_, _ = b.Acquire("automations", req("a", false))
	if err := b.Release("automations", "b"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("release by another holder: %v", err)
	}
	if err := b.Release("automations", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("automations"); ok {
		t.Fatal("a released lease has no holder")
	}
	c.add(time.Second)
	if l, err := b.Acquire("automations", req("b", false)); err != nil || l.Epoch != 2 {
		t.Fatalf("the epoch must not go back after a release: %+v %v", l, err)
	}
	if err := b.Release("other", "x"); err != nil {
		t.Fatalf("a free lease: %v", err)
	}
}

func TestRestartKeepsHolder(t *testing.T) {
	b, c, path := newBook(t)
	_, _ = b.Acquire("automations", req("a", false))
	again := NewBook(path, c.Now)
	l, ok := again.Get("automations")
	if !ok || l.Holder != "a" || l.Epoch != 1 {
		t.Fatalf("after restart: %+v %v", l, ok)
	}
	if _, err := again.Acquire("automations", req("b", false)); !errors.Is(err, ErrHeld) {
		t.Fatalf("after restart another holder: %v", err)
	}
}

func TestBadRequests(t *testing.T) {
	b, _, _ := newBook(t)
	for _, tc := range []struct {
		name string
		r    wire.LeaseRequest
	}{
		{"Automations", req("a", false)},
		{"automations", wire.LeaseRequest{HolderName: "x", TTLSeconds: 90}},
		{"automations", wire.LeaseRequest{Holder: "a", HolderName: "x", TTLSeconds: 10}},
		{"automations", wire.LeaseRequest{Holder: "a", HolderName: "x", TTLSeconds: 601}},
	} {
		if _, err := b.Acquire(tc.name, tc.r); !errors.Is(err, ErrBadInput) {
			t.Errorf("%s %+v: %v", tc.name, tc.r, err)
		}
	}
}
