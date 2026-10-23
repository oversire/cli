package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

var now = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

type fakeSup struct {
	mu       sync.Mutex
	state    wire.WorkerState
	reads    int
	stopped  []string
	released []string
	failRel  bool
}

func (f *fakeSup) StartWorker(_ context.Context, w wire.WorkerStart) (runtime.WorkerHandle, error) {
	return runtime.WorkerHandle{Dispatch: "ctx_1", Task: "task_1", Terminal: "term_w"}, nil
}
func (f *fakeSup) WorkerState(context.Context, runtime.WorkerHandle) (wire.WorkerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.state, nil
}
func (f *fakeSup) StopWorker(_ context.Context, h runtime.WorkerHandle) error {
	f.stopped = append(f.stopped, h.Dispatch)
	return nil
}
func (f *fakeSup) ReleaseWorker(_ context.Context, h runtime.WorkerHandle) error {
	f.released = append(f.released, h.Dispatch)
	if f.failRel {
		return errors.New("Orca has no dispatch ctx_1")
	}
	return nil
}

type sups map[string]runtime.Supervisor

func (s sups) Supervisor(id string) (runtime.Supervisor, bool) {
	sup, ok := s[id]
	return sup, ok
}

// allowClaude passes only the agent claude.
type allowClaude struct{}

func (allowClaude) Check(o wire.WorkOrder) error {
	if o.Payload.(wire.LaunchRequest).Agent != "claude" {
		return errors.New("The allow list does not have the agent codex.")
	}
	return nil
}

type rig struct {
	sup    *fakeSup
	book   *Book
	path   string
	caller server.Caller
	srv    *httptest.Server
}

func newRig(t *testing.T, path string) *rig {
	g := &rig{sup: &fakeSup{state: wire.WorkerState{State: wire.WorkerRunning}}, path: path, caller: server.Caller{Loopback: true}}
	g.book = NewBook(sups{"orca": g.sup}, allowClaude{}, path, func() time.Time { return now })
	mux := http.NewServeMux()
	for _, r := range g.book.Routes() {
		mux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), g.caller)))
		})
	}
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *rig) call(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, g.srv.URL+path, bytes.NewReader(b))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

var start = wire.WorkerStart{Spec: "Enhance it", Title: "Enhance TREBI-9", Path: filepath.Join(os.TempDir(), "w"), Agent: "claude"}

func TestWorkerLife(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workers.json")
	g := newRig(t, path)
	var w wire.Worker
	if code := g.call(t, "POST", "/v1/workers", start, &w); code != 200 || w.Runtime != "orca" || w.Terminal != "term_w" || !w.StartedAt.Equal(now) {
		t.Fatal(code, w)
	}
	if g.book.Terminals()["term_w"] != true {
		t.Fatal(g.book.Terminals())
	}
	var st wire.WorkerState
	if code := g.call(t, "GET", "/v1/workers/"+w.ID, nil, &st); code != 200 || st.State != wire.WorkerRunning {
		t.Fatal(code, st)
	}
	if code := g.call(t, "POST", "/v1/workers/"+w.ID+"/stop", nil, nil); code != 204 || len(g.sup.stopped) != 1 {
		t.Fatal(code, g.sup.stopped)
	}

	// A restart of the daemon reads the journal. The worker settles, and the book keeps the settled state.
	g2 := newRig(t, path)
	g2.sup.state = wire.WorkerState{State: wire.WorkerDone, Outcome: "succeeded", Body: `{"verdict":"clear"}`}
	for range 2 {
		if code := g2.call(t, "GET", "/v1/workers/"+w.ID, nil, &st); code != 200 || st != g2.sup.state {
			t.Fatal(code, st)
		}
	}
	if g2.sup.reads != 1 {
		t.Fatal("reads after settle:", g2.sup.reads)
	}
	g3 := newRig(t, path)
	if code := g3.call(t, "GET", "/v1/workers/"+w.ID, nil, &st); code != 200 || st.State != wire.WorkerDone || g3.sup.reads != 0 {
		t.Fatal("the journal lost the settled state:", code, st, g3.sup.reads)
	}

	g3.sup.failRel = true
	if code := g3.call(t, "DELETE", "/v1/workers/"+w.ID, nil, nil); code != 502 {
		t.Fatal(code)
	}
	if code := g3.call(t, "GET", "/v1/workers/"+w.ID, nil, nil); code != 404 {
		t.Fatal("a released worker stays:", code)
	}
	if code := newRig(t, path).call(t, "GET", "/v1/workers/"+w.ID, nil, nil); code != 404 {
		t.Fatal("the journal keeps a released worker:", code)
	}
}

func TestWorkerAllowList(t *testing.T) {
	g := newRig(t, filepath.Join(t.TempDir(), "workers.json"))
	codex := start
	codex.Agent = "codex"
	if code := g.call(t, "POST", "/v1/workers", codex, nil); code != 200 {
		t.Fatal("loopback:", code)
	}
	g.caller = server.Caller{DeviceID: "a-1", Kind: string(wire.DeviceApp), Machine: "mba"}
	if code := g.call(t, "POST", "/v1/workers", codex, nil); code != 403 {
		t.Fatal("tailnet:", code)
	}
	if code := g.call(t, "POST", "/v1/workers", start, nil); code != 200 {
		t.Fatal("tailnet claude:", code)
	}
}

func TestWorkerBadCalls(t *testing.T) {
	g := newRig(t, filepath.Join(t.TempDir(), "workers.json"))
	other := start
	other.Runtime = "oversire"
	cases := []struct {
		method, path string
		body         any
		code         int
	}{
		{"POST", "/v1/workers", other, 404},
		{"POST", "/v1/workers", map[string]any{"spec": "x"}, 400},
		{"GET", "/v1/workers/wk-none", nil, 404},
		{"POST", "/v1/workers/wk-none/stop", nil, 404},
		{"DELETE", "/v1/workers/wk-none", nil, 404},
	}
	for _, c := range cases {
		if code := g.call(t, c.method, c.path, c.body, nil); code != c.code {
			t.Fatal(c, code)
		}
	}
}
