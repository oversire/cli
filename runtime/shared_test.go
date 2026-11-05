package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// slowCat counts the fetches. A fetch waits for gate when gate is set.
type slowCat struct {
	fake
	repos, lists atomic.Int64
	gate         chan struct{}
	fail         atomic.Bool
}

func (c *slowCat) ID() string { return "slow" }
func (c *slowCat) wait(ctx context.Context) error {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.fail.Load() {
		return errors.New("orca repo list: timeout")
	}
	return nil
}
func (c *slowCat) Repos(ctx context.Context) ([]wire.DaemonRepo, error) {
	c.repos.Add(1)
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return []wire.DaemonRepo{{ID: "r1", Runtime: "slow", Path: "/r"}}, nil
}
func (c *slowCat) Workspaces(ctx context.Context) ([]wire.DaemonWorkspace, error) {
	n := c.lists.Add(1)
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return []wire.DaemonWorkspace{{ID: "w", Name: string(rune('0' + n)), Runtime: "slow"}}, nil
}

// testClock moves only when the test moves it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

func clockRegistry(rts ...Runtime) (*Registry, *testClock) {
	clk := &testClock{at: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	r := NewRegistry(rts...)
	r.cache = NewCatalogCache(clk.now)
	return r, clk
}

func TestCatalogParallelCallsMakeOneFetch(t *testing.T) {
	c := &slowCat{gate: make(chan struct{})}
	r, _ := clockRegistry(c)
	var wg sync.WaitGroup
	var got atomic.Int64
	for range 20 {
		wg.Go(func() {
			if len(r.Repos(context.Background()).Repos) == 1 {
				got.Add(1)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(c.gate)
	wg.Wait()
	if c.repos.Load() != 1 || got.Load() != 20 {
		t.Fatalf("%d fetches, %d answers", c.repos.Load(), got.Load())
	}
}

func TestCatalogTTL(t *testing.T) {
	c := &slowCat{}
	r, clk := clockRegistry(c)
	ctx := context.Background()
	r.Repos(ctx)
	r.Workspaces(ctx)
	clk.add(workspacesTTL)
	r.Repos(ctx)
	r.Workspaces(ctx)
	if c.repos.Load() != 1 || c.lists.Load() != 2 {
		t.Fatalf("repos %d, workspaces %d", c.repos.Load(), c.lists.Load())
	}
	clk.add(reposTTL)
	r.Repos(ctx)
	if c.repos.Load() != 2 {
		t.Fatalf("repos %d", c.repos.Load())
	}
}

func TestCatalogStoppedCallKeepsTheFetch(t *testing.T) {
	c := &slowCat{gate: make(chan struct{})}
	r, _ := clockRegistry(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan wire.DaemonWorkspacesPage)
	go func() { done <- r.Workspaces(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if page := <-done; page.Errors["slow"] != context.Canceled.Error() {
		t.Fatalf("%+v", page)
	}
	close(c.gate)
	if page := r.Workspaces(context.Background()); len(page.Workspaces) != 1 || c.lists.Load() != 1 {
		t.Fatalf("%+v, %d fetches", page, c.lists.Load())
	}
}

func TestCatalogLastGoodOnError(t *testing.T) {
	c := &slowCat{}
	r, clk := clockRegistry(c)
	ctx := context.Background()
	if page := r.Workspaces(ctx); len(page.Workspaces) != 1 || len(r.Repos(ctx).Repos) != 1 {
		t.Fatalf("%+v", page)
	}
	c.fail.Store(true)
	clk.add(workspacesTTL)
	if page := r.Workspaces(ctx); len(page.Workspaces) != 1 || page.Errors != nil || c.lists.Load() != 2 {
		t.Fatalf("%+v", page)
	}
	clk.add(reposTTL)
	if len(r.Repos(ctx).Repos) != 1 || c.repos.Load() != 2 {
		t.Fatal("the last good repos")
	}
	// A runtime with no good list gives the error, and keeps it only errKeep.
	r2, clk2 := clockRegistry(c)
	if page := r2.Workspaces(ctx); page.Errors["slow"] == "" {
		t.Fatalf("%+v", page)
	}
	c.fail.Store(false)
	clk2.add(errKeep)
	if page := r2.Workspaces(ctx); len(page.Workspaces) != 1 {
		t.Fatalf("%+v", page)
	}
}

func TestCatalogLaunchAndRemoveClearWorkspaces(t *testing.T) {
	c := &slowCat{}
	r, _ := clockRegistry(c)
	ctx := context.Background()
	r.Workspaces(ctx)
	r.Workspaces(ctx)
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "slow", Payload: wire.LaunchRequest{}}); err != nil {
		t.Fatal(err)
	}
	if page := r.Workspaces(ctx); c.lists.Load() != 2 || page.Workspaces[0].Name != "2" {
		t.Fatalf("%d fetches, %+v", c.lists.Load(), page)
	}
	if _, err := r.Do(ctx, wire.WorkOrder{Runtime: "slow", Payload: wire.StopPayload{}}); err != nil {
		t.Fatal(err)
	}
	r.Workspaces(ctx)
	r.Repos(ctx)
	r.Repos(ctx)
	if c.lists.Load() != 3 || c.repos.Load() != 1 {
		t.Fatalf("workspaces %d, repos %d", c.lists.Load(), c.repos.Load())
	}
}
