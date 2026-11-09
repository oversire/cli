package review

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

// serve puts the routes on a loopback mux and on a Tailscale mux, as the server does: a LoopbackOnly route
// is not on the Tailscale mux. On Tailscale, "Bearer good" is an app and "Bearer peer" is a daemon of the group.
func serve(t *testing.T, e *Engine) (loop, tailnet string) {
	t.Helper()
	loopMux, netMux := http.NewServeMux(), http.NewServeMux()
	callers := map[string]server.Caller{
		"Bearer good": {DeviceID: "D1", Name: "mba app", Kind: string(wire.DeviceApp)},
		"Bearer peer": {DeviceID: "D2", Name: "mini", Kind: string(wire.DeviceDaemon)},
	}
	for _, r := range NewRoutes(e).Routes() {
		loopMux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
		if r.Access == server.LoopbackOnly {
			continue
		}
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

func decodeAs[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestRoutes(t *testing.T) {
	w, r, g := codeRig(t)
	loop, tailnet := serve(t, r.Engine)
	app := []string{"Authorization", "Bearer good"}
	peer := []string{"Authorization", "Bearer peer"}

	code, body := send(t, "GET", tailnet+"/v1/reviews?kind=code", "", app...)
	page := decodeAs[wire.ReviewsPage](t, body)
	if code != 200 || len(page.Reviews) != 1 || page.Reviews[0].Key != "APP-1" || page.NotPushed == nil {
		t.Fatal(code, body)
	}
	if code, body = send(t, "GET", loop+"/v1/reviews?kind=other", ""); code != 400 || !strings.HasPrefix(body, `{"error":`) {
		t.Fatal(code, body)
	}
	// The key in the path can be in lower case.
	code, body = send(t, "GET", tailnet+"/v1/reviews/app-1", "", app...)
	if d := decodeAs[wire.ReviewDetail](t, body); code != 200 || len(d.Files) != 2 {
		t.Fatal(code, body)
	}
	if code, _ = send(t, "GET", tailnet+"/v1/reviews/APP-1/diff?repo=app&path=n.go", "", peer...); code != 200 {
		t.Fatal("a peer daemon reads", code)
	}
	code, body = send(t, "GET", loop+"/v1/reviews/APP-1/diff?repo=app&path=n.go&context=full", "")
	if d := decodeAs[wire.FileDiff](t, body); code != 200 || !d.Full || !strings.Contains(body, `"full":true`) {
		t.Fatal(code, body)
	}
	if code, body = send(t, "GET", loop+"/v1/reviews/APP-1/diff?repo=app&path=n.go&context=x", ""); code != 400 || !strings.Contains(body, "only be full") {
		t.Fatal(code, body)
	}
	if code, body = send(t, "GET", loop+"/v1/reviews/APP-1/diff?repo=app", ""); code != 400 {
		t.Fatal(code, body)
	}
	if code, _ = send(t, "GET", loop+"/v1/reviews/APP-9", ""); code != 404 {
		t.Fatal(code)
	}

	draft := `{"anchor":{"repo":"app","path":"n.go","line":3},"text":"Rename."}`
	if code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/comments", draft, peer...); code != 403 {
		t.Fatal("a peer daemon writes", code, body)
	}
	if code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/comments", `{"text":"x","extra":1}`, app...); code != 400 {
		t.Fatal(code, body)
	}
	code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/comments", draft, app...)
	c := decodeAs[wire.ReviewComment](t, body)
	if code != 201 || c.Excerpt != "func N() {}" {
		t.Fatal(code, body)
	}
	cpath := "/v1/reviews/APP-1/comments/" + c.ID
	if code, body = send(t, "PATCH", tailnet+cpath, `{"text":"Rename N."}`, app...); code != 200 || decodeAs[wire.ReviewComment](t, body).Text != "Rename N." {
		t.Fatal(code, body)
	}
	if code, _ = send(t, "PUT", tailnet+"/v1/reviews/APP-1/viewed", `{"repo":"app","path":"n.go","viewed":true}`, app...); code != 204 {
		t.Fatal(code)
	}
	other := decodeAs[wire.ReviewComment](t, func() string {
		_, b := send(t, "POST", loop+"/v1/reviews/APP-1/comments", `{"anchor":{"repo":"app","path":"n.go","line":5},"text":"Drop."}`)
		return b
	}())
	if code, _ = send(t, "DELETE", tailnet+"/v1/reviews/APP-1/comments/"+other.ID+"?kind=code", "", app...); code != 204 {
		t.Fatal(code)
	}

	// The agent calls are not on Tailscale.
	for _, x := range []struct{ method, path, body string }{
		{"POST", cpath + "/reply", `{"text":"Done."}`},
		{"POST", "/v1/reviews/APP-1/ready", `{}`},
		{"PUT", "/v1/reviews/APP-1/plan", `{"key":"APP-1","version":1,"blocks":[{"id":"a","heading":"A","markdown":""}]}`},
	} {
		if code, _ = send(t, x.method, tailnet+x.path, x.body, app...); code != 404 && code != 405 {
			t.Fatalf("%s %s on Tailscale: %d", x.method, x.path, code)
		}
	}

	r.book.put(session("s1", wire.PhaseWaiting, r.clock.now(), filepath.Join(g, "app")))
	code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/submit", `{"verdict":"requestChanges","summary":"One thing."}`, app...)
	if round := decodeAs[wire.ReviewRound](t, body); code != 200 || round.Delivery != wire.DeliverySent || round.Count != 1 {
		t.Fatal(code, body)
	}
	if code, body = send(t, "PATCH", tailnet+cpath, `{"text":"Later."}`, app...); code != 409 {
		t.Fatal(code, body)
	}
	if code, body = send(t, "POST", loop+cpath+"/reply", `{"text":"Renamed."}`); code != 200 || decodeAs[wire.ReviewComment](t, body).State != wire.CommentAnswered {
		t.Fatal(code, body)
	}
	if code, body = send(t, "POST", loop+"/v1/reviews/APP-1/ready", `{"note":"Renamed."}`); code != 200 || decodeAs[wire.ReviewSummary](t, body).Note != "Renamed." {
		t.Fatal(code, body)
	}
	if code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/fix?kind=plan", "", app...); code != 400 {
		t.Fatal(code, body)
	}

	// A change in the primary checkout in a file of the branch fails the check: 409.
	w.write(filepath.Join(w.root, "app", "lib.go"), "changed\n")
	if code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/merge", `{"removeGroup":false}`, app...); code != 409 {
		t.Fatal(code, body)
	}
	w.git(filepath.Join(w.root, "app"), "checkout", "-q", "lib.go")
	r.clock.add(sendWait)
	code, body = send(t, "POST", tailnet+"/v1/reviews/APP-1/merge", `{"removeGroup":true}`, app...)
	if res := decodeAs[wire.MergeResult](t, body); code != 200 || !res.Merged || !res.Removed {
		t.Fatal(code, body)
	}

	// A plan from loopback.
	root := t.TempDir()
	plan := `{"key":"APP-2","version":1,"root":` + strconv.Quote(root) + `,"blocks":[{"id":"goal","heading":"Goal","markdown":"x"}]}`
	if code, body = send(t, "PUT", loop+"/v1/reviews/app-2/plan", plan); code != 200 || decodeAs[wire.PlanDoc](t, body).Version != 1 {
		t.Fatal(code, body)
	}
	if code, body = send(t, "POST", loop+"/v1/reviews/APP-2/ready", `{"kind":"plan"}`); code != 200 || decodeAs[wire.ReviewSummary](t, body).Kind != wire.ReviewPlan {
		t.Fatal(code, body)
	}
}
