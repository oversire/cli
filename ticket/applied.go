package ticket

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oversire/cli/wire"
)

// The event kinds of the skill writes.
var appliedKinds = map[string]bool{wire.KindGroomApplied: true}

// AppliedLine is one item that the skill changed. The agent writes these lines to a file.
type AppliedLine struct {
	Item    string          `json:"item"`
	Parent  string          `json:"parent,omitempty"`
	Summary string          `json:"summary"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Actor is the agent session that applied the change.
type Actor struct{ Harness, Session, Machine string }

// AppliedDrafts makes one event row for each line. The id does not depend on the time,
// so a retry gives the same id and the events store drops the copy.
func AppliedDrafts(kind, project string, lines []AppliedLine, a Actor, now time.Time) ([]wire.EventDraft, error) {
	if !appliedKinds[kind] {
		return nil, fmt.Errorf("%q is not an applied event kind", kind)
	}
	verb := strings.TrimSuffix(kind, ".applied")
	out := make([]wire.EventDraft, 0, len(lines))
	for i, l := range lines {
		item, err := NormalKey(l.Item)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		parent := ""
		if l.Parent != "" {
			if parent, err = NormalKey(l.Parent); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
		if n := len([]rune(l.Summary)); n == 0 || n > wire.MaxEventSummary {
			return nil, fmt.Errorf("line %d: the summary must have 1 to %d characters", i+1, wire.MaxEventSummary)
		}
		if len(l.Payload) > wire.MaxEventPayload {
			return nil, fmt.Errorf("line %d: the payload is above %d bytes", i+1, wire.MaxEventPayload)
		}
		out = append(out, wire.EventDraft{
			ID:         fmt.Sprintf("cli:%s:%s:%s:%s", verb, a.Session, item, payloadAction(l.Payload)),
			At:         now.UTC(),
			Kind:       kind,
			Item:       item,
			ParentItem: parent,
			Project:    strings.ToLower(project),
			Machine:    a.Machine,
			Session:    a.Session,
			ActorType:  wire.ActorAgent,
			Actor:      a.Harness,
			Via:        "cli",
			Summary:    l.Summary,
			Payload:    l.Payload,
		})
	}
	return out, nil
}

// payloadAction is the kind or the action of a payload, for the row id.
func payloadAction(p json.RawMessage) string {
	var v struct{ Kind, Action string }
	_ = json.Unmarshal(p, &v)
	switch {
	case v.Kind != "":
		return v.Kind
	case v.Action != "":
		return v.Action
	}
	return "change"
}
