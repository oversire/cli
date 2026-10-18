// Package runtime holds the agent runtimes that this runner drives, keyed by provider id.
package runtime

import (
	"context"
	"fmt"
	"slices"

	"github.com/oversire/runner/wire"
)

// MaxReadLines caps the lines of one sessionRead.
const MaxReadLines = 200

// Runtime drives the sessions of one agent runtime.
type Runtime interface {
	ID() string
	Start(ctx context.Context, r wire.LaunchRequest) (wire.SessionRef, error)
	Stop(ctx context.Context, s wire.SessionRef) error
	Read(ctx context.Context, s wire.SessionRef, lines int) ([]string, error)
	Send(ctx context.Context, s wire.SessionRef, text string) error
}

type Registry struct{ byID map[string]Runtime }

func NewRegistry(rs ...Runtime) *Registry {
	r := &Registry{byID: map[string]Runtime{}}
	for _, rt := range rs {
		r.byID[rt.ID()] = rt
	}
	return r
}

// IDs are sorted.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (r *Registry) Has(id string) bool {
	_, ok := r.byID[id]
	return ok
}

// Do runs the op of o in its runtime.
func (r *Registry) Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	rt, ok := r.byID[o.Runtime]
	if !ok {
		return nil, fmt.Errorf("This runner has no runtime %s.", o.Runtime)
	}
	switch p := o.Payload.(type) {
	case wire.LaunchRequest:
		s, err := rt.Start(ctx, p)
		if err != nil {
			return nil, err
		}
		return &wire.WorkOrderResult{Session: &s}, nil
	case wire.StopPayload:
		return &wire.WorkOrderResult{}, rt.Stop(ctx, p.Session)
	case wire.ReadPayload:
		lines, err := rt.Read(ctx, p.Session, min(max(p.Lines, 1), MaxReadLines))
		if err != nil {
			return nil, err
		}
		if lines == nil {
			lines = []string{}
		}
		return &wire.WorkOrderResult{Lines: lines}, nil
	case wire.SendPayload:
		return &wire.WorkOrderResult{}, rt.Send(ctx, p.Session, p.Text)
	}
	return nil, fmt.Errorf("This runner does not know the op of order %s.", o.ID)
}
