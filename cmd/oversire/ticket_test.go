package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oversire/cli/config"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
)

// TestSkillCommandsExist fails when the skill body names an oversire command that the CLI does not have.
func TestSkillCommandsExist(t *testing.T) {
	cmd := regexp.MustCompile("`oversire ([a-z]+)(?: ([a-z]+))?")
	s := ticket.NewSkill()
	n := 0
	fs.WalkDir(s.Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(s.Files(), p)
		for _, m := range cmd.FindAllStringSubmatch(string(b), -1) {
			n++
			if !usageHas(m[1], m[2]) {
				t.Errorf("%s names %q, which is not in the usage", p, m[0])
			}
		}
		return nil
	})
	if n < 5 {
		t.Fatalf("only %d commands found", n)
	}
}

func usageHas(cmd, sub string) bool {
	for _, line := range strings.Split(usage, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "oversire "+cmd+" ")
		if !ok {
			continue
		}
		if sub == "" {
			return true
		}
		words := strings.FieldsFunc(strings.Fields(rest)[0], func(r rune) bool { return r == '|' })
		for _, w := range words {
			if w == sub {
				return true
			}
		}
	}
	return false
}

func ticketTest(t *testing.T) (*cli, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	return &cli{out: out, home: t.TempDir(), wd: "/w", paths: config.Paths{Dir: t.TempDir()}}, out
}

// putTranscript puts a Claude transcript of session id under the fake home.
func putTranscript(t *testing.T, home, id string) {
	t.Helper()
	b, err := os.ReadFile("../../contract/fixtures/session-digest/claude/long.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude", "projects", "-w", id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, b, 0o600)
}

