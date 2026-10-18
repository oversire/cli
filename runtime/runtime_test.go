package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/oversire/runner/wire"
)

type fake struct{ read int }

func (f *fake) ID() string { return "fake" }
func (f *fake) Start(context.Context, wire.LaunchRequest) (wire.SessionRef, error) {
	return wire.SessionRef{ID: "s1"}, nil
}
func (f *fake) Stop(context.Context, wire.SessionRef) error { return nil }
func (f *fake) Read(_ context.Context, _ wire.SessionRef, n int) ([]string, error) {
	f.read = n
	return nil, nil
}
func (f *fake) Send(context.Context, wire.SessionRef, string) error { return nil }

func TestRegistry(t *testing.T) {
	f := &fake{}
	r := NewRegistry(f)
	if !slices.Equal(r.IDs(), []string{"fake"}) || !r.Has("fake") || r.Has("orca") {
		t.Fatal(r.IDs())
	}
	ctx := context.Background()
	res, err := r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.LaunchRequest{}})
	if err != nil || res.Session.ID != "s1" {
		t.Fatal(res, err)
	}
	res, err = r.Do(ctx, wire.WorkOrder{Runtime: "fake", Payload: wire.ReadPayload{Lines: 5000}})
	if err != nil || f.read != MaxReadLines || res.Lines == nil {
		t.Fatal(f.read, res, err)
	}
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "orca", Payload: wire.StopPayload{}}); err == nil {
		t.Fatal("an unknown runtime passed")
	}
}
