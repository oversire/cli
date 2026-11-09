package ticket

import (
	"path/filepath"
	"testing"

	"github.com/oversire/cli/wire"
)

func TestGroupOf(t *testing.T) {
	for dir, want := range map[string][2]string{
		"/w/.wt/app-1":           {"/w", "app-1"},
		"/w/.wt/app-1/cli/cmd/x": {"/w", "app-1"},
		"/w/cli":                 {"", ""},
		"/":                      {"", ""},
	} {
		root, name, ok := GroupOf(filepath.FromSlash(dir))
		if want[0] != "" {
			want[0] = filepath.FromSlash(want[0])
		}
		if root != want[0] || name != want[1] || ok != (want[1] != "") {
			t.Errorf("%s: %s %s %v", dir, root, name, ok)
		}
	}
}

func TestItemOf(t *testing.T) {
	for _, c := range []struct {
		s    wire.DaemonSession
		want string
		ok   bool
	}{
		{wire.DaemonSession{Key: "work:a-1"}, "A-1", true},
		{wire.DaemonSession{Key: "plan:A-2"}, "A-2", true},
		{wire.DaemonSession{Key: "refine:a-3"}, "A-3", true},
		{wire.DaemonSession{Key: "enhance:3f2c"}, "3f2c", true},
		{wire.DaemonSession{Key: "runbook:r1.s.1"}, "", false},
		{wire.DaemonSession{Cwd: filepath.FromSlash("/r/.wt/app-1/app")}, "APP-1", true},
		{wire.DaemonSession{Repo: filepath.FromSlash("/r/.wt/app-2/cli")}, "APP-2", true},
		{wire.DaemonSession{Cwd: "/r/app"}, "", false},
	} {
		got, ok := ItemOf(c.s)
		if got != c.want || ok != c.ok {
			t.Errorf("%+v: %q %v", c.s, got, ok)
		}
	}
}

func TestLive(t *testing.T) {
	for _, c := range []struct {
		runtime string
		state   wire.SessionPhase
		want    bool
	}{
		{"orca", wire.PhaseWorking, true},
		{"orca", wire.PhaseFailed, false},
		{"oversire", wire.PhaseDone, false},
		{"orca", wire.PhaseDone, false},
		{wire.RuntimeCLI, wire.PhaseDone, true},
	} {
		if got := Live(wire.DaemonSession{Runtime: c.runtime, State: c.state}); got != c.want {
			t.Errorf("%s %s: %v", c.runtime, c.state, got)
		}
	}
}
