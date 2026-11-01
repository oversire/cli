//go:build !darwin

package gitwatch

func newNotifier() (notifier, error) { return &pollNotifier{ch: make(chan struct{}, 1)}, nil }
