package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

// TestServeFeatures starts a test daemon with its own folder and port. whoami lists the CI features,
// and the CI route answers 503 because no app is connected.
func TestServeFeatures(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the daemon")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "oversire")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg := fmt.Sprintf(`{"roles":["serve"],"serve":{"port":%d,"tailscale":false}}`, port)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "daemon", "run")
	cmd.Env = append(os.Environ(), "OVERSIRE_DAEMON_HOME="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var info wire.DaemonInfo
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get(base + "/v1/whoami")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not answer: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, f := range []string{wire.FeatureMirrorCI, wire.FeatureCI} {
		if !slices.Contains(info.Features, f) {
			t.Errorf("features %v have no %q", info.Features, f)
		}
	}
	resp, err := http.Get(base + "/v1/ci/status?repo=o/r&ref=main")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ci status gave %d", resp.StatusCode)
	}
}
