package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/oversire/runner/config"
	"github.com/oversire/runner/secret"
	"github.com/oversire/runner/service"
)

type noRun struct{ calls [][]string }

func (n *noRun) Run(_ context.Context, exe string, args []string, _ []byte) ([]byte, []byte, error) {
	n.calls = append(n.calls, append([]string{exe}, args...))
	return nil, nil, nil
}

func testCLI(t *testing.T, stdin string) (*cli, *bytes.Buffer, *noRun) {
	dir := t.TempDir()
	paths := config.Paths{Dir: dir}
	run := &noRun{}
	out := &bytes.Buffer{}
	return &cli{
		paths: paths, in: strings.NewReader(stdin), out: out,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		secrets: secret.New(run, "windows", paths.Token(), nil),
		service: service.NewManager("linux", dir, 1000, "/bin/oversire-runner", run),
		exe:     "/bin/oversire-runner",
	}, out, run
}

func fakePair(t *testing.T, role string, got *map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pair" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(got)
		json.NewEncoder(w).Encode(map[string]string{"token": "abc123", "deviceId": "DEV1", "role": role, "account": "acc", "hookRoute": "h"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPairFromStdin(t *testing.T) {
	var body map[string]string
	srv := fakePair(t, "runner", &body)
	c, out, run := testCLI(t, "  ABCD-1234 \n")
	if err := c.pair(context.Background(), []string{"-", "--hub", srv.URL + "/", "--name", "mini"}); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "abcd-1234" || body["name"] != "mini" {
		t.Fatalf("body %v", body)
	}
	cfg, ok, _ := c.paths.LoadConfig()
	if !ok || cfg.Hub != srv.URL || cfg.Device != "DEV1" || cfg.Account != "acc" || cfg.Name != "mini" {
		t.Fatalf("config %#v", cfg)
	}
	if tok, _ := c.secrets.Get(context.Background(), "DEV1"); tok != "abc123" {
		t.Fatal(tok)
	}
	for _, call := range run.calls {
		if strings.Contains(strings.Join(call, " "), "abc123") || strings.Contains(strings.Join(call, " "), "abcd") {
			t.Fatalf("argv holds a secret: %q", call)
		}
	}
	if !strings.Contains(out.String(), "Paired as mini") {
		t.Fatal(out.String())
	}
	if st, _ := os.Stat(c.paths.Config()); goruntime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", st.Mode())
	}
}

func TestPairFlagsBeforeCode(t *testing.T) {
	var body map[string]string
	srv := fakePair(t, "runner", &body)
	c, _, _ := testCLI(t, "")
	if err := c.pair(context.Background(), []string{"--hub", srv.URL, "WXYZ-0000"}); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "wxyz-0000" || body["name"] == "" {
		t.Fatalf("body %v", body)
	}
}

func TestPairClientCodeFails(t *testing.T) {
	var body map[string]string
	srv := fakePair(t, "client", &body)
	c, _, _ := testCLI(t, "")
	err := c.pair(context.Background(), []string{"ABCD-1234", "--hub", srv.URL})
	if err == nil || err.Error() != "This code is for a client. Make a runner code in the console." {
		t.Fatal(err)
	}
	if _, ok, _ := c.paths.LoadConfig(); ok {
		t.Fatal("a client code paired the runner")
	}
}

func TestStatusJSON(t *testing.T) {
	c, out, _ := testCLI(t, "")
	c.paths.SaveConfig(config.Config{Hub: "https://hub", Device: "DEV1", Name: "mini"})
	if err := c.status(context.Background(), []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "paired", "device", "hub", "connected", "since", "lastError", "service", "runtimes"} {
		if _, ok := r[k]; !ok {
			t.Errorf("status has no %s", k)
		}
	}
	if r["paired"] != true || r["device"] != "DEV1" || r["connected"] != false {
		t.Fatal(r)
	}
}

func TestInstallKeepAwake(t *testing.T) {
	c, _, _ := testCLI(t, "")
	c.paths.SaveConfig(config.Config{Hub: "https://hub", Device: "DEV1"})
	if err := c.install(context.Background(), []string{"--keep-awake"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ := c.paths.LoadConfig(); !cfg.KeepAwake {
		t.Fatal("keepAwake not stored")
	}
}