func TestTicketWorkPrintsEarlierDigest(t *testing.T) {
	c, out := ticketTest(t)
	putTranscript(t, c.home, "s1")
	if err := c.ticket([]string{"new", "oversire-7", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.ticket([]string{"work", "OVERSIRE-7", "--session", "s2"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OVERSIRE-7: this session (claude s2) is bound (work)", "Earlier sessions:", "new   claude s1",
		"Digest of session s1", "### Prompt"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	got, _ := c.ledger().Sessions("OVERSIRE-7")
	if len(got) != 2 {
		t.Fatal(got)
	}
}

func TestTicketWorkTranscriptElsewhere(t *testing.T) {
	c, out := ticketTest(t)
	c.ledger().Add(ticket.SessionRecord{Key: "A-1", Harness: "claude", Session: "s1", Machine: "far", Role: "new"})
	if err := c.ticket([]string{"work", "A-1", "--session", "s2"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ssh far oversire digest --harness claude --session s1") {
		t.Fatal(out)
	}
}

func TestTicketDoneUsesBoundKey(t *testing.T) {
	c, out := ticketTest(t)
	closedPort(t, c)
	putTranscript(t, c.home, "s1")
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	dir := t.TempDir()
	if err := c.ticket([]string{"done", "--session", "s1", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "claude-session-s1.jsonl.gz") + "\tclaude-session-s1.jsonl.gz\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Fatalf("%q", out)
	}
	if items, _ := c.ledger().Items("s1"); len(items) != 0 {
		t.Fatal(items)
	}
}

func TestTicketDoneNoKeyTwoItems(t *testing.T) {
	c, _ := ticketTest(t)
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	c.ticket([]string{"new", "B-2", "--session", "s1"})
	err := c.ticket([]string{"done", "--session", "s1"})
	var e *exitError
	if !errors.As(err, &e) || !strings.Contains(err.Error(), "A-1, B-2") {
		t.Fatal(err)
	}
}

func TestTicketSkillBody(t *testing.T) {
	c, out := ticketTest(t)
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	if err := c.ticket([]string{"skill", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Tickets of this session: A-1") {
		t.Fatal(out)
	}
	out.Reset()
	if err := c.ticket([]string{"skill", "spec"}); err != nil || !strings.Contains(out.String(), "## Changes") {
		t.Fatal(err, out)
	}
	if err := c.ticket([]string{"skill", "enrich"}); err == nil {
		t.Fatal("no error for enrich")
	}
	if err := c.ticket([]string{"work"}); err == nil {
		t.Fatal("no error without a key")
	}
}

func TestTicketPlanManyKeys(t *testing.T) {
	c, out := ticketTest(t)
	if err := c.ticket([]string{"plan", "a-1", "B-2", "a-1", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if items, _ := c.ledger().Items("s1"); !slices.Equal(items, []string{"A-1", "B-2"}) {
		t.Fatal(items)
	}
	if n := strings.Count(out.String(), "is bound (plan)"); n != 2 || strings.Contains(out.String(), "Digest of session") {
		t.Fatal(out)
	}
	if !strings.Contains(out.String(), "B-2: no earlier sessions on this machine") {
		t.Fatal(out)
	}
}

func TestTicketPlanBadKeyBindsNone(t *testing.T) {
	c, _ := ticketTest(t)
	var e *exitError
	if err := c.ticket([]string{"plan", "A-1", "nokey", "--session", "s1"}); !errors.As(err, &e) || e.code != 2 {
		t.Fatal(err)
	}
	if items, _ := c.ledger().Items("s1"); len(items) != 0 {
		t.Fatal(items)
	}
}

func TestTicketPlanTooManyKeys(t *testing.T) {
	c, _ := ticketTest(t)
	args := []string{"plan", "--session", "s1"}
	for i := 1; i <= 21; i++ {
		args = append(args, fmt.Sprintf("A-%d", i))
	}
	var e *exitError
	if err := c.ticket(args); !errors.As(err, &e) || e.code != 2 || !strings.Contains(err.Error(), "at most 20") {
		t.Fatal(err)
	}
}

func TestTicketWorkOneKey(t *testing.T) {
	c, _ := ticketTest(t)
	for _, verb := range []string{"work", "new"} {
		var e *exitError
		if err := c.ticket([]string{verb, "A-1", "B-2", "--session", "s1"}); !errors.As(err, &e) || e.code != 2 {
			t.Fatalf("%s: %v", verb, err)
		}
	}
}

func TestTicketGroom(t *testing.T) {
	c, out, _ := testCLI(t, "")
	book, _ := reviewDaemon(t, c)
	if err := c.ticket([]string{"groom", "trebi", "--session", "s9"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"key groom:TREBI is free", "Items with a live session: none."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	rt, key, working := "oversire", "groom:TREBI", wire.PhaseWorking
	book.Apply(sessions.Patch{ID: "g1", Source: sessions.SourceWire, Runtime: &rt, Key: &key, State: &working})
	err := c.ticket([]string{"groom", "trebi", "--session", "s9"})
	var e *exitError
	if !errors.As(err, &e) || e.code != 1 || !strings.Contains(err.Error(), "A Groom session of TREBI runs") {
		t.Fatal(err)
	}
}

func TestTicketGroomNoDaemon(t *testing.T) {
	c, out, _ := testCLI(t, "")
	closedPort(t, c)
	if err := c.ticket([]string{"groom", "TREBI"}); err != nil || !strings.Contains(out.String(), "No daemon answers") {
		t.Fatal(err, out)
	}
}

// closedPort points the config of c at a loopback port with no listener.
func closedPort(t *testing.T, c *cli) {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := c.paths.SaveConfig(config.Config{Serve: &config.ServeConfig{Port: port}}); err != nil {
		t.Fatal(err)
	}
}

// fakeDaemon serves h on a loopback port and points the config of c at it.
func fakeDaemon(t *testing.T, c *cli, h http.Handler) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	off := false
	if err := c.paths.SaveConfig(config.Config{Serve: &config.ServeConfig{Port: port, Tailscale: &off}}); err != nil {
		t.Fatal(err)
	}
}

func TestTicketAppliedPosts(t *testing.T) {
	c, out, _ := testCLI(t, "")
	var got []wire.EventDraft
	fakeDaemon(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		var body wire.EventAppend
		json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body.Events...)
		json.NewEncoder(w).Encode(wire.EventAppendResult{Accepted: len(body.Events), Head: 9})
	}))
	f := filepath.Join(t.TempDir(), "applied.json")
	os.WriteFile(f, []byte(`[{"item":"TREBI-12","summary":"Merged into TREBI-10","payload":{"kind":"merge"}},
		{"item":"TREBI-14","summary":"Done: shipped","payload":{"kind":"done"}}]`), 0o600)
	if err := c.ticket([]string{"applied", "groom", "--file", f, "--project", "TREBI", "--session", "s1"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != wire.KindGroomApplied || got[0].Session != "s1" || !strings.Contains(out.String(), "2 event rows written") {
		t.Fatal(got, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`[{"item":"nokey","summary":"x"}]`), 0o600)
	var e *exitError
	if err := c.ticket([]string{"applied", "groom", "--file", bad}); !errors.As(err, &e) || e.code != 2 || len(got) != 2 {
		t.Fatal(err, len(got))
	}
	closedPort(t, c)
	out.Reset()
	if err := c.ticket([]string{"applied", "groom", "--file", f, "--session", "s1"}); err != nil || !strings.Contains(out.String(), "No daemon answers") {
		t.Fatal(err, out)
	}
}

func TestTicketSweepJSON(t *testing.T) {
	c, out, _ := testCLI(t, "")
	reviewDaemon(t, c)
	root, wt := gitGroup(t)
	c.wd = wt
	if err := c.dispatch(context.Background(), []string{"review", "ready", "--note", "Adds N."}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := c.ticket([]string{"sweep", "APP-1", "--json", "--root", root}); err != nil {
		t.Fatal(err)
	}
	var rep ticket.SweepReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err, out)
	}
	if !rep.Daemon || len(rep.Rows) != 1 {
		t.Fatalf("%+v", rep)
	}
	r := rep.Rows[0]
	if r.Key != "APP-1" || r.Group == "" || r.Review != wire.ReviewReadyState || r.Signal != ticket.SignalReady {
		t.Fatalf("%+v", r)
	}
}

func TestTicketStop(t *testing.T) {
	c, out, _ := testCLI(t, "")
	var posted []wire.WorkOrder
	fakeDaemon(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			json.NewEncoder(w).Encode(wire.DaemonSessionsPage{Sessions: []wire.DaemonSession{
				{ID: "s1", Machine: "mbp", Runtime: "orca", Workspace: "ws1", Terminal: "term-1", HarnessSession: "h1", State: wire.PhaseWaiting},
				{ID: "cli:c1", Runtime: wire.RuntimeCLI, HarnessSession: "c1", State: wire.PhaseWorking},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/orders":
			var o wire.WorkOrder
			if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			posted = append(posted, o)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"order": o})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/orders/"):
			json.NewEncoder(w).Encode(map[string]any{"state": wire.StateDone})
		default:
			http.NotFound(w, r)
		}
	}))
	if err := c.ticket([]string{"stop", "s1", "--session", "me"}); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || !strings.Contains(out.String(), "s1: stopped") {
		t.Fatal(posted, out)
	}
	o := posted[0]
	stop, ok := o.Payload.(wire.StopPayload)
	if !ok || stop.Session.ID != "term-1" || stop.Session.Workspace.ID != "ws1" || o.Runtime != "orca" || o.Runner != "mbp" || !o.ExpiresAt.After(time.Now()) {
		t.Fatalf("%+v", o)
	}
	var e *exitError
	for _, args := range [][]string{{"stop", "c1"}, {"stop", "s1", "--session", "s1"}, {"stop", "h1", "--session", "h1"}, {"stop", "nope"}} {
		if err := c.ticket(args); !errors.As(err, &e) || e.code != 2 {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(posted) != 1 {
		t.Fatal(len(posted))
	}
}

// turnsDaemon serves the worker A-1 and n turns for its session ws1, in pages of the requested limit.
func turnsDaemon(t *testing.T, c *cli, n int) {
	t.Helper()
	fakeDaemon(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/workers/A-1":
			json.NewEncoder(w).Encode(worker.Record{Session: "ws1", Key: "work:A-1"})
		case r.URL.Path == "/v1/sessions/ws1/turns":
			after, _ := strconv.Atoi(r.URL.Query().Get("after"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			p := wire.SessionTurnsPage{Turns: []wire.SessionTurn{}}
			for seq := after + 1; seq <= n && len(p.Turns) < limit; seq++ {
				p.Turns = append(p.Turns, wire.SessionTurn{Seq: seq, Role: wire.RoleAssistant, Text: "turn"})
			}
			p.More = len(p.Turns) > 0 && p.Turns[len(p.Turns)-1].Seq < n
			json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
}

func doneWithWorker(t *testing.T, n int) (string, string) {
	t.Helper()
	c, out := ticketTest(t)
	turnsDaemon(t, c, n)
	putTranscript(t, c.home, "s1")
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	dir := t.TempDir()
	if err := c.ticket([]string{"done", "--session", "s1", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	return dir, out.String()
}

func gzipLines(t *testing.T, p string) int {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(z)
	return bytes.Count(b, []byte("\n"))
}

func TestTicketDonePacksTheWorkerTurns(t *testing.T) {
	dir, out := doneWithWorker(t, 3)
	p := filepath.Join(dir, "A-1-worker-turns.jsonl.gz")
	if !strings.Contains(out, p+"\tA-1-worker-turns.jsonl.gz\n") || gzipLines(t, p) != 3 {
		t.Fatal(out)
	}
}

func TestTicketDoneReadsAllPages(t *testing.T) {
	dir, _ := doneWithWorker(t, 250)
	if n := gzipLines(t, filepath.Join(dir, "A-1-worker-turns.jsonl.gz")); n != 250 {
		t.Fatal(n)
	}
}

func TestTicketDoneWithoutAWorkerPacksOnlyTheTranscript(t *testing.T) {
	c, out := ticketTest(t)
	fakeDaemon(t, c, http.NotFoundHandler())
	putTranscript(t, c.home, "s1")
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	dir := t.TempDir()
	if err := c.ticket([]string{"done", "--session", "s1", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "No worker turns for A-1") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(dir, "A-1-worker-turns.jsonl.gz")); err == nil {
		t.Fatal("a worker file")
	}
}

func TestTicketDoneWithoutADaemonContinues(t *testing.T) {
	c, out := ticketTest(t)
	closedPort(t, c)
	putTranscript(t, c.home, "s1")
	c.ticket([]string{"new", "A-1", "--session", "s1"})
	out.Reset()
	dir := t.TempDir()
	if err := c.ticket([]string{"done", "--session", "s1", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "No worker turns for A-1") {
		t.Fatal(out)
	}
	if items, _ := c.ledger().Items("s1"); len(items) != 0 {
		t.Fatal("the ticket stays open", items)
	}
}
