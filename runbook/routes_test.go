package runbook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// serve puts the routes on a loopback mux and on a Tailscale mux, as the server does.
// On Tailscale, "Bearer good" is an app and "Bearer peer" is a daemon of the group.
func serve(t *testing.T, e *Engine) (loop, tailnet string) {
	t.Helper()
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	callers := map[string]server.Caller{
		"Bearer good": {DeviceID: "D1", Name: "mba app", Kind: string(wire.DeviceApp)},
		"Bearer peer": {DeviceID: "D2", Name: "mini", Kind: string(wire.DeviceDaemon)},
	}
	for _, r := range NewRoutes(e).Routes() {
		if r.Access != server.Member {
			t.Fatalf("%s is not for members", r.Pattern)
		}
		loopMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		netMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			c, ok := callers[req.Header.Get("Authorization")]
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), c)))
		})
	}
	l, n := httptest.NewServer(loopMux), httptest.NewServer(netMux)
	t.Cleanup(func() { l.Close(); n.Close() })
	return l.URL, n.URL
}

func send(t *testing.T, method, url, body string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func startBody(t *testing.T, key string, b wire.Runbook) string {
	t.Helper()
	it := item
	it.Key = key
	data, err := json.Marshal(wire.RunbookStart{Item: it, Repo: "/r/app", Runbook: b, Agents: agents})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRoutes(t *testing.T) {
	g := newRig(t, "")
	loop, tailnet := serve(t, g.e)

	code, body := send(t, "POST", loop+"/v1/runbook-runs", startBody(t, "OVERSIRE-341", linear()))
	var run wire.RunbookRun
	if code != 201 || json.Unmarshal([]byte(body), &run) != nil || run.State != wire.RunRunning {
		t.Fatal(code, body)
	}
	g.e.wg.Wait()
	id := run.ID
	if code, body := send(t, "POST", loop+"/v1/runbook-runs", startBody(t, "OVERSIRE-341", linear())); code != 409 || body != `{"error":"OVERSIRE-341 has the open run `+id+`."}` {
		t.Fatal(code, body)
	}
	bad := linear()
	bad.Steps[0].Prompt = "No done."
	if code, body := send(t, "POST", loop+"/v1/runbook-runs", startBody(t, "OVERSIRE-9", bad)); code != 400 ||
		body != `{"error":"The runbook is not valid.","problems":[{"step":"plan","text":"The prompt has no {done}. The agent needs it to end the step."}]}` {
		t.Fatal(code, body)
	}
	if code, _ := send(t, "POST", loop+"/v1/runbook-runs", `{"item":{},"x":1}`); code != 400 {
		t.Fatal("unknown field", code)
	}

	for _, c := range []struct {
		method, path, body string
		code               int
		has                string
	}{
		{"GET", "/v1/runbook-runs?item=OVERSIRE-341&open=1", "", 200, `"id":"` + id},
		{"GET", "/v1/runbook-runs?item=OVERSIRE-9", "", 200, `[]`},
		{"GET", "/v1/runbook-runs/" + id, "", 200, `"current":"plan"`},
		{"GET", "/v1/runbook-runs/rb_00000000", "", 404, `{"error":"There is no run rb_00000000."}`},
		{"POST", "/v1/runbook-runs/" + id + "/approve", "", 409, `does not wait at a gate`},
		{"POST", "/v1/runbook-runs/" + id + "/resume", "", 409, `not paused`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/build/done", `{"verdict":"done"}`, 409, `not the current step`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/plan/done", `{"verdict":"pass"}`, 400, `takes the verdict done, skipped or blocked`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/nope/done", `{"verdict":"done"}`, 404, `no step nope`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/plan/done", `{"verdict":`, 400, `not valid JSON`},
		{"PATCH", "/v1/runbook-runs/" + id + "/steps/plan", `{"agent":"claude-max"}`, 409, `started`},
		{"PATCH", "/v1/runbook-runs/" + id + "/steps/build", `{"agent":"codex"}`, 400, `no agent codex`},
		{"PATCH", "/v1/runbook-runs/" + id + "/steps/build", `{"prompt":"Build {key}. Run {done}."}`, 200, `Build {key}. Run {done}.`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/plan/retry", "", 409, `runs now`},
		{"POST", "/v1/runbook-runs/" + id + "/pause", "", 200, `"state":"paused"`},
		{"POST", "/v1/runbook-runs/" + id + "/pause", "", 409, `Only a running run`},
		{"POST", "/v1/runbook-runs/" + id + "/resume", "", 200, `"state":"running"`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/plan/done", `{"verdict":"done","note":"ok"}`, 200, `"current":"build"`},
		{"POST", "/v1/runbook-runs/" + id + "/steps/build/skip", "", 200, `"state":"done"`},
		{"POST", "/v1/runbook-runs/" + id + "/stop", "", 409, `already`},
		{"GET", "/v1/runbook-runs?item=OVERSIRE-341&open=1", "", 200, `[]`},
	} {
		code, body := send(t, c.method, loop+c.path, c.body)
		g.e.wg.Wait()
		if code != c.code || !strings.Contains(body, c.has) {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}

	// Tailscale: no credential is 401, an app changes runs, a daemon only reads.
	if code, _ := send(t, "GET", tailnet+"/v1/runbook-runs", ""); code != 401 {
		t.Fatal("no credential", code)
	}
	if code, _ := send(t, "POST", tailnet+"/v1/runbook-runs/"+id+"/stop", ""); code != 401 {
		t.Fatal("no credential", code)
	}
	if code, body := send(t, "GET", tailnet+"/v1/runbook-runs/"+id, "", "Authorization", "Bearer peer"); code != 200 || !strings.Contains(body, `"state":"done"`) {
		t.Fatal("daemon read", code, body)
	}
	if code, body := send(t, "POST", tailnet+"/v1/runbook-runs", startBody(t, "OVERSIRE-7", linear()), "Authorization", "Bearer peer"); code != 403 {
		t.Fatal("daemon start", code, body)
	}
	code, body = send(t, "POST", tailnet+"/v1/runbook-runs", startBody(t, "OVERSIRE-7", linear()), "Authorization", "Bearer good")
	if code != 201 {
		t.Fatal("app start", code, body)
	}
	json.Unmarshal([]byte(body), &run)
	g.e.wg.Wait()
	if code, body := send(t, "POST", tailnet+"/v1/runbook-runs/"+run.ID+"/stop", "", "Authorization", "Bearer good"); code != 200 || !strings.Contains(body, `"state":"stopped"`) {
		t.Fatal("app stop", code, body)
	}
}
