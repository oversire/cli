package gitwatch

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// kqueue tells when an entry of a watched folder changes.
type kqueue struct {
	fd  int
	mu  sync.Mutex
	fds map[string]int
	ch  chan struct{}
}

func newNotifier() (notifier, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	return &kqueue{fd: fd, fds: map[string]int{}, ch: make(chan struct{}, 1)}, nil
}

func (k *kqueue) Changed() <-chan struct{} { return k.ch }

// Watch replaces the set of watched folders.
func (k *kqueue) Watch(dirs []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	want := map[string]bool{}
	for _, d := range dirs {
		want[d] = true
		if _, ok := k.fds[d]; ok {
			continue
		}
		fd, err := unix.Open(d, unix.O_EVTONLY, 0)
		if err != nil {
			continue
		}
		ev := unix.Kevent_t{}
		unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
		ev.Fflags = unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_EXTEND
		if _, err := unix.Kevent(k.fd, []unix.Kevent_t{ev}, nil, nil); err != nil {
			unix.Close(fd)
			continue
		}
		k.fds[d] = fd
	}
	for d, fd := range k.fds {
		if !want[d] {
			unix.Close(fd) // closing the fd removes its kevent
			delete(k.fds, d)
		}
	}
}

func (k *kqueue) Run(ctx context.Context) {
	events := make([]unix.Kevent_t, 32)
	wait := unix.NsecToTimespec(int64(time.Second))
	for ctx.Err() == nil {
		n, err := unix.Kevent(k.fd, nil, events, &wait)
		if err != nil && err != unix.EINTR {
			return
		}
		if n > 0 {
			select {
			case k.ch <- struct{}{}:
			default:
			}
		}
	}
	k.mu.Lock()
	for _, fd := range k.fds {
		unix.Close(fd)
	}
	k.fds = map[string]int{}
	k.mu.Unlock()
	unix.Close(k.fd)
}
