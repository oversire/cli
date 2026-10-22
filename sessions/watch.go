package sessions

import "sync"

// watchers sends the id of each session whose state or reason changes, or that goes away.
// A send never blocks the book: a full channel drops the id.
type watchers struct {
	mu   sync.Mutex
	subs map[int]chan string
	next int
}

func (w *watchers) notify(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.subs {
		select {
		case c <- id:
		default:
		}
	}
}

// Watch gives a channel that gets the id of each session whose state or reason changes, or that goes away, and a cancel func.
// The channel drops ids when it is full, so a watcher also checks the sessions now and then.
func (b *Book) Watch() (<-chan string, func()) {
	w := &b.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs == nil {
		w.subs = map[int]chan string{}
	}
	n := w.next
	w.next++
	c := make(chan string, 64)
	w.subs[n] = c
	return c, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.subs, n)
	}
}
