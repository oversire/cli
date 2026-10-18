package wire

import (
	"encoding/json"
	"slices"
	"time"
)

// MaxChangedPaths caps ChangedPaths.Paths.
const MaxChangedPaths = 500

// SubjectRef names any thing. Ref stays raw, because the runner only makes item subjects.
type SubjectRef struct {
	Kind string          `json:"kind"`
	Ref  json.RawMessage `json:"ref"`
}

func ItemSubject(r ItemRef) SubjectRef {
	b, _ := json.Marshal(r)
	return SubjectRef{Kind: "item", Ref: b}
}

// StoreKey is "<account id>/<id>", the same as ProjectRef.storeKey in the app.
func (p ProjectRef) StoreKey() string { return p.Account.ID + "/" + p.ID }

// ItemAreasID is the store id of the item records (ItemAreas.id(of:) in the app).
func (r ItemRef) ItemAreasID() string { return r.Project.StoreKey() + "/" + r.ID }

// ChangedPaths is the paths that the running work of one subject changed. Only paths, never file text.
type ChangedPaths struct {
	ID      string     `json:"id"`
	Subject SubjectRef `json:"subject"`
	Paths   []string   `json:"paths"`
	At      time.Time  `json:"at"`
}

// NewItemPaths sorts the paths, drops repeats and keeps at most MaxChangedPaths.
func NewItemPaths(item ItemRef, paths []string, at time.Time) ChangedPaths {
	p := slices.Compact(slices.Sorted(slices.Values(paths)))
	if p == nil {
		p = []string{}
	}
	return ChangedPaths{ID: item.ItemAreasID(), Subject: ItemSubject(item), Paths: p[:min(len(p), MaxChangedPaths)], At: at.UTC()}
}

// Paths is the changed paths of the worktree of one sessionStart order.
type Paths struct {
	Order string       `json:"order"`
	Paths ChangedPaths `json:"paths"`
}

func (Paths) Type() string { return "paths" }
