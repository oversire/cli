package agentsecret

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type fakeGroup struct {
	g     wire.Group
	ok    bool
	me    string
	base  string
	token string
}

func (f *fakeGroup) Snapshot() (wire.Group, bool) { return f.g, f.ok }
func (f *fakeGroup) Self() wire.GroupMember       { return wire.GroupMember{Machine: f.me} }
func (f *fakeGroup) HomeEndpoint() (string, string, bool) {
	return f.base, f.token, f.g.Home == f.me
}

func groupOf(me, home string) *fakeGroup {
	return &fakeGroup{g: wire.Group{ID: "g1", Home: home}, ok: true, me: me}
}

type fakeAgents map[string]string

func (f fakeAgents) Record(collection, id string) (json.RawMessage, bool, error) {
	r, ok := f[collection+"/"+id]
	return json.RawMessage(r), ok, nil
}

type fakeEvents struct {
	mu   sync.Mutex
	rows []wire.EventDraft
}

func (f *fakeEvents) AddFor(_ wire.DaemonSession, d wire.EventDraft) {
	f.mu.Lock()
	f.rows = append(f.rows, d)
	f.mu.Unlock()
}

func (f *fakeEvents) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.rows {
		out = append(out, r.Kind)
	}
	return out
}

type homeFixture struct {
	home   *Home
	store  *Store
	events *fakeEvents
	log    *bytes.Buffer
}

func newHome(t *testing.T, g *fakeGroup) *homeFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := NewStore(NewKeychain(nil, "", filepath.Join(dir, "v"), noLook), filepath.Join(dir, "index.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := &fakeEvents{}
	agents := fakeAgents{"agents/claude": `{"name":"claude","runsOn":["mbp","mba"],"env":[],"updatedAt":"2026-11-02T10:00:00Z"}`}
	h := NewHome(st, g, agents, ev, func() time.Time { return time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC) })
	var buf bytes.Buffer
	h.Log = slog.New(slog.NewTextHandler(&buf, nil))
	return &homeFixture{home: h, store: st, events: ev, log: &buf}
}

// handler routes like the server: it sets the caller and dispatches the patterns.
func (f *homeFixture) handler(c server.Caller) http.Handler {
	mux := http.NewServeMux()
	for _, r := range f.home.Routes() {
		mux.HandleFunc(r.Pattern, r.Handle)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(server.WithCaller(r.Context(), c)))
	})
}

func (f *homeFixture) do(c server.Caller, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	f.handler(c).ServeHTTP(w, r)
	return w
}

var (
	app      = server.Caller{DeviceID: "d-app", Kind: string(wire.DeviceApp), Machine: "mbp"}
	loopback = server.Caller{Loopback: true}
	mbaD     = server.Caller{DeviceID: "d-mba", Kind: string(wire.DeviceDaemon), Machine: "mba"}
	boxD     = server.Caller{DeviceID: "d-box", Kind: string(wire.DeviceDaemon), Machine: "build-box"}
)

const token = "sk-ant-SECRET-VALUE"

func TestSetListDelete(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	w := f.do(app, "PUT", "/v1/agent-secrets/shared/claude/ANTHROPIC_AUTH_TOKEN", `{"value":"`+token+`"}`)
	if w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	// The agent * is percent-escaped by the app.
	if w := f.do(loopback, "PUT", "/v1/agent-secrets/shared/%2A/HTTPS_PROXY", `{"value":"http://p"}`); w.Code != 200 {
		t.Fatalf("put * = %d %s", w.Code, w.Body)
	}
	w = f.do(app, "GET", "/v1/agent-secrets", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "http://p") {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	var l wire.AgentSecretList
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil || len(l.Secrets) != 2 || l.Secrets[0].Agent != "*" || l.Secrets[1].Key != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if w := f.do(app, "DELETE", "/v1/agent-secrets/shared/claude/ANTHROPIC_AUTH_TOKEN", ""); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := f.do(app, "DELETE", "/v1/agent-secrets/shared/claude/ANTHROPIC_AUTH_TOKEN", ""); w.Code != 204 {
		t.Fatalf("second delete = %d", w.Code)
	}
	if got := f.events.kinds(); strings.Join(got, ",") != "secret.set,secret.set,secret.set" {
		t.Fatalf("events = %v", got)
	}
	ev := f.events.rows[0]
	if ev.ID != "secret:shared/claude/ANTHROPIC_AUTH_TOKEN:1793613600000" || ev.Via != "app@mbp" || ev.ActorType != wire.ActorHuman {
		t.Fatalf("event = %+v", ev)
	}
	var pl map[string]string
	_ = json.Unmarshal(f.events.rows[2].Payload, &pl)
	if pl["action"] != "removed" || pl["key"] != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("payload = %v", pl)
	}
	noValue(t, f, token)
}

