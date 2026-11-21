package ci

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oversire/cli/wire"
)

func homeServer(t *testing.T, s *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ci/wait":
			if r.URL.Query().Get("repo") == "o/x" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"OverSire does not track o/x."}`))
				return
			}
			json.NewEncoder(w).Encode(wire.CIRollup{Repo: "o/r", State: wire.CIRunning, Rev: "r1"})
		case "/v1/ci/watches":
			json.NewEncoder(w).Encode(wire.CIWatchAnswer{Done: false, Notify: "queued"})
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"OverSire does not track o/x."}`))
		}
	})))
	t.Cleanup(srv.Close)
	return srv
}

func TestHomeClientSelf(t *testing.T) {
	var s seen
	local := homeServer(t, &s)
	h := NewHomeClient(homeIs{machine: "mbp", home: "mbp"}, local.URL, http.DefaultClient)
	r, err := h.Wait(context.Background(), WaitQuery{Repo: "o/r", Ref: "main", Since: "r0", Timeout: 30 * time.Second, Settle: time.Minute, FailFast: true,
		Started: time.Unix(100, 0), Workflows: []string{"CI"}})
	if err != nil || r.Rev != "r1" {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := h.AddWatch(context.Background(), wire.CIWatchRequest{Repo: "o/r", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if len(s.reqs) != 2 {
		t.Fatalf("%d requests", len(s.reqs))
	}
	w := s.reqs[0]
	q := w.URL.Query()
	if w.Header.Get("Authorization") != "" || w.Header.Get(ForwardedHeader) != "" || q.Get("repo") != "o/r" || q.Get("ref") != "main" || q.Get("since") != "r0" ||
		q.Get("timeout") != "30" || q.Get("settle") != "60" || q.Get("started") != "100" || q.Get("failFast") != "1" || q.Get("workflow") != "CI" {
		t.Fatalf("%s %v", w.URL, w.Header)
	}
	if p := s.reqs[1]; p.Method != "POST" || p.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %v", p.Method, p.Header)
	}
}

func TestHomeClientForward(t *testing.T) {
	var s seen
	home := homeServer(t, &s)
	h := NewHomeClient(homeIs{machine: "mba", home: "mbp", base: home.URL, token: "tok"}, "http://127.0.0.1:1", http.DefaultClient)
	if _, err := h.AddWatch(context.Background(), wire.CIWatchRequest{Repo: "o/r", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	r := s.reqs[0]
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(ForwardedHeader) != "1" || r.URL.Path != "/v1/ci/watches" {
		t.Fatalf("%s %v", r.URL, r.Header)
	}
}

func TestHomeClientErrors(t *testing.T) {
	var s seen
	home := homeServer(t, &s)
	h := NewHomeClient(homeIs{machine: "mba", home: "mbp", base: home.URL, token: "tok"}, "", http.DefaultClient)
	var he *HomeError
	if _, err := h.Wait(context.Background(), WaitQuery{Repo: "o/x", Ref: "main"}); !errors.As(err, &he) || he.Status != 404 || he.Text != "OverSire does not track o/x." {
		t.Fatalf("%v", err)
	}
	off := NewHomeClient(homeIs{machine: "mba", home: "mbp", base: "http://127.0.0.1:1", token: "tok"}, "", http.DefaultClient)
	if _, err := off.Wait(context.Background(), WaitQuery{Repo: "o/r", Ref: "main"}); !errors.As(err, &he) || he.Status != 0 || he.Text != "mbp (home) is offline." {
		t.Fatalf("%v", err)
	}
}
