package orca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/wire"
)

// sleepyOrca is a slow Orca CLI: each call takes delay, like an Orca app that is busy.
type sleepyOrca struct {
	delay time.Duration
	calls sync.Map // command -> *atomic.Int64
}

func (s *sleepyOrca) count(command string) int64 {
	if n, ok := s.calls.Load(command); ok {
		return n.(*atomic.Int64).Load()
	}
	return 0
}

func (s *sleepyOrca) Run(ctx context.Context, _ string, args []string, _ []byte) ([]byte, []byte, error) {
	command := strings.Join(args[:2], " ")
	n, _ := s.calls.LoadOrStore(command, &atomic.Int64{})
	n.(*atomic.Int64).Add(1)
	if command != "terminal read" {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	switch command {
	case "worktree ps":
		return []byte(`{"ok":true,"result":{"worktrees":[{"worktreeId":"r1::/wt/a","repo":"app","path":"/wt/a"}]}}`), nil, nil
	case "terminal list":
		return []byte(`{"ok":true,"result":{"terminals":[]}}`), nil, nil
	case "terminal read":
		return []byte(`{"ok":true,"result":{"terminal":{"tail":["ready"]}}}`), nil, nil
	}
	return []byte(`{"ok":true,"result":{"repos":[]}}`), nil, nil
}

// The load of OVERSIRE-425: many parallel workspace reads made many slow Orca processes, and cheap calls waited.
func TestManyWorkspaceReadsMakeOneFetch(t *testing.T) {
	orca := &sleepyOrca{delay: 300 * time.Millisecond}
	reg := runtime.NewRegistry(New("/bin/orca", orca))
	mux := http.NewServeMux()
	for _, r := range runtime.NewAPI(reg, t.TempDir(), time.Now).Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 32}}

	var wg sync.WaitGroup
	var good atomic.Int64
	for range 16 {
		wg.Go(func() {
			res, err := client.Get(srv.URL + "/v1/workspaces")
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			var page wire.DaemonWorkspacesPage
			if json.NewDecoder(res.Body).Decode(&page) == nil && len(page.Workspaces) == 1 {
				good.Add(1)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	// A session read while the workspace fetch runs gets a free slot at once.
	start := time.Now()
	res, err := reg.Do(context.Background(), wire.WorkOrder{Runtime: ID, Payload: wire.ReadPayload{Session: wire.SessionRef{ID: "h1"}, Lines: 5}})
	if took := time.Since(start); err != nil || len(res.Lines) != 1 || took > 50*time.Millisecond {
		t.Fatalf("read took %v: %+v %v", took, res, err)
	}
	wg.Wait()
	if good.Load() != 16 || orca.count("worktree ps") != 1 || orca.count("terminal list") != 1 {
		t.Fatalf("%d good, worktree ps %d, terminal list %d", good.Load(), orca.count("worktree ps"), orca.count("terminal list"))
	}
}