func TestSetChecksThePath(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	for path, want := range map[string]int{
		"/v1/agent-secrets/shared/claude/OVERSIRE_SESSION": 400,
		"/v1/agent-secrets/shared/claude/AGENT_VISOR_HOME": 400,
		"/v1/agent-secrets/shared/claude/ORCA_PANE_KEY":    400,
		"/v1/agent-secrets/shared/claude/1BAD":             400,
		"/v1/agent-secrets/shared/a%2Fb/KEY":               400,
		"/v1/agent-secrets/shared/my%20agent/KEY":          200,
	} {
		if w := f.do(app, "PUT", path, `{"value":"x"}`); w.Code != want {
			t.Errorf("%s = %d %s, want %d", path, w.Code, w.Body, want)
		}
	}
	if w := f.do(app, "PUT", "/v1/agent-secrets/shared/claude/KEY", `{"value":""}`); w.Code != 400 {
		t.Errorf("empty value = %d", w.Code)
	}
	if w := f.do(mbaD, "PUT", "/v1/agent-secrets/shared/claude/KEY", `{"value":"x"}`); w.Code != 403 {
		t.Errorf("daemon put = %d", w.Code)
	}
}

func TestNotHomeIs409(t *testing.T) {
	f := newHome(t, groupOf("mba", "mini"))
	for _, m := range []string{"GET /v1/agent-secrets", "PUT /v1/agent-secrets/shared/claude/KEY", "DELETE /v1/agent-secrets/shared/claude/KEY", "POST /v1/agent-secrets/resolve"} {
		method, path, _ := strings.Cut(m, " ")
		w := f.do(app, method, path, `{"value":"x"}`)
		var b notHome
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		if w.Code != 409 || b.Home != "mini" {
			t.Errorf("%s = %d %s", m, w.Code, w.Body)
		}
	}
}

func (f *homeFixture) set(t *testing.T, scope, agent, key, value string) {
	t.Helper()
	if _, err := f.store.Set(context.Background(), scope, agent, key, value); err != nil {
		t.Fatal(err)
	}
}

func resolveBody(machine string, refs map[string]wire.AgentSecretRef) string {
	b, _ := json.Marshal(wire.AgentSecretResolve{Order: "o1", Machine: machine, Refs: refs})
	return string(b)
}

var claudeRef = map[string]wire.AgentSecretRef{"ANTHROPIC_AUTH_TOKEN": {Agent: "claude", Scope: "shared"}}

