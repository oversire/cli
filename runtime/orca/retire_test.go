package orca

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A journal of the old workers book: one running worker, one settled, one with a terminal that is gone.
const journal = `{
	"w1": {"worker": {"id": "w1", "runtime": "orca"}, "handle": {"dispatch": "ctx_1", "terminal": "term_live"}},
	"w2": {"worker": {"id": "w2", "runtime": "orca"}, "handle": {"dispatch": "ctx_2", "terminal": "term_done"}, "settled": {"state": "done"}},
	"w3": {"worker": {"id": "w3", "runtime": "orca"}, "handle": {"dispatch": "ctx_3", "terminal": "term_gone"}, "settled": null}}`

const coordTerminals = `{"ok":true,"result":{"terminals":[
	{"handle":"term_old","orphaned":true,"title":"oversire-coordinator"},
	{"handle":"term_coord","orphaned":false,"title":"oversire-coordinator"},
	{"handle":"term_item","orphaned":false,"title":"◐ OVERSIRE-178"}]}}`

const (
	closed       = `{"ok":true,"result":{"closed":true}}`
	notFound     = `{"ok":false,"error":{"code":"terminal_not_found","message":"no terminal"}}`
	closeRefused = `{"ok":false,"error":{"code":"runtime_unavailable","message":"Orca is not running"}}`
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func closedTerminals(r *replay) []string {
	var out []string
	for _, a := range r.args("terminal close") {
		out = append(out, a[3])
	}
	slices.Sort(out)
	return out
}

func writeJournal(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "workers.json")
	if err := os.WriteFile(p, []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRetireWorkersClosesTheOpenWorkersAndTheCoordinator(t *testing.T) {
	o, r := setup()
	p := writeJournal(t)
	r.on("terminal close", closed)
	r.on("terminal list", coordTerminals)
	o.RetireWorkers(context.Background(), p, quiet)
	// w3 has a null settled field, so it is open.
	if got := closedTerminals(r); !slices.Equal(got, []string{"term_coord", "term_gone", "term_live"}) {
		t.Fatal(got)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the journal is still there", err)
	}
}

func TestRetireWorkersDeletesTheJournalWhenATerminalIsGone(t *testing.T) {
	o, r := setup()
	p := writeJournal(t)
	r.on("terminal close", notFound)
	r.on("terminal list", `{"ok":true,"result":{"terminals":[]}}`)
	o.RetireWorkers(context.Background(), p, quiet)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the journal is still there", err)
	}
}

func TestRetireWorkersKeepsTheJournalWhenACloseFails(t *testing.T) {
	o, r := setup()
	p := writeJournal(t)
	r.on("terminal close", closeRefused)
	r.on("terminal list", coordTerminals)
	o.RetireWorkers(context.Background(), p, quiet)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the journal is gone", err)
	}
}

func TestRetireWorkersWithNoJournal(t *testing.T) {
	o, r := setup()
	r.on("terminal close", closed)
	r.on("terminal list", coordTerminals)
	o.RetireWorkers(context.Background(), filepath.Join(t.TempDir(), "workers.json"), quiet)
	if got := closedTerminals(r); !slices.Equal(got, []string{"term_coord"}) {
		t.Fatal(got)
	}
}
