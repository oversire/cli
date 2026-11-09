package worker

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

func serve(t *testing.T, e *Engine) string {
	t.Helper()
	mux := http.NewServeMux()
	for _, r := range NewRoutes(e).Routes() {
		if r.Access != server.LoopbackOnly {
			t.Fatalf("%s is not for loopback", r.Pattern)
		}
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func send(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestRoutesReadAndCommit(t *testing.T) {
	r := newRig(t, turnsTo(5), live("s1", "work:OVERSIRE-12"), wire.DaemonSession{ID: "o1", Runtime: "orca"})
	base := serve(t, r.e)

	code, body := send(t, "GET", base+"/v1/workers/OVERSIRE-12/read?reader=overseer&limit=3", "")
	var got Read
	if code != 200 || json.Unmarshal([]byte(body), &got) != nil || !equalInts(seqs(got.Turns), []int{1, 2, 3}) || got.Next != 3 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ = send(t, "PUT", base+"/v1/workers/s1/cursors/overseer", `{"seq":3}`); code != http.StatusNoContent {
		t.Fatalf("commit %d", code)
	}
	code, body = send(t, "GET", base+"/v1/workers/s1/read?reader=overseer", "")
	if json.Unmarshal([]byte(body), &got); code != 200 || !equalInts(seqs(got.Turns), []int{4, 5}) {
		t.Fatalf("%d %s", code, body)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/v1/workers/nope/read?reader=x", "", 404},
		{"GET", "/v1/workers/s1/read", "", 400},
		{"GET", "/v1/workers/s1/read?reader=x&limit=a", "", 400},
		{"GET", "/v1/workers/s1/read?reader=x&limit=5000", "", 400},
		{"GET", "/v1/workers/o1/read?reader=x", "", 409},
		{"PUT", "/v1/workers/s1/cursors/x", `{"seq":-1}`, 400},
		{"PUT", "/v1/workers/s1/cursors/x", `{"seq":1,"extra":2}`, 400},
		{"PUT", "/v1/workers/nope/cursors/x", `{"seq":1}`, 404},
	} {
		if code, body := send(t, c.method, base+c.path, c.body); code != c.want {
			t.Fatalf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
}