func TestResolveGivesTheValuesToAMember(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	f.set(t, "shared", "claude", "ANTHROPIC_AUTH_TOKEN", tricky)
	f.set(t, "shared", "*", "HTTPS_PROXY", "http://p")
	refs := map[string]wire.AgentSecretRef{"ANTHROPIC_AUTH_TOKEN": claudeRef["ANTHROPIC_AUTH_TOKEN"], "HTTPS_PROXY": {Agent: "*", Scope: "shared"}}
	w := f.do(mbaD, "POST", "/v1/agent-secrets/resolve", resolveBody("mba", refs))
	if w.Code != 200 {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	var v wire.AgentSecretValues
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.Env["ANTHROPIC_AUTH_TOKEN"] != tricky || v.Env["HTTPS_PROXY"] != "http://p" {
		t.Fatalf("values = %+v, %v", v, err)
	}
	rows := f.events.rows
	if len(rows) != 1 || rows[0].Kind != "secret.resolved" || rows[0].ID != "secret:resolve:o1:1793613600000" {
		t.Fatalf("events = %+v", rows)
	}
	var pl struct {
		Agent   string   `json:"agent"`
		Keys    []string `json:"keys"`
		Machine string   `json:"machine"`
		Order   string   `json:"order"`
	}
	_ = json.Unmarshal(rows[0].Payload, &pl)
	if pl.Agent != "claude" || strings.Join(pl.Keys, ",") != "ANTHROPIC_AUTH_TOKEN,HTTPS_PROXY" || pl.Machine != "mba" || pl.Order != "o1" {
		t.Fatalf("payload = %+v", pl)
	}
	noValue(t, f, "quoted")
	noValue(t, f, "http://p")
}

func TestResolveRefusals(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	f.set(t, "shared", "claude", "ANTHROPIC_AUTH_TOKEN", token)
	f.set(t, "build-box", "claude", "LOCAL_KEY", token)
	cases := []struct {
		name   string
		caller server.Caller
		body   string
		code   int
		text   string
	}{
		{"app credential", app, resolveBody("mbp", claudeRef), 403, "only a daemon"},
		{"loopback", loopback, resolveBody("mini", claudeRef), 403, "only a daemon"},
		{"loopback daemon kind", server.Caller{Loopback: true, Kind: string(wire.DeviceDaemon), Machine: "mini"}, resolveBody("mini", claudeRef), 403, "only a daemon"},
		{"other machine", mbaD, resolveBody("mbp", claudeRef), 403, "its own machine"},
		{"not on runsOn", boxD, resolveBody("build-box", claudeRef), 403, "claude cannot run on build-box. Its Runs on list is mbp, mba."},
		{"no value", mbaD, resolveBody("mba", map[string]wire.AgentSecretRef{"OTHER": {Agent: "claude", Scope: "shared"}}), 404,
			"mini has no value for OTHER of claude. Type it again in Settings › Agents."},
		{"reserved key", mbaD, resolveBody("mba", map[string]wire.AgentSecretRef{"OVERSIRE_SESSION": {Agent: "claude", Scope: "shared"}}), 400, "valid env keys"},
	}
	for _, c := range cases {
		w := f.do(c.caller, "POST", "/v1/agent-secrets/resolve", c.body)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.text) || strings.Contains(w.Body.String(), token) {
			t.Errorf("%s = %d %s", c.name, w.Code, w.Body)
		}
	}
	// A secret of one Mac has no Runs on check.
	local := map[string]wire.AgentSecretRef{"LOCAL_KEY": {Agent: "claude", Scope: "build-box"}}
	if w := f.do(boxD, "POST", "/v1/agent-secrets/resolve", resolveBody("build-box", local)); w.Code != 200 {
		t.Errorf("device scope = %d %s", w.Code, w.Body)
	}
	if n := len(f.events.rows); n != 1 {
		t.Errorf("events = %d, want only the good resolve", n)
	}
}

func TestForgetDropsTheSharedSecretsOfAGoneAgent(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	f.set(t, "shared", "claude", "A", "1")
	f.set(t, "mba", "claude", "A", "2")
	f.set(t, "shared", "codex", "A", "3")
	f.home.Forget([]wire.StoreOp{{Collection: "agents", ID: "claude", Op: wire.StoreDelete}, {Collection: "notes", ID: "codex", Op: wire.StoreDelete}})
	l := f.store.List()
	if len(l) != 2 || l[0].Scope != "mba" || l[1].Agent != "codex" {
		t.Fatalf("list = %+v", l)
	}
}

// noValue checks that no event row and no log line holds the value.
func noValue(t *testing.T, f *homeFixture, value string) {
	t.Helper()
	rows, _ := json.Marshal(f.events.rows)
	if strings.Contains(string(rows), value) {
		t.Fatalf("an event holds the value: %s", rows)
	}
	if strings.Contains(f.log.String(), value) {
		t.Fatalf("a log line holds the value: %s", f.log)
	}
}
