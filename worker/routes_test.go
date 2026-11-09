package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/runtime"
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

func TestRoutesWaitAndBrief(t *testing.T) {
	r := newRig(t, turnsTo(3), live("s1", "work:OVERSIRE-12"))
	r.e.Poll = 5 * time.Millisecond
	r.book.setPhase("s1", wire.PhaseWaiting, wire.ReasonTurnEnd)
	base := serve(t, r.e)

	code, body := send(t, "GET", base+"/v1/workers/OVERSIRE-12/wait?max=1s", "")
	var w Wake
	if code != 200 || json.Unmarshal([]byte(body), &w) != nil || w.Reason != WakeInput {
		t.Fatalf("%d %s", code, body)
	}
	code, body = send(t, "GET", base+"/v1/workers/s1/brief", "")
	var b Brief
	if code != 200 || json.Unmarshal([]byte(body), &b) != nil || b.Last != "text" {
		t.Fatalf("%d %s", code, body)
	}
	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/workers/s1/wait?max=60s", 400},
		{"/v1/workers/s1/wait?max=abc", 400},
		{"/v1/workers/nope/wait", 404},
		{"/v1/workers/nope/brief", 404},
	} {
		if code, body := send(t, "GET", base+c.path, ""); code != c.want {
			t.Fatalf("%s: %d %s", c.path, code, body)
		}
	}
}

func TestRoutesLifecycle(t *testing.T) {
	r := newRig(t, nil, overseen("s1", "work:OVERSIRE-12"))
	base := serve(t, r.e)
	r.watch.set("s1", runtime.Inspection{Session: wire.RunnerSession{State: wire.SessionWaitingInput,
		Question: &wire.RunnerQuestion{ID: "q-1", Items: []wire.RunnerQuestionItem{{ID: "i1", Text: "Which?"}}}}})

	code, body := send(t, "POST", base+"/v1/workers", `{"key":"oversire-12","harness":"codex","dir":"/w/.wt/oversire-12","overseer":"ov1","budget":7200000000000}`)
	var st Started
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || st.Reused || st.Record.Session != "s1" || st.Record.Budget != 2*time.Hour {
		t.Fatalf("start %d %s", code, body)
	}
	code, body = send(t, "GET", base+"/v1/workers/OVERSIRE-12", "")
	var rec Record
	if code != 200 || json.Unmarshal([]byte(body), &rec) != nil || rec.Harness != "codex" || rec.State != StateWorking {
		t.Fatalf("get %d %s", code, body)
	}
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/send", `{"text":"Go on."}`, 204},
		{"/answer", `{"question":"q-1","text":"Use sqlite."}`, 204},
		{"/answer", `{"permission":"p-1","allow":false}`, 204},
		{"/interrupt", ``, 204},
		{"/escalate", `{"question":"Which db?"}`, 204},
		{"/escalate", `{"question":"Again?"}`, 409},
		{"/send", `{"text":""}`, 400},
		{"/send", `{"text":"x","extra":1}`, 400},
		{"/answer", `{"permission":"p-1"}`, 400},
		{"/done", `{"verdict":"pass"}`, 400},
		{"/done", `{"verdict":"done","note":"ok"}`, 200},
		{"/stop", ``, 204},
	} {
		if code, body := send(t, "POST", base+"/v1/workers/s1"+c.path, c.body); code != c.want {
			t.Fatalf("%s %s: %d %s", c.path, c.body, code, body)
		}
	}
	if rec, _ := r.store.Load("s1"); rec.State != StateStopped || rec.Verdict != "done" {
		t.Fatalf("record %+v", rec)
	}
	if code, _ := send(t, "POST", base+"/v1/workers/nope/stop", ""); code != 404 {
		t.Fatalf("unknown %d", code)
	}
	if code, _ := send(t, "POST", base+"/v1/workers", `{"key":""}`); code != 400 {
		t.Fatalf("bad start %d", code)
	}
}
