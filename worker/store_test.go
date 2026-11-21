package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreReadsTheFolderOnce(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir).Save(Record{Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	b := NewStore(dir)
	if rec, err := b.Load("s1"); err != nil || rec.Session != "s1" {
		t.Fatalf("%+v %v", rec, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s2.json"), []byte(`{"session":"s2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if all, err := b.List(); err != nil || len(all) != 1 || all[0].Session != "s1" {
		t.Fatalf("%+v %v", all, err)
	}
}

func TestStoreLoadGivesACopy(t *testing.T) {
	s := NewStore(t.TempDir())
	in := Record{Session: "s1", Cursors: map[string]int{"a": 1}, Escalation: &Escalation{Question: "Q", Reason: ReasonBudget}}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	in.Cursors["a"] = 9
	in.Escalation.Question = "changed"
	check := func(step string) {
		t.Helper()
		rec, err := s.Load("s1")
		if err != nil || rec.Cursors["a"] != 1 || rec.Escalation.Question != "Q" {
			t.Fatalf("%s: %+v %v", step, rec, err)
		}
	}
	check("after change of the saved value")
	rec, _ := s.Load("s1")
	rec.Cursors["a"] = 7
	rec.Escalation.Question = "other"
	check("after change of the loaded value")
	all, _ := s.List()
	all[0].Cursors["a"] = 5
	check("after change of the listed value")
}

func TestStoreSaveErrorKeepsTheOldRecord(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a read-only folder")
	}
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(Record{Session: "s1", Note: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := s.Save(Record{Session: "s1", Note: "b"}); err == nil {
		t.Fatal("Save in a read-only folder must fail")
	}
	if rec, err := s.Load("s1"); err != nil || rec.Note != "a" {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestStoreBadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if all, err := s.List(); err != nil || len(all) != 0 {
		t.Fatalf("%+v %v", all, err)
	}
	if _, err := s.Load("s1"); err == nil || !strings.HasPrefix(err.Error(), "s1.json:") {
		t.Fatalf("%v", err)
	}
	if err := s.Save(Record{Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	if rec, err := s.Load("s1"); err != nil || rec.Session != "s1" {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestStoreListOrder(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, id := range []string{"b", "a", "a-b"} {
		if err := s.Save(Record{Session: id, Key: "work:X-1", UpdatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List()
	if err != nil || len(all) != 3 || all[0].Session != "a-b" || all[1].Session != "a" || all[2].Session != "b" {
		t.Fatalf("%+v %v", all, err)
	}
	if rec, ok, err := s.ByKey("work:X-1"); err != nil || !ok || rec.Session != "a-b" {
		t.Fatalf("%+v %v %v", rec, ok, err)
	}
}

func TestStoreBadSessionID(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("../x"); err == nil || !strings.Contains(err.Error(), "bad session id") {
		t.Fatalf("%v", err)
	}
	if err := s.Save(Record{Session: ".x"}); err == nil || !strings.Contains(err.Error(), "bad session id") {
		t.Fatalf("%v", err)
	}
}

func TestStoreConcurrentCalls(t *testing.T) {
	s := NewStore(t.TempDir())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("s%d", g%4)
			for i := 0; i < 200; i++ {
				if err := s.Save(Record{Session: id, Key: "work:" + id, Note: fmt.Sprint(g, ":", i), Cursors: map[string]int{"a": i}}); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Load(id); err != nil {
					t.Error(err)
				}
				if _, err := s.List(); err != nil {
					t.Error(err)
				}
				if _, _, err := s.ByKey("work:" + id); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("s%d", i)
		rec, err := s.Load(id)
		if err != nil || rec.Session != id || rec.Note == "" {
			t.Fatalf("%+v %v", rec, err)
		}
		disk, err := NewStore(s.dir).Load(id)
		if err != nil || disk.Note != rec.Note {
			t.Fatalf("memory %q, disk %q, %v", rec.Note, disk.Note, err)
		}
	}
}

func TestEngineRunWithRoutesHasNoRace(t *testing.T) {
	r := newRig(t, turnsTo(3), overseen("s1", "work:OVERSIRE-12"))
	r.e.Tick, r.e.Poll = time.Millisecond, time.Millisecond
	if err := r.store.Save(Record{Session: "s1", Key: "work:OVERSIRE-12", State: StateWorking, StartedAt: t0, Budget: DefaultBudget}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.e.Run(ctx); close(done) }()
	var wg sync.WaitGroup
	deadline := time.Now().Add(100 * time.Millisecond)
	for _, fn := range []func(){
		func() { r.e.Commit("s1", "overseer", 2) },
		func() { r.e.Wait(ctx, "s1", 5*time.Millisecond) },
		func() { r.e.Get("s1") },
		func() { r.e.Stop(ctx, "s1") },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				fn()
			}
		}()
	}
	wg.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
