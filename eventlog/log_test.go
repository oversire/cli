package eventlog

import (
	"fmt"
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
