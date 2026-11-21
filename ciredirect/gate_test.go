package ciredirect

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGateRefuse(t *testing.T) {
	g := NewGate()
	if reason, ok := g.Refuse("Bash", json.RawMessage(`{"command":"gh run watch 9"}`)); !ok || !strings.Contains(reason, "oversire ci wait 9") {
		t.Fatalf("%v %q", ok, reason)
	}
	for _, c := range []struct{ tool, in string }{
		{"Read", `{"command":"gh run watch 9"}`},
		{"Bash", `{`},
		{"Bash", `{"command":"go test ./..."}`},
		{"Bash", `null`},
	} {
		if _, ok := g.Refuse(c.tool, json.RawMessage(c.in)); ok {
			t.Errorf("refused %s %s", c.tool, c.in)
		}
	}
}
