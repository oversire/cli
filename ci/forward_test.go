package ci

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(r.Context()))
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// member serves h behind a Forward of the machine mba. Home is mbp at base.
func member(t *testing.T, base string, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	f := NewForward(homeIs{machine: "mba", home: "mbp", base: base, token: "tok"}, &http.Client{Timeout: 5 * time.Second})
	srv := httptest.NewServer(f.Wrap(h))
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestForwardToHome(t *testing.T) {
	home := seeded(t)
	var s seen
	hs := httptest.NewServer(s.wrap(home.mux()))
	t.Cleanup(hs.Close)
	m := member(t, hs.URL, func(http.ResponseWriter, *http.Request) { t.Error("the member answered") })
	code, body, _ := fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main")
	_, want, _ := home.get("/v1/ci/status?repo=o/r&ref=main")
	if code != 200 || body != string(want) {
		t.Fatalf("%d %s", code, body)
	}
	if len(s.reqs) != 1 {
		t.Fatalf("%d requests at home", len(s.reqs))
	}
	r := s.reqs[0]
	if r.URL.Path != "/v1/ci/status" || r.URL.RawQuery != "repo=o/r&ref=main" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(ForwardedHeader) != "1" {
		t.Fatalf("%s %s %v", r.URL.Path, r.URL.RawQuery, r.Header)
	}
	// The status and Retry-After of home pass through.
	fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main&fresh=1")
	code, _, h := fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main&fresh=1")
	if code != 429 || h.Get("Retry-After") != "20" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", code, h)
	}
}

func TestForwardNoLoop(t *testing.T) {
	m := member(t, "http://127.0.0.1:1", func(http.ResponseWriter, *http.Request) { t.Error("next ran") })
	code, body, _ := fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main", ForwardedHeader, "1")
	if code != 409 || body != `{"error":"mbp is home","home":"mbp"}`+"\n" {
		t.Fatalf("%d %s", code, body)
	}
}

func TestForwardOffline(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	m := member(t, dead.URL, nil)
	code, body, _ := fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main")
	if code != 503 || body != `{"error":"mbp (home) is offline."}`+"\n" {
		t.Fatalf("%d %s", code, body)
	}
}

func TestForwardHomeRefusesTheToken(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	t.Cleanup(hs.Close)
	m := member(t, hs.URL, nil)
	code, body, _ := fetch(t, m.URL+"/v1/ci/status?repo=o/r&ref=main")
	if code != 502 || body != `{"error":"mba cannot reach mbp (home)."}`+"\n" {
		t.Fatalf("%d %s", code, body)
	}
}

func TestForwardSelf(t *testing.T) {
	ran := false
	f := NewForward(homeIs{machine: "mbp", home: "mbp"}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("an HTTP call went out")
		return nil, io.EOF
	})})
	srv := httptest.NewServer(f.Wrap(func(w http.ResponseWriter, _ *http.Request) { ran = true }))
	t.Cleanup(srv.Close)
	fetch(t, srv.URL+"/v1/ci/status")
	if !ran {
		t.Fatal("next did not run")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
