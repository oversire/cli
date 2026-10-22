package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/group"
	"github.com/oversire/cli/server"
	"github.com/oversire/cli/wire"
)

type memSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSecrets) Set(_ context.Context, a, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[a] = v
	return nil
}

func (s *memSecrets) Get(_ context.Context, a string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[a]; ok {
		return v, nil
	}
	return "", errors.New("none")
}

func (s *memSecrets) Delete(_ context.Context, a string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, a)
}

// groupDaemon serves the group routes as the loopback listener of a daemon, and points the config of c at it.
func groupDaemon(t *testing.T, c *cli) (*group.Store, *group.Joins) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}}); err != nil {
		t.Fatal(err)
	}
	store := group.NewStore(filepath.Join(t.TempDir(), "group.json"), &memSecrets{m: map[string]string{}}, nil, nil)
	if err := store.EnsureCreated(wire.GroupMember{Machine: "mbp", Host: "mbp", Port: 7690}); err != nil {
		t.Fatal(err)
	}
	joins := group.NewJoins(store, nil, nil)
	for _, r := range append(joins.Routes(), group.NewSyncer(store, nil, nil).Routes()...) {
		mux.HandleFunc(r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodGet && req.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "content type", http.StatusForbidden)
				return
			}
			r.Handle(w, req.WithContext(server.WithCaller(req.Context(), server.Caller{Loopback: true})))
		})
	}
	return store, joins
}

func TestGroupCommands(t *testing.T) {
	c, out, _ := testCLI(t, "")
	store, joins := groupDaemon(t, c)
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out.Reset()
		if err := c.group(ctx, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}

	if s := run("show"); !strings.Contains(s, "My machines") || !strings.Contains(s, "mbp  http://mbp:7690  home") {
		t.Fatal(s)
	}
	if s := run("show", "--json"); !strings.Contains(s, `"home": "mbp"`) || strings.Contains(s, "secret") {
		t.Fatal(s)
	}
	if s := run("invite"); !strings.Contains(s, "oversire join mbp ") {
		t.Fatal(s)
	}

	p, err := joins.Request(wire.JoinRequest{Machine: "mba", Host: "mba", Kind: wire.DeviceDaemon, Nonce: "0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if s := run("requests"); !strings.Contains(s, p.Request) || !strings.Contains(s, p.Confirm) {
		t.Fatal(s)
	}
	run("approve", p.Request)
	if g, _ := store.Snapshot(); len(g.Members) != 2 {
		t.Fatalf("%+v", g)
	}
	if s := run("requests"); !strings.Contains(s, "No open") {
		t.Fatal(s)
	}
	run("home", "mba")
	if _, home := store.Info(); home != "mba" {
		t.Fatal(home)
	}
	run("remove", "mba")
	if g, _ := store.Snapshot(); len(g.Members) != 1 || g.Home != "mbp" {
		t.Fatalf("%+v", g)
	}
	run("rotate")
	if s := run("request-code"); !strings.Contains(s, "oversire group admit mbp ") {
		t.Fatal(s)
	}

	out.Reset()
	if err := c.group(ctx, []string{"remove", "nothing"}); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Fatal(err)
	}
	var e *exitError
	if err := c.group(ctx, []string{"approve"}); !errors.As(err, &e) || e.code != 2 {
		t.Fatal(err)
	}
	if err := c.group(ctx, []string{"nope"}); !errors.As(err, &e) || e.code != 2 {
		t.Fatal(err)
	}
}

func TestJoinCommand(t *testing.T) {
	c, _, _ := testCLI(t, "")
	groupDaemon(t, c)
	// The remote host does not answer: the daemon gives the error.
	err := c.join(context.Background(), []string{"127.0.0.1:1", "K7Q2-M9XD"})
	if err == nil || !strings.Contains(err.Error(), "join 127.0.0.1:1") {
		t.Fatal(err)
	}
	var e *exitError
	if err := c.join(context.Background(), []string{"mbp"}); !errors.As(err, &e) || e.code != 2 {
		t.Fatal(err)
	}
}

func TestGroupNoDaemon(t *testing.T) {
	c, _, _ := testCLI(t, "")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	_ = c.paths.SaveConfig(config.Config{Serve: &config.ServeConfig{Port: port}})
	if err := c.group(context.Background(), []string{"show"}); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}
