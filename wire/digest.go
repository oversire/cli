package wire

// Digest sources (SessionDigest.source). A value that the runner does not know stays as it is.
const (
	DigestFromFile   = "file"
	DigestFromRunner = "runner"
)

// HistoryBudget tells how much of the history a reader keeps. A nil field takes its default.
type HistoryBudget struct {
	Earlier  *int `json:"earlier,omitempty"`
	Full     *int `json:"full,omitempty"`
	MaxChars *int `json:"maxChars,omitempty"`
}

// The defaults of HistoryBudget.
const (
	DefaultEarlier  = 12
	DefaultFull     = 3
	DefaultMaxChars = 4000
)

// Values gives the budget with the defaults for the missing fields. A nil budget gives all defaults.
func (b *HistoryBudget) Values() (earlier, full, maxChars int) {
	earlier, full, maxChars = DefaultEarlier, DefaultFull, DefaultMaxChars
	if b == nil {
		return
	}
	if b.Earlier != nil {
		earlier = *b.Earlier
	}
	if b.Full != nil {
		full = *b.Full
	}
	if b.MaxChars != nil {
		maxChars = *b.MaxChars
	}
	return
}

// NewHistoryBudget sets all three fields.
func NewHistoryBudget(earlier, full, maxChars int) HistoryBudget {
	return HistoryBudget{Earlier: &earlier, Full: &full, MaxChars: &maxChars}
}

type EarlierTurn struct {
	Prompt      string `json:"prompt"`
	Closing     string `json:"closing"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

type LastTurn struct {
	Prompt      string `json:"prompt"`
	Text        string `json:"text"`
	Tools       int    `json:"tools"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

// SessionDigest is a short record of the history of one agent session (session-digest.json).
type SessionDigest struct {
	Harness   string        `json:"harness"`
	SessionID string        `json:"sessionId"`
	Source    string        `json:"source"`
	Turns     int           `json:"turns"`
	Earlier   []EarlierTurn `json:"earlier"`
	Last      []LastTurn    `json:"last"`
	Truncated bool          `json:"truncated"`
}

// HistoryPayload asks for the history of a session as a SessionDigest.
type HistoryPayload struct {
	Session SessionRef     `json:"session"`
	Budget  *HistoryBudget `json:"budget,omitempty"`
}

func (HistoryPayload) Op() Op { return OpSessionHistory }
