package ticket

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/oversire/cli/wire"
)

var projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// NormalProject gives the project identifier in upper case.
func NormalProject(p string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(p))
	if !projectPattern.MatchString(n) || len(n) > 50 {
		return "", fmt.Errorf("%q is not a project identifier (for example OVERSIRE)", p)
	}
	return n, nil
}

// GroomKey is the start key of the Groom of a project (StartKey.groom in the app).
func GroomKey(project string) string { return "groom:" + project }

// GroomCheck tells if a Groom of the project runs, and which items have a live session.
type GroomCheck struct {
	Project string
	Key     string
	Daemon  bool                            // false: no daemon answered, the check did not run
	Holder  *wire.DaemonSession             // the live session with the key; nil: the key is free
	Busy    map[string][]wire.DaemonSession // item key -> live sessions, only items of the project
}

// CheckGroom reads the sessions of the daemon. The session of the caller does not hold the key.
func CheckGroom(ctx context.Context, d SessionLister, project, session string) (GroomCheck, error) {
	g := GroomCheck{Project: project, Key: GroomKey(project), Busy: map[string][]wire.DaemonSession{}}
	list, err := d.Sessions(ctx)
	if errors.Is(err, ErrNoDaemon) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	g.Daemon = true
	for i, s := range list {
		if !Live(s) {
			continue
		}
		if s.Key == g.Key && g.Holder == nil && (session == "" || s.HarnessSession != session) {
			g.Holder = &list[i]
			continue
		}
		item, ok := ItemOf(s)
		if ok && (strings.HasPrefix(item, project+"-") || strings.HasPrefix(s.Key, "enhance:") && !keyPattern.MatchString(item)) {
			g.Busy[item] = append(g.Busy[item], s)
		}
	}
	return g, nil
}
