package term

import (
	"sync"
	"time"

	"github.com/oversire/cli/wire"
)

// LeaseIdle is how long a holder keeps the size with no input.
const LeaseIdle = 10 * time.Minute

// holderSink gets the holder as the member sees it.
type holderSink interface{ leaseChanged(wire.LeaseHolder) }

// Leases keeps the one size holder of each session.
type Leases struct {
	mu  sync.Mutex
	now func() time.Time
	by  map[string]*lease
}

type lease struct {
	members   map[holderSink]struct{}
	holder    holderSink // nil: none or desktop
	desktop   bool
	lastInput time.Time
}

func NewLeases(now func() time.Time) *Leases {
	return &Leases{now: now, by: map[string]*lease{}}
}

func (l *Leases) join(session string, m holderSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls := l.by[session]
	if ls == nil {
		ls = &lease{members: map[holderSink]struct{}{}}
		l.by[session] = ls
	}
	ls.members[m] = struct{}{}
	m.leaseChanged(ls.holderFor(m))
}

// leave tells if m held the size.
func (l *Leases) leave(session string, m holderSink) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls := l.by[session]
	if ls == nil {
		return false
	}
	delete(ls.members, m)
	held := ls.holder == m
	if held {
		ls.holder = nil
		ls.notify()
	}
	if len(ls.members) == 0 {
		delete(l.by, session)
	}
	return held
}

func (l *Leases) take(session string, m holderSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ls := l.by[session]; ls != nil {
		changed := ls.holder != m
		ls.holder, ls.desktop, ls.lastInput = m, false, l.now()
		if changed {
			ls.notify()
		}
	}
}

// release tells if m held the size.
func (l *Leases) release(session string, m holderSink) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls := l.by[session]
	if ls == nil || ls.holder != m {
		return false
	}
	ls.holder = nil
	ls.notify()
	return true
}

// takeBack is the runtime UI that took the size.
func (l *Leases) takeBack(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ls := l.by[session]; ls != nil && !ls.desktop {
		ls.holder, ls.desktop = nil, true
		ls.notify()
	}
}

func (l *Leases) touch(session string, m holderSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ls := l.by[session]; ls != nil && ls.holder == m {
		ls.lastInput = l.now()
	}
}

// expired releases the size of m after LeaseIdle with no input, and tells if it did.
func (l *Leases) expired(session string, m holderSink) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ls := l.by[session]
	if ls == nil || ls.holder != m || l.now().Sub(ls.lastInput) < LeaseIdle {
		return false
	}
	ls.holder = nil
	ls.notify()
	return true
}

func (ls *lease) holderFor(m holderSink) wire.LeaseHolder {
	switch {
	case ls.holder == m:
		return wire.HolderYou
	case ls.holder != nil:
		return wire.HolderOther
	case ls.desktop:
		return wire.HolderDesktop
	}
	return wire.HolderNone
}

func (ls *lease) notify() {
	for m := range ls.members {
		m.leaseChanged(ls.holderFor(m))
	}
}
