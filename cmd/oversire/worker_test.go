package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

// workerDaemon serves the read and cursor routes of a worker. status is the code of the read.
type workerDaemon struct {
	mu     sync.Mutex
	status int
	read   worker.Read
	query  string
	puts   []string
	cursor []worker.Cursor
}

func (w *workerDaemon) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/workers/OVERSIRE-12/read":
		w.query = r.URL.RawQuery
		if w.status != 0 {
			rw.WriteHeader(w.status)
			json.NewEncoder(rw).Encode(map[string]string{"error": "The read failed."})
			return
		}
		json.NewEncoder(rw).Encode(w.read)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/workers/OVERSIRE-12/cursors/"):
		var c worker.Cursor
		json.NewDecoder(r.Body).Decode(&c)
		w.puts = append(w.puts, r.URL.Path)
		w.cursor = append(w.cursor, c)
		rw.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(rw, r)
	}
}

func sampleRead() worker.Read {
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	return worker.Read{
		Header: worker.Header{Session: "s1", Key: "work:OVERSIRE-12", Harness: "codex", State: wire.PhaseWorking, First: 4, Last: 5},
		Turns: []wire.SessionTurn{
			{Seq: 4, Role: wire.RoleUser, At: at, Text: "Run the tests.", Origin: wire.OriginOverseer},
			{Seq: 5, Role: wire.RoleAssistant, At: at, Text: "I run them now."},
		},
		Next: 5,
	}
}

func TestWorkerReadPrintsTheFoldAndCommitsTheCursor(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12", "--reader", "main", "--limit", "50"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "work:OVERSIRE-12 | codex | working") || !strings.Contains(got, "turns 4-5") ||
		!strings.Contains(got, "[overseer] Run the tests.\n") || !strings.Contains(got, "I run them now.\n") {
		t.Fatalf("output:\n%s", got)
	}
	if d.query != "limit=50&reader=main" {
		t.Fatalf("query %q", d.query)
	}
	if len(d.puts) != 1 || d.puts[0] != "/v1/workers/OVERSIRE-12/cursors/main" || d.cursor[0].Seq != 5 {
		t.Fatalf("puts %v %v", d.puts, d.cursor)
	}
}

func TestWorkerReadUsesTheOverseerReaderByDefault(t *testing.T) {
	c, _, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12"}); err != nil {
		t.Fatal(err)
	}
	if d.query != "reader=overseer" || len(d.puts) != 1 || d.puts[0] != "/v1/workers/OVERSIRE-12/cursors/overseer" {
		t.Fatalf("%q %v", d.query, d.puts)
	}
}

func TestWorkerReadJSONPrintsTheRawTurns(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{read: sampleRead()}
	fakeDaemon(t, c, d)
	if err := c.worker(context.Background(), []string{"read", "OVERSIRE-12", "--json"}); err != nil {
		t.Fatal(err)
	}
	var turns []wire.SessionTurn
	if err := json.Unmarshal(out.Bytes(), &turns); err != nil || len(turns) != 2 || turns[0].Origin != wire.OriginOverseer || turns[1].Seq != 5 {
		t.Fatalf("%v %s", err, out)
	}
	if len(d.puts) != 1 {
		t.Fatalf("puts %v", d.puts)
	}
}

func TestWorkerReadDoesNotCommitOnError(t *testing.T) {
	c, out, _ := testCLI(t, "")
	d := &workerDaemon{status: http.StatusInternalServerError}
	fakeDaemon(t, c, d)
	err := c.worker(context.Background(), []string{"read", "OVERSIRE-12"})
	var de *daemonError
	if !errors.As(err, &de) || de.Status != 500 || len(d.puts) != 0 || out.Len() != 0 {
		t.Fatalf("%v %v %q", err, d.puts, out)
	}
}

func TestWorkerUsage(t *testing.T) {
	c, _, _ := testCLI(t, "")
	for _, args := range [][]string{nil, {"nope"}, {"read"}, {"read", "A", "B"}, {"read", "A", "--limit", "-1"}, {"read", "A", "--reader", ""}} {
		var e *exitError
		if err := c.worker(context.Background(), args); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
