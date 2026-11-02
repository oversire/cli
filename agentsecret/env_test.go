package agentsecret

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

func launch(refs map[string]wire.AgentSecretRef) wire.LaunchRequest {
	return wire.LaunchRequest{Repo: "r", Agent: "claude", Prompt: "p", Env: map[string]string{"PLAIN": "1"}, EnvSecrets: refs}
}

// remoteHome serves a real Home behind a server that gives the credential of caller.
func remoteHome(t *testing.T, caller server.Caller) (*homeFixture, *httptest.Server) {
	t.Helper()
	f := newHome(t, groupOf("mini", "mini"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer group-token" {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		f.handler(caller).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func runner(me, base string) *fakeGroup {
	g := groupOf(me, "mini")
	g.base, g.token = base, "group-token"
	return g
}

func TestFillOnARunnerCallsHome(t *testing.T) {
	f, srv := remoteHome(t, mbaD)
	f.set(t, "shared", "claude", "ANTHROPIC_AUTH_TOKEN", tricky)
	r, err := NewEnv(runner("mba", srv.URL), nil, nil).Fill(context.Background(), "o1", launch(claudeRef))
	if err != nil {
		t.Fatal(err)
	}
	if r.Env["ANTHROPIC_AUTH_TOKEN"] != tricky || r.Env["PLAIN"] != "1" || r.EnvSecrets != nil {
		t.Fatalf("filled = %+v", r)
	}
	if len(f.events.rows) != 1 || f.events.rows[0].Kind != "secret.resolved" {
		t.Fatalf("events = %+v", f.events.rows)
	}
}

func TestFillOnHomeResolvesInProcess(t *testing.T) {
	f := newHome(t, groupOf("mini", "mini"))
	f.set(t, "shared", "codex", "ANTHROPIC_AUTH_TOKEN", token)
	in := launch(map[string]wire.AgentSecretRef{"ANTHROPIC_AUTH_TOKEN": {Agent: "codex", Scope: "shared"}})
	r, err := NewEnv(f.home.group.(*fakeGroup), f.home, nil).Fill(context.Background(), "o1", in)
	if err != nil || r.Env["ANTHROPIC_AUTH_TOKEN"] != token {
		t.Fatalf("filled = %+v, %v", r, err)
	}
	if _, ok := in.Env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("Fill changed the env map of the caller")
	}
}

type failDoer struct{ err error }

func (d failDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

func TestFillErrors(t *testing.T) {
	_, srv := remoteHome(t, mbaD)
	_, boxSrv := remoteHome(t, boxD)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	cases := []struct {
		name string
		env  *Env
		want string
	}{
		{"offline", NewEnv(runner("mba", closed.URL), nil, nil),
			"mini (home) is offline. claude needs the secret ANTHROPIC_AUTH_TOKEN."},
		{"no route", NewEnv(runner("mba", srv.URL), nil, failDoer{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}}),
			"mba cannot reach mini (home). A secret needs a route to home."},
		{"no endpoint", NewEnv(runner("mba", ""), nil, nil),
			"mba cannot reach mini (home). A secret needs a route to home."},
		{"no value", NewEnv(runner("mba", srv.URL), nil, nil),
			"mini has no value for ANTHROPIC_AUTH_TOKEN of claude. Type it again in Settings › Agents."},
		{"runs on", NewEnv(runner("build-box", boxSrv.URL), nil, nil),
			"claude cannot run on build-box. Its Runs on list is mbp, mba."},
	}
	for _, c := range cases {
		_, err := c.env.Fill(context.Background(), "o1", launch(claudeRef))
		var e *Error
		if !errors.As(err, &e) || e.Text != c.want {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestFillChecksTheKeysFirst(t *testing.T) {
	_, err := NewEnv(runner("mba", ""), nil, nil).Fill(context.Background(), "o1", wire.LaunchRequest{Agent: "claude", Env: map[string]string{"OVERSIRE_SESSION": "x"}})
	if err == nil || !strings.Contains(err.Error(), "OVERSIRE_SESSION") {
		t.Fatalf("err = %v", err)
	}
}
