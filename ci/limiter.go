package ci

import (
	"sync"
	"time"
)

// Limiter lets one fresh read through for each key within a window.
type Limiter struct {
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

func NewLimiter(every time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{every: every, now: now, last: map[string]time.Time{}}
}

// Allow records a read for key when the window is free. Else it gives the time left in the window.
func (l *Limiter) Allow(key string) (retry time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, t := range l.last {
		if now.Sub(t) >= l.every {
			delete(l.last, k)
		}
	}
	if t, found := l.last[key]; found {
		return l.every - now.Sub(t), false
	}
	l.last[key] = now
	return 0, true
}

// Forget frees the window of key. A read that failed does not count.
func (l *Limiter) Forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, key)
}

// Window is the length of the window.
func (l *Limiter) Window() time.Duration { return l.every }
