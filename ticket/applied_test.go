package ticket

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func TestAppliedDrafts(t *testing.T) {
	lines := []AppliedLine{
		{Item: "trebi-12", Summary: "Merged into TREBI-10", Payload: json.RawMessage(`{"kind":"merge","keep":"TREBI-10"}`)},
		{Item: "TREBI-14", Parent: "trebi-2", Summary: "Done: shipped", Payload: json.RawMessage(`{"kind":"done"}`)},
	}
	a := Actor{Harness: "claude", Session: "s1", Machine: "mbp"}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	rows, err := AppliedDrafts(wire.KindGroomApplied, "TREBI", lines, a, now)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	r := rows[0]
	if r.ID != "cli:groom:s1:TREBI-12:merge" || r.ActorType != wire.ActorAgent || r.Actor != "claude" || r.Via != "cli" ||
		r.Item != "TREBI-12" || r.Project != "trebi" || r.Kind != "groom.applied" {
		t.Fatalf("%+v", r)
	}
	if rows[1].ParentItem != "TREBI-2" || rows[1].ID != "cli:groom:s1:TREBI-14:done" {
		t.Fatalf("%+v", rows[1])
	}
	again, _ := AppliedDrafts(wire.KindGroomApplied, "TREBI", lines, a, now.Add(time.Hour))
	if again[0].ID != r.ID || again[1].ID != rows[1].ID {
		t.Fatal("a retry gives a new id")
	}
	for name, ls := range map[string][]AppliedLine{
		"long summary": {{Item: "A-1", Summary: strings.Repeat("x", 201)}},
		"bad key":      {{Item: "nokey", Summary: "x"}},
	} {
		if _, err := AppliedDrafts(wire.KindGroomApplied, "A", ls, a, now); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := AppliedDrafts("x.applied", "A", lines, a, now); err == nil {
		t.Error("x.applied: no error")
	}
}
