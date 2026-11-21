package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/wire"
)

// writeDaemon answers the CI writes with res and records the path and the body.
type writeDaemon struct {
	mu   sync.Mutex
	path string
	in   wire.CIActionRequest
	res  wire.CIActionResult
}

func (d *writeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	d.path = r.URL.Path
	_ = json.NewDecoder(r.Body).Decode(&d.in)
	res := d.res
	d.mu.Unlock()
	writeJSON(w, res)
}

func (d *writeDaemon) got() (string, wire.CIActionRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.path, d.in
}

func writeCLI(t *testing.T, res wire.CIActionResult) (*cli, *writeDaemon, *bytes.Buffer) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("OVERSIRE_SESSION", "")
	c, out, _ := testCLI(t, "")
	d := &writeDaemon{res: res}
	fakeDaemon(t, c, d)
	return c, d, out
}

func okResult(summary string) wire.CIActionResult {
	return wire.CIActionResult{OK: true, Summary: summary, Run: &wire.CIRunView{ID: "123", Attempt: 1, Workflow: "CI"}}
}

func TestParseRunArg(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want runArg
	}{
		{"123", runArg{Run: "123"}},
		{"https://github.com/o/r/actions/runs/123", runArg{Repo: "o/r", Run: "123"}},
		{"https://github.com/o/r/actions/runs/123/attempts/2", runArg{Repo: "o/r", Run: "123", Attempt: 2}},
		{"https://github.com/o/r/actions/runs/123/job/456", runArg{Repo: "o/r", Run: "123", Job: "456"}},
	} {
		got, err := parseRunArg(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"abc", "https://github.com/o/r/pull/5", ""} {
		if _, err := parseRunArg(bad); exitCode(err) != 2 {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestRerunFailedWithJobIsUsage(t *testing.T) {
	c, _, _ := writeCLI(t, okResult("ok"))
	if err := c.ci(context.Background(), []string{"rerun", "123", "--failed", "--job", "456", "--repo", "o/r"}); exitCode(err) != 2 {
		t.Fatal(err)
	}
	err := c.ci(context.Background(), []string{"rerun", "https://github.com/o/r/actions/runs/123/job/456", "--failed"})
	if exitCode(err) != 2 {
		t.Fatal(err)
	}
}

func TestRerunFailedRoute(t *testing.T) {
	c, d, out := writeCLI(t, okResult("Run 123 runs again."))
	if err := c.ci(context.Background(), []string{"rerun", "123", "--failed", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
	path, in := d.got()
	if path != "/v1/ci/runs/123/rerun" || !in.FailedOnly || in.Run != "123" || in.Repo != "o/r" || in.Agent != "" {
		t.Fatalf("%s %+v", path, in)
	}
	if want := "Run 123 runs again.\nNext: oversire ci wait 123 --repo o/r\n"; out.String() != want {
		t.Fatalf("%q", out.String())
	}
}

func TestRerunJobURLUsesJobRoute(t *testing.T) {
	c, d, _ := writeCLI(t, okResult("Job 456 runs again."))
	if err := c.ci(context.Background(), []string{"rerun", "https://github.com/o/r/actions/runs/123/job/456"}); err != nil {
		t.Fatal(err)
	}
	path, in := d.got()
	if path != "/v1/ci/jobs/456/rerun" || in.Run != "123" || in.Repo != "o/r" {
		t.Fatalf("%s %+v", path, in)
	}
}

func TestCancelAndApproveBodies(t *testing.T) {
	c, d, _ := writeCLI(t, okResult("Run 123 is cancelled."))
	if err := c.ci(context.Background(), []string{"cancel", "123", "--repo", "gh:o/r"}); err != nil {
		t.Fatal(err)
	}
	if path, in := d.got(); path != "/v1/ci/runs/123/cancel" || in.Repo != "o/r" {
		t.Fatalf("%s %+v", path, in)
	}
	if err := c.ci(context.Background(), []string{"approve", "123", "--repo", "o/r", "--env", "prod", "--env", "stage", "--comment", "LGTM"}); err != nil {
		t.Fatal(err)
	}
	if path, in := d.got(); path != "/v1/ci/runs/123/approve" || strings.Join(in.Environments, ",") != "prod,stage" || in.Comment != "LGTM" {
		t.Fatalf("%s %+v", path, in)
	}
}

func TestDispatchInputs(t *testing.T) {
	c, d, _ := writeCLI(t, okResult("Workflow CI is dispatched on main. Run 123."))
	if err := c.ci(context.Background(), []string{"dispatch", "ci.yml", "--repo", "o/r", "--ref", "main", "-f", "a=1", "-f", "b=x=y"}); err != nil {
		t.Fatal(err)
	}
	path, in := d.got()
	if path != "/v1/ci/dispatch" || in.Workflow != "ci.yml" || in.Ref != "main" || len(in.Inputs) != 2 || in.Inputs["a"] != "1" || in.Inputs["b"] != "x=y" {
		t.Fatalf("%s %+v", path, in)
	}
	if err := c.ci(context.Background(), []string{"dispatch", "ci.yml", "--repo", "o/r", "--ref", "main", "-f", "a"}); exitCode(err) != 2 {
		t.Fatal(err)
	}
}

// gitBranches fakes git: branch, upstream ("" for none) and origin/HEAD ("" for none).
func gitBranches(branch, upstream, originHead string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "remote get-url origin":
			return "git@github.com:o/r.git", nil
		case "rev-parse --abbrev-ref HEAD":
			return branch, nil
		case "rev-parse --abbrev-ref @{upstream}":
			if upstream == "" {
				return "", errors.New("no upstream")
			}
			return upstream, nil
		case "symbolic-ref --short refs/remotes/origin/HEAD":
			if originHead == "" {
				return "", errors.New("no origin/HEAD")
			}
			return originHead, nil
		}
		return "", errors.New("unexpected git call")
	}
}

func TestDispatchRefDefault(t *testing.T) {
	c, d, out := writeCLI(t, wire.CIActionResult{OK: true, Summary: "Workflow CI is dispatched."})
	c.git = gitBranches("feat", "origin/feat", "origin/main")
	if err := c.ci(context.Background(), []string{"dispatch", "ci.yml"}); err != nil {
		t.Fatal(err)
	}
	if _, in := d.got(); in.Ref != "feat" || in.Repo != "o/r" {
		t.Fatalf("%+v", in)
	}
	if !strings.Contains(out.String(), "Ref: feat") {
		t.Fatalf("%q", out.String())
	}
	c.git = gitBranches("local", "", "origin/main")
	if err := c.ci(context.Background(), []string{"dispatch", "ci.yml"}); err != nil {
		t.Fatal(err)
	}
	if _, in := d.got(); in.Ref != "main" {
		t.Fatalf("%+v", in)
	}
	c.git = gitBranches("HEAD", "", "")
	if err := c.ci(context.Background(), []string{"dispatch", "ci.yml"}); exitCode(err) != 2 {
		t.Fatal(err)
	}
}

func TestAgentFromHarness(t *testing.T) {
	c, d, _ := writeCLI(t, okResult("ok"))
	if err := c.ci(context.Background(), []string{"cancel", "123", "--repo", "o/r", "--harness", "claude", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, in := d.got(); in.Agent != "claude s1" {
		t.Fatalf("%+v", in)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "5b6e")
	if err := c.ci(context.Background(), []string{"cancel", "123", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
	if _, in := d.got(); in.Agent != "claude 5b6e" {
		t.Fatalf("%+v", in)
	}
}

func TestNoopExitsZero(t *testing.T) {
	res := okResult("Run 123 attempt 2 is already running. No new rerun.")
	res.Noop = true
	c, _, out := writeCLI(t, res)
	if err := c.ci(context.Background(), []string{"rerun", "123", "--repo", "o/r"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No new rerun.") {
		t.Fatalf("%q", out.String())
	}
}

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   int
		text   string
	}{
		{422, `{"error":"Agents may not cancel runs."}`, 1, "Agents may not cancel runs."},
		{502, `{"error":"GitHub said no."}`, 1, "GitHub said no."},
		{504, `{"error":"mbp did not answer."}`, 1, "mbp did not answer."},
		{409, `{"error":"mbp is home","home":"mbp"}`, 1, "mbp is home"},
		{503, `{"error":"The OverSire app on mbp is not open."}`, 3, "not open"},
		{404, `{"error":"OverSire does not track o/r."}`, 1, "does not track"},
		{404, "404 page not found", 3, "Update it: oversire update"},
	} {
		c, _, _ := testCLI(t, "")
		fakeDaemon(t, c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		err := c.ci(context.Background(), []string{"cancel", "123", "--repo", "o/r"})
		if exitCode(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%d %s: %v (exit %d)", tc.status, tc.body, err, exitCode(err))
		}
	}
	c, _, _ := testCLI(t, "")
	off := false
	if err := c.paths.SaveConfig(config.Config{Serve: &config.ServeConfig{Port: 1, Tailscale: &off}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ci(context.Background(), []string{"cancel", "123", "--repo", "o/r"}); exitCode(err) != 3 {
		t.Fatalf("no daemon: %v", err)
	}
}

func TestJSONPrintsResult(t *testing.T) {
	c, _, out := writeCLI(t, okResult("Run 123 runs again."))
	if err := c.ci(context.Background(), []string{"rerun", "123", "--repo", "o/r", "--json"}); err != nil {
		t.Fatal(err)
	}
	var res wire.CIActionResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || !res.OK || res.Run.ID != "123" {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestRerunWaitUsesRun(t *testing.T) {
	var mu sync.Mutex
	var waited map[string][]string
	c, _, _ := testCLI(t, "")
	fakeDaemon(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ci/runs/123/rerun":
			writeJSON(w, okResult("Run 123 runs again."))
		case "/v1/ci/wait":
			mu.Lock()
			waited = r.URL.Query()
			mu.Unlock()
			writeJSON(w, failedRollup())
		case "/v1/ci/runs/7/failures":
			writeJSON(w, wire.CIFailuresAnswer{Repo: "o/r", Run: "7", Failures: []wire.CIFailureView{}})
		default:
			http.NotFound(w, r)
		}
	}))
	err := c.ci(context.Background(), []string{"rerun", "123", "--repo", "o/r", "--wait", "--timeout", time.Minute.String()})
	if exitCode(err) != 1 {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if waited["ref"][0] != "123" || waited["workflow"][0] != "CI" {
		t.Fatalf("%v", waited)
	}
}
