package ciredirect

import "encoding/json"

// Gate refuses the shell calls that poll CI. It implements hooks.Gate.
type Gate struct{ rules Rules }

func NewGate() *Gate { return &Gate{} }

// Refuse gives the reason for a Bash call that polls CI. Another tool, or a bad input, is not refused.
func (g *Gate) Refuse(tool string, input json.RawMessage) (reason string, refused bool) {
	if tool != "Bash" {
		return "", false
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return "", false
	}
	r, ok := g.rules.Match(in.Command)
	return r.Reason, ok
}
