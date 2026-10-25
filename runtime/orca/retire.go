package orca

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/oversire/cli/wire"
)

// coordinatorTitle names the terminal of the old Orca orchestration coordinator (before OVERSIRE-322).
const coordinatorTitle = "oversire-coordinator"

// RetireWorkers removes what the old Enhance workers left (OVERSIRE-322). It closes the terminal of each
// worker in the journal that did not settle, then deletes the journal. It also closes each live coordinator
// terminal. A failed close keeps the journal, so the next start tries again.
func (o *Orca) RetireWorkers(ctx context.Context, journal string, log *slog.Logger) {
	if data, err := os.ReadFile(journal); err == nil {
		var book map[string]struct {
			Handle struct {
				Terminal string `json:"terminal"`
			} `json:"handle"`
			Settled json.RawMessage `json:"settled"`
		}
		_ = json.Unmarshal(data, &book)
		keep := false
		for id, e := range book {
			if e.Handle.Terminal == "" || (len(e.Settled) > 0 && string(e.Settled) != "null") {
				continue
			}
			err := o.Stop(ctx, wire.SessionRef{ID: e.Handle.Terminal})
			if gone(err) {
				err = nil
			}
			keep = keep || err != nil
			log.Info("retire worker: close terminal", "worker", id, "terminal", e.Handle.Terminal, "err", err)
		}
		if !keep {
			err := os.Remove(journal)
			log.Info("retire worker: delete journal", "path", journal, "err", err)
		}
	}
	var list struct{ Terminals []terminal }
	if err := o.callInto(ctx, 20*time.Second, &list, "terminal", "list", "--limit", "1000"); err != nil {
		log.Warn("retire worker: list terminals", "err", err)
		return
	}
	for _, t := range list.Terminals {
		if t.Title == coordinatorTitle && !t.Orphaned {
			err := o.Stop(ctx, wire.SessionRef{ID: t.Handle})
			log.Info("retire worker: close coordinator", "terminal", t.Handle, "err", err)
		}
	}
}
