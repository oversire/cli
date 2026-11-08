package ticket

import (
	"reflect"
	"testing"
	"time"
)

func TestNormalKey(t *testing.T) {
	if k, err := NormalKey(" oversire-12 "); err != nil || k != "OVERSIRE-12" {
		t.Fatal(k, err)
	}
	for _, bad := range []string{"", "12", "OVERSIRE", "../X-1", "X-1/2"} {
		if _, err := NormalKey(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestLedgerAddSessionsItems(t *testing.T) {
	l := NewLedger(t.TempDir())
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	l.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "s2", Role: "work", At: t0.Add(time.Hour)})
	l.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Role: "new", At: t0})
	l.Add(SessionRecord{Key: "B-2", Harness: "claude", Session: "s1", Role: "new", At: t0})
	got, err := l.Sessions("A-1")
	if err != nil || len(got) != 2 || got[0].Session != "s1" || got[1].Session != "s2" {
		t.Fatal(got, err)
	}
	items, _ := l.Items("s1")
	if !reflect.DeepEqual(items, []string{"A-1", "B-2"}) {
		t.Fatal(items)
	}
	if none, _ := l.Sessions("C-3"); len(none) != 0 {
		t.Fatal(none)
	}
}

func TestLedgerAddSameSessionUpdates(t *testing.T) {
	l := NewLedger(t.TempDir())
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	l.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Role: "work", At: t0})
	l.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Role: "work", At: t0.Add(time.Minute)})
	got, _ := l.Sessions("A-1")
	if len(got) != 1 || !got[0].At.Equal(t0.Add(time.Minute)) {
		t.Fatal(got)
	}
}

func TestLedgerClose(t *testing.T) {
	l := NewLedger(t.TempDir())
	l.Add(SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Role: "new", At: time.Now()})
	if err := l.Close("A-1", "s1"); err != nil {
		t.Fatal(err)
	}
	if items, _ := l.Items("s1"); len(items) != 0 {
		t.Fatal(items)
	}
	got, _ := l.Sessions("A-1")
	if !got[0].Done {
		t.Fatal(got)
	}
}
