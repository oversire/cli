package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

func TestStatusServe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(wire.DaemonInfo{Machine: "mbp", Group: "g1", Home: "mba",
			Listen: wire.DaemonListen{Loopback: "127.0.0.1:7690", Tailscale: "100.97.81.23:7690"}})
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	c, out, _ := testCLI(t, "")
	c.paths.SaveConfig(config.Config{Roles: []string{config.RoleServe}, Serve: &config.ServeConfig{Port: port}})
	if err := c.status(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var r statusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	want := serveStatus{Loopback: "127.0.0.1:7690", Tailscale: "100.97.81.23:7690", Group: "g1", Home: "mba"}
	if r.Serve != want {
		t.Fatalf("%+v", r.Serve)
	}
}
