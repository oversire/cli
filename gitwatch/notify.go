package gitwatch

import (
	"context"
	"time"
)

// pollNotifier wakes the watcher each 2 s, with no file events. A read only stats the reflog files.
type pollNotifier struct{ ch chan struct{} }

func (t *pollNotifier) Changed() <-chan struct{} { return t.ch }
func (t *pollNotifier) Watch([]string)           {}

func (t *pollNotifier) Run(ctx context.Context) {
	tk := time.NewTicker(debounce)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			select {
			case t.ch <- struct{}{}:
			default:
			}
		}
	}
}
