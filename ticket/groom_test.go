package ticket

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/oversire/cli/wire"
)

type fakeSessions struct {
	list []wire.DaemonSession
	err  error
}

func (f fakeSessions) Sessions(context.Context) ([]wire.DaemonSession, error) { return f.list, f.err }

func TestCheckGroom(t *testing.T) {
	holder := wire.DaemonSession{ID: "g1", Runtime: "oversire", Key: "groom:TREBI", HarnessSession: "h1", State: wire.PhaseWorking}
	busy := []wire.DaemonSession{
		{ID: "w1", Runtime: "orca", Key: "work:TREBI-3", State: wire.PhaseWorking},
		{ID: "w2", Runtime: "orca", Cwd: filepath.FromSlash("/r/.wt/trebi-5/app"), State: wire.PhaseWaiting},
		{ID: "w3", Runtime: "orca", Key: "work:OTHER-1", State: wire.PhaseWorking},
	}
	g, err := CheckGroom(context.Background(), fakeSessions{list: append([]wire.DaemonSession{holder}, busy...)}, "TREBI", "s9")
	if err != nil || !g.Daemon || g.Holder == nil || g.Holder.ID != "g1" {
		t.Fatal(g, err)
	}
	if len(g.Busy) != 2 || g.Busy["TREBI-3"] == nil || g.Busy["TREBI-5"] == nil {
		t.Fatal(g.Busy)
	}
	if g, _ := CheckGroom(context.Background(), fakeSessions{list: []wire.DaemonSession{holder}}, "TREBI", "h1"); g.Holder != nil {
		t.Fatal("the session of the caller holds the key")
	}
	failed := holder
	failed.State = wire.PhaseFailed
	if g, _ := CheckGroom(context.Background(), fakeSessions{list: []wire.DaemonSession{failed}}, "TREBI", "s9"); g.Holder != nil {
		t.Fatal("a failed session holds the key")
	}
}

func TestCheckGroomNoDaemon(t *testing.T) {
	g, err := CheckGroom(context.Background(), fakeSessions{err: ErrNoDaemon}, "TREBI", "s9")
	if err != nil || g.Daemon {
		t.Fatal(g, err)
	}
}

func TestNormalProject(t *testing.T) {
	if p, err := NormalProject(" trebi "); err != nil || p != "TREBI" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"", "9X", "A-1", "../x"} {
		if _, err := NormalProject(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
