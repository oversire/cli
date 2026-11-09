package ticket

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/oversire/cli/wire"
)

// ErrNoDaemon: no daemon answers on the loopback port.
var ErrNoDaemon = errors.New("no daemon answers")

// SessionLister reads the sessions of the local daemon.
type SessionLister interface {
	Sessions(ctx context.Context) ([]wire.DaemonSession, error)
}

// GroupOf finds the worktree group <root>/.wt/<name> that holds dir.
func GroupOf(dir string) (root, name string, ok bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		parent := filepath.Dir(d)
		if filepath.Base(parent) == ".wt" {
			return filepath.Dir(parent), filepath.Base(d), true
		}
		if parent == d {
			return "", "", false
		}
	}
}

// itemPrefixes are the start keys that name an item.
var itemPrefixes = []string{"work:", "plan:", "refine:", "enhance:"}

// ItemOf gives the item of a session: from its start key, else from its worktree group.
// An enhance: intake id stays as it is.
func ItemOf(s wire.DaemonSession) (string, bool) {
	if s.Key != "" {
		for _, p := range itemPrefixes {
			rest, ok := strings.CutPrefix(s.Key, p)
			if !ok || rest == "" {
				continue
			}
			if k, err := NormalKey(rest); err == nil {
				return k, true
			}
			if p == "enhance:" {
				return rest, true
			}
		}
		return "", false
	}
	for _, dir := range []string{s.Cwd, s.Repo} {
		if dir == "" {
			continue
		}
		if _, name, ok := GroupOf(dir); ok {
			return strings.ToUpper(name), true
		}
	}
	return "", false
}

// Live is true for a session that works or waits. An ended cli session stays in the list for
// sessions.CLIEndTTL, so it counts as live for that time. A done orca or oversire session does not.
func Live(s wire.DaemonSession) bool {
	switch s.State {
	case wire.PhaseStarting, wire.PhaseWorking, wire.PhaseWaiting:
		return true
	case wire.PhaseDone:
		return s.Runtime == wire.RuntimeCLI
	}
	return false
}
