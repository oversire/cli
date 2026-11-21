//go:build !windows

package oversire

import (
	"context"
	"errors"
	"os"
	"testing"

	agentvisor "github.com/trebi-ai/agent-visor"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// stopFailHost fails each stop.
type stopFailHost struct {
	Host
	err error
}

func (h *stopFailHost) Stop(context.Context, string) error { return h.err }

func TestRemoveWorkspaceKeepsTheWorkdirWhenStopFails(t *testing.T) {
	errStop := errors.New("stop failed")
	f := setupWith(t, chat, Options{}, func(v *agentvisor.Visor) Host { return &stopFailHost{Host: v, err: errStop} })
	ref := f.launch(t, "APP-3", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	w := f.workdir(t, "APP-3")

	_, err := f.rt.RemoveWorkspace(context.Background(), w.ID, true, false)
	if !errors.Is(err, errStop) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("the workdir is gone: %v", err)
	}
	if got := f.workdir(t, "APP-3"); got.State != agentvisor.WorkdirActive {
		t.Fatalf("state %v", got.State)
	}
}

func TestRemoveWorkspaceRemovesAWorktree(t *testing.T) {
	f := setup(t, chat, Options{})
	ref := f.launch(t, "APP-3", runtime.Order{ID: "o1"})
	f.rec.wait(t, ref.ID, wire.SessionWaitingInput)
	w := f.workdir(t, "APP-3")

	if _, err := f.rt.RemoveWorkspace(context.Background(), w.ID, true, false); err != nil {
		t.Fatal(err)
	}
	wds, err := f.visor.Workdirs(context.Background(), agentvisor.Filter{Key: "APP-3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range wds {
		if d.State == agentvisor.WorkdirActive {
			t.Fatalf("the workdir is still active: %#v", d)
		}
	}
}
