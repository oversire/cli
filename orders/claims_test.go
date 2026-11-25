package orders

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oversire/cli/server"
	"github.com/oversire/cli/sessions"
	"github.com/oversire/cli/wire"
)

func TestClaimRoute(t *testing.T) {
	r, _, book := keyRig()
	handStarted(book, "t1:l1", wire.RuntimeOrca)
	var rows []string
	c := NewClaims(r, func(c sessions.Claim, role string) { rows = append(rows, c.Session.Key+" "+role) })
	mux := http.NewServeMux()
	mux.HandleFunc(c.Routes()[0].Pattern, c.Routes()[0].Handle)
	post := func(body string) (int, sessions.Claim) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/claim", strings.NewReader(body)))
		var got sessions.Claim
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	const body = `{"key":"work:A-1","paneKey":"t1:l1","role":"new"}`
	if code, got := post(body); code != 200 || !got.Changed || got.Session.Key != "work:A-1" || len(rows) != 1 || rows[0] != "work:A-1 new" {
		t.Fatalf("first: %d %+v %v", code, got, rows)
	}
	if code, got := post(body); code != 200 || got.Changed || len(rows) != 1 {
		t.Fatalf("again: %d %+v %v", code, got, rows)
	}
	if code, _ := post(`{"key":`); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	if code, _ := post(`{"key":"work:A-1","paneKey":"nope"}`); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
}

func TestClaimRouteIsLoopbackOnly(t *testing.T) {
	if got := NewClaims(nil, nil).Routes()[0].Access; got != server.LoopbackOnly {
		t.Fatalf("access = %v", got)
	}
}
