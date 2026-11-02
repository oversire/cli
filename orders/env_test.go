package orders

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oversire/cli/agentsecret"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

const secretValue = "sk-ant-\"q\"\nVALUE"

// roster is mba in a group with the home mini at base.
type roster struct{ base string }

func (r roster) Snapshot() (wire.Group, bool) { return wire.Group{ID: "g", Home: "mini"}, true }
func (r roster) Self() wire.GroupMember       { return wire.GroupMember{Machine: "mba"} }
func (r roster) HomeEndpoint() (string, string, bool) {
	return r.base, "group-token", false
}

// fakeHome answers POST /v1/agent-secrets/resolve with status and body.
func fakeHome(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in wire.AgentSecretResolve
		if r.URL.Path != "/v1/agent-secrets/resolve" || r.Header.Get("Authorization") != "Bearer group-token" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Machine != "mba" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type failDoer struct{ err error }

func (d failDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

func secretOrder(id string) wire.WorkOrder {
	o := order(id, "claude", now.Add(time.Hour))
	p := o.Payload.(wire.LaunchRequest)
	p.Env = map[string]string{"PLAIN": "1"}
	p.EnvSecrets = map[string]wire.AgentSecretRef{"ANTHROPIC_AUTH_TOKEN": {Agent: "claude", Scope: "shared"}}
	o.Payload = p
	return o
}

// paneRuntimes records each order and gives a pane key, so that the book keeps the session.
type paneRuntimes struct{ fakeRuntimes }

func (f *paneRuntimes) Do(ctx context.Context, o wire.WorkOrder) (*wire.WorkOrderResult, error) {
	out, err := f.fakeRuntimes.Do(ctx, o)
	out.PaneKey = out.Session.ID
	return out, err
}

func envRig(env Env) (*Runner, *paneRuntimes, *sessions.Book) {
	rts := &paneRuntimes{}
	book := sessions.NewBook("mba", "", nil, func() time.Time { return now })
	r := NewRunner(rts, fakeAllow{agent: "claude"}, &memJournal{res: map[string]wire.Result{}, sent: map[string]bool{}}, func() time.Time { return now })
	if env != nil {
		r.WithEnv(env)
	}
	return r.WithKeys(book), rts, book
}

func TestLaunchResolvesTheSecretsFirst(t *testing.T) {
	home := fakeHome(t, http.StatusOK, wire.AgentSecretValues{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": secretValue}})
	r, rts, book := envRig(agentsecret.NewEnv(roster{home.URL}, nil, nil))
	res, _ := r.Run(trusted(), secretOrder("wo-1"), Hooks{})
	if res.State != wire.ResultDone {
		t.Fatalf("result = %+v", res.Result)
	}
	got := rts.calls[0].Payload.(wire.LaunchRequest)
	if got.Env["ANTHROPIC_AUTH_TOKEN"] != secretValue || got.Env["PLAIN"] != "1" || got.EnvSecrets != nil {
		t.Fatalf("launch = %+v", got)
	}
	s, ok := book.Get("s-1")
	if !ok || strings.Join(s.EnvKeys, ",") != "ANTHROPIC_AUTH_TOKEN,PLAIN" {
		t.Fatalf("book = %+v %v", s, ok)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "VALUE") {
		t.Fatalf("the result holds the value: %s", b)
	}
}

func TestLaunchSecretErrors(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	noValue := fakeHome(t, http.StatusNotFound, map[string]string{"error": "mini has no value for ANTHROPIC_AUTH_TOKEN of claude. Type it again in Settings › Agents."})
	runsOn := fakeHome(t, http.StatusForbidden, map[string]string{"error": "claude cannot run on build-box. Its Runs on list is mbp, mba."})
	cases := []struct {
		name string
		env  Env
		want string
	}{
		{"offline", agentsecret.NewEnv(roster{closed.URL}, nil, nil), "mini (home) is offline. claude needs the secret ANTHROPIC_AUTH_TOKEN."},
		{"no route", agentsecret.NewEnv(roster{"http://mini.example"}, nil, failDoer{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}}),
			"mba cannot reach mini (home). A secret needs a route to home."},
		{"no value", agentsecret.NewEnv(roster{noValue.URL}, nil, nil), "mini has no value for ANTHROPIC_AUTH_TOKEN of claude. Type it again in Settings › Agents."},
		{"runs on", agentsecret.NewEnv(roster{runsOn.URL}, nil, nil), "claude cannot run on build-box. Its Runs on list is mbp, mba."},
		{"no filler", nil, "This daemon has no route to the agent secrets."},
	}
	for _, c := range cases {
		r, rts, _ := envRig(c.env)
		res, _ := r.Run(trusted(), secretOrder("wo-"+c.name), Hooks{})
		if res.State != wire.ResultFailed || res.Result.Error != c.want {
			t.Errorf("%s: result = %+v", c.name, res.Result)
		}
		if rts.count() != 0 {
			t.Errorf("%s: the runtime launched", c.name)
		}
	}
}

func TestDirectRefusesABadEnvKey(t *testing.T) {
	g := newRig(t)
	o := order("wo-1", "claude", now.Add(time.Hour))
	p := o.Payload.(wire.LaunchRequest)
	p.Env = map[string]string{"OVERSIRE_SESSION": "x"}
	o.Payload = p
	if code, body := g.post(t, "/v1/orders", o); code != http.StatusBadRequest || !strings.Contains(string(body), "OVERSIRE_SESSION") {
		t.Fatalf("post = %d %s", code, body)
	}
}

func TestDirectLogsNoSecretValue(t *testing.T) {
	home := fakeHome(t, http.StatusOK, wire.AgentSecretValues{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": secretValue}})
	g := newRig(t)
	g.d.runner.WithEnv(agentsecret.NewEnv(roster{home.URL}, nil, nil))
	var buf lockedBuf
	g.d.Log = slog.New(slog.NewTextHandler(&buf, nil))
	o := secretOrder("wo-1")
	p := o.Payload.(wire.LaunchRequest)
	p.Env["PLAIN"] = "plain-VALUE"
	o.Payload = p
	if code, body := g.post(t, "/v1/orders", o); code != http.StatusAccepted {
		t.Fatalf("post = %d %s", code, body)
	}
	g.d.Wait()
	if res := g.results(); len(res) != 1 || res[0].State != wire.ResultDone {
		t.Fatalf("results = %+v", res)
	}
	slog.New(slog.NewTextHandler(&buf, nil)).Info("launch", "req", g.rts.calls[0].Payload)
	if strings.Contains(buf.String(), "VALUE") || !strings.Contains(buf.String(), "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("log = %s", buf.String())
	}
}
