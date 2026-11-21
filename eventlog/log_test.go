package eventlog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func ev(i int) wire.ProviderEvent { return wire.ProviderEvent{ID: fmt.Sprint("e", i), Schema: 1} }

func ids(p wire.EventsPage) []string {
	var s []string
	for _, e := range p.Events {
		s = append(s, e.ID)
	}
	return s
}

func TestPaging(t *testing.T) {
	l := New(10)
	if p := l.Page(0, 5); len(p.Events) != 0 || p.Head != 0 || p.Next != 0 || p.More || p.Truncated {
		t.Fatalf("empty %+v", p)
	}
	for i := 1; i <= 7; i++ {
		l.Append(ev(i))
	}
	p := l.Page(0, 5)
	if fmt.Sprint(ids(p)) != "[e1 e2 e3 e4 e5]" || !p.More || p.Next != 5 || p.Head != 7 || p.Truncated {
		t.Fatalf("first %+v", p)
	}
	p = l.Page(int(p.Next), 5)
	if fmt.Sprint(ids(p)) != "[e6 e7]" || p.More || p.Next != 7 {
		t.Fatalf("second %+v", p)
	}
	if p = l.Page(7, 5); len(p.Events) != 0 || p.Next != 7 {
		t.Fatalf("at head %+v", p)
	}
}

func TestTruncated(t *testing.T) {
	l := New(3)
	for i := 1; i <= 5; i++ {
		l.Append(ev(i))
	}
	p := l.Page(0, 10)
	if fmt.Sprint(ids(p)) != "[e3 e4 e5]" || !p.Truncated {
		t.Fatalf("%+v", p)
	}
	if p = l.Page(2, 10); p.Truncated || len(p.Events) != 3 {
		t.Fatalf("cursor at oldest-1 %+v", p)
	}
}

// After a restart the head is lower than the cursor of the client.
func TestEpochReset(t *testing.T) {
	a := New(10)
	for i := 1; i <= 4; i++ {
		a.Append(ev(i))
	}
	b := New(10)
	b.Append(ev(9))
	if a.Epoch() == b.Epoch() {
		t.Fatal("same epoch")
	}
	if p := b.Page(4, 10); p.Head >= 4 || len(p.Events) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestSubscribers(t *testing.T) {
	l := New(10)
	c, cancel := l.Subscribe()
	slow, cancelSlow := l.Subscribe()
	defer cancelSlow()
	for i := 1; i <= 3; i++ {
		l.Append(ev(i))
		select {
		case <-c:
		case <-time.After(time.Second):
			t.Fatal("no poke")
		}
	}
	// The slow subscriber never reads. Append must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			l.Append(ev(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("append blocks")
	}
	<-slow
	<-c
	cancel()
	l.Append(ev(0))
	select {
	case <-c:
		t.Fatal("poke after cancel")
	default:
	}
}

// blockKeeper holds each SaveHint until release closes. A nil release does not block.
type blockKeeper struct {
	release chan struct{}
	failAt  int64
	mu      sync.Mutex
	seqs    []int64
}

func (k *blockKeeper) SaveHint(_ context.Context, seq int64, _ wire.ProviderEvent) error {
	if k.release != nil {
		<-k.release
	}
	k.mu.Lock()
	k.seqs = append(k.seqs, seq)
	k.mu.Unlock()
	if seq == k.failAt {
		return errors.New("disk")
	}
	return nil
}

func (k *blockKeeper) Hints(context.Context, int) ([]Hint, error) { return nil, nil }

func (k *blockKeeper) saved() []int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int64(nil), k.seqs...)
}

func TestAppendDoesNotWaitForTheKeeper(t *testing.T) {
	k := &blockKeeper{release: make(chan struct{})}
	l := New(20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Keep(ctx, k); err != nil {
		t.Fatal(err)
	}
	go l.Run(ctx)
	done := make(chan struct{})
	go func() {
		for i := 1; i <= 10; i++ {
			l.Append(ev(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Append waits for the keeper")
	}
	if p := l.Page(0, 0); len(p.Events) != 10 {
		t.Fatalf("%+v", p)
	}
	close(k.release)
	deadline := time.Now().Add(time.Second)
	for len(k.saved()) < 10 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := k.saved(); !reflect.DeepEqual(got, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("%v", got)
	}
}

func TestRunWritesTheQueueAtStop(t *testing.T) {
	k := &blockKeeper{release: make(chan struct{})}
	l := New(20)
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Keep(ctx, k); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- l.Run(ctx) }()
	for i := 1; i <= 5; i++ {
		l.Append(ev(i))
	}
	cancel()
	close(k.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if got := k.saved(); len(got) != 5 {
		t.Fatalf("%v", got)
	}
}

func TestFullHintQueueDrops(t *testing.T) {
	l := New(10)
	if err := l.Keep(context.Background(), &blockKeeper{}); err != nil {
		t.Fatal(err)
	}
	l.keep = make(chan Hint, 2)
	for i := 1; i <= 3; i++ {
		l.Append(ev(i))
	}
	if len(l.keep) != 2 || len(l.Page(0, 0).Events) != 3 || l.Head() != 3 {
		t.Fatalf("queue %d head %d", len(l.keep), l.Head())
	}
}

func TestAppendWithoutKeeperQueuesNothing(t *testing.T) {
	l := New(10)
	for i := 1; i <= 3; i++ {
		l.Append(ev(i))
	}
	if len(l.keep) != 0 {
		t.Fatal(len(l.keep))
	}
}

func TestKeeperErrorDoesNotStopRun(t *testing.T) {
	k := &blockKeeper{failAt: 2}
	l := New(10)
	if err := l.Keep(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		l.Append(ev(i))
	}
	l.Flush(context.Background())
	if got := k.saved(); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("%v", got)
	}
}
