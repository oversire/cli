package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func logged(t *testing.T, h http.Handler, method, target string, header map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	s := &Server{Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	r := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	s.accessLog("loopback", h).ServeHTTP(httptest.NewRecorder(), r)
	return buf.String()
}

func TestAccessLogAFailureHasTheErrorAndTheRequestID(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "wire: order needs id, runner and suggestion")
	})
	line := logged(t, h, "POST", "/v1/orders?token=abc", map[string]string{RequestID: "0a1b2c3d", "Authorization": "Bearer sekret"})
	for _, want := range []string{"level=WARN", "method=POST", "path=/v1/orders", "status=400", "id=0a1b2c3d", `err="wire: order needs id, runner and suggestion"`, "via=loopback"} {
		if !strings.Contains(line, want) {
			t.Errorf("no %q in %q", want, line)
		}
	}
	if strings.Contains(line, "abc") || strings.Contains(line, "sekret") {
		t.Errorf("a query or a header in %q", line)
	}
}

func TestAccessLogLevels(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) })
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "orca: exit 1\nmore", http.StatusBadGateway) })
	for _, c := range []struct {
		h              http.Handler
		method, target string
		level          string
	}{
		{ok, "GET", "/v1/sessions", "level=DEBUG"},
		{ok, "POST", "/v1/orders", "level=INFO"},
		{ok, "POST", "/v1/hooks/claude", "level=DEBUG"},
		{ok, "POST", "/v1/ack", "level=DEBUG"},
		{boom, "GET", "/v1/workspaces", "level=ERROR"},
	} {
		line := logged(t, c.h, c.method, c.target, nil)
		if !strings.Contains(line, c.level) {
			t.Errorf("%s %s: want %s in %q", c.method, c.target, c.level, line)
		}
	}
	if line := logged(t, boom, "GET", "/v1/workspaces", nil); !strings.Contains(line, `err="orca: exit 1 more"`) {
		t.Errorf("no error text in %q", line)
	}
}

func TestAccessLogHasTheCallerOfTheAccessRule(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { setCaller(r.Context(), "mba") })
	if line := logged(t, h, "GET", "/v1/sessions", nil); !strings.Contains(line, "caller=mba") {
		t.Errorf("no caller in %q", line)
	}
}

func TestAccessLogAnswersTheRequestID(t *testing.T) {
	s := &Server{Log: slog.New(slog.DiscardHandler)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/health", nil)
	r.Header.Set(RequestID, "ff00ff00")
	s.accessLog("tailnet", http.NotFoundHandler()).ServeHTTP(w, r)
	if got := w.Header().Get(RequestID); got != "ff00ff00" {
		t.Errorf("got %q", got)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAccessLogAWebSocketOpensAndCloses(t *testing.T) {
	f := newFixture(t, true)
	var buf syncBuf
	f.s.Log = slog.New(slog.NewTextHandler(&buf, nil))
	conn := dial(t, f.net.URL, http.Header{"Authorization": {"Bearer good"}, RequestID: {"abcd1234"}})
	waitClients(t, f.s, 1)
	conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "websocket close") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	line := buf.String()
	for _, want := range []string{"msg=\"websocket open\"", "msg=\"websocket close\"", "via=tailnet", "path=/v1/connect", "id=abcd1234"} {
		if !strings.Contains(line, want) {
			t.Errorf("no %q in %q", want, line)
		}
	}
	if strings.Contains(line, "caller=tailnet") {
		t.Errorf("no device name in %q", line)
	}
}
