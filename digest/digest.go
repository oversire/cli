// Package digest makes a SessionDigest (contract/schema/session-digest.json) from the transcript of a harness session.
// The rules are the same as in oversire/scripts/digest.py and AgentBinder TranscriptReader.
// contract/fixtures/session-digest holds the cases that all readers pass.
package digest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oversire/runner/wire"
)

// Reader makes the digest of one session of a harness.
type Reader interface {
	// Digest reads the session, a transcript file or a harness session id.
	Digest(session string, b *wire.HistoryBudget, source string) (wire.SessionDigest, error)
}

// ErrNoSession means that the reader found no transcript for the session.
var ErrNoSession = errors.New("no transcript")

// ForHarness gives the reader of the harness. home is the home folder of the user.
func ForHarness(harness, home string) (Reader, error) {
	switch harness {
	case Claude:
		return NewClaudeReader(filepath.Join(home, ".claude", "projects")), nil
	}
	return nil, fmt.Errorf("The runner cannot read the history of the harness %q. Only %s is supported.", harness, Claude)
}

// ForUser is ForHarness with the home folder of this user.
func ForUser(harness string) (Reader, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return ForHarness(harness, home)
}
