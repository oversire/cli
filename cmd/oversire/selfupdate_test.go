package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

// daemonAt is a serve config for the loopback port of srv.
func daemonAt(t *testing.T, srv *httptest.Server) config.Config {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port) //nolint:errcheck // httptest gives a number
	return config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: p}}
}

func TestAskUpdate(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   wire.UpdateStatus
		err    error
	}{
		{"installed", http.StatusOK, `{"installed":"0.3.1","latest":"0.3.1"}`, wire.UpdateStatus{Installed: "0.3.1", Latest: "0.3.1"}, nil},
		{"old daemon", http.StatusNotFound, `404 page not found`, wire.UpdateStatus{}, errNoRoute},
		{"refused", http.StatusUnauthorized, `{"error":"member access only"}`, wire.UpdateStatus{Error: "the daemon refused the update (401 member access only)"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The loopback guard of the daemon.
				if r.Header.Get("Content-Type") != "application/json" {
					http.Error(w, `{"error":"content type must be application/json"}`, http.StatusForbidden)
					return
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body)) //nolint:errcheck // test server
			}))
			defer srv.Close()
			got, err := askUpdate(context.Background(), daemonAt(t, srv))
			if err != c.err || got != c.want {
				t.Fatalf("got %#v %v, want %#v %v", got, err, c.want, c.err)
			}
		})
	}
}
