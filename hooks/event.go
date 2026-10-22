// Package hooks installs the agent CLI hooks that call `oversire hook <harness>`, and turns their payloads into session patches.
// It holds no OverSire UI text, so that it can move to agent-wire later.
package hooks

import (
	"encoding/json"
	"time"

	"github.com/oversire/cli/wire"
)

// Event is one hook call of a harness.
type Event struct {
	Harness string
	Name    string // hook_event_name
	Payload json.RawMessage
	Env     wire.HookEnv
	At      time.Time
	Session string // the session book id that Map gave
}

// QuestionSink gets each hook event after the book applied it (questions.Channel).
type QuestionSink interface{ OnHook(ev Event) }
