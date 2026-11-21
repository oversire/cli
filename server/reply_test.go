package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, struct {
		A int `json:"a"`
	}{1})
	if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != "{\"a\":1}\n" {
		t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "a <b>")
	if rec.Code != 400 || rec.Body.String() != "{\"error\":\"a \\u003cb\\u003e\"}\n" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestReadJSON(t *testing.T) {
	type body struct {
		A string `json:"a"`
	}
	for _, c := range []struct {
		name, in string
		limit    int64
		err      error
		bad      bool
	}{
		{"good", `{"a":"x"}`, 100, nil, false},
		{"unknown field", `{"b":"x"}`, 100, nil, true},
		{"data after", `{"a":"x"} {}`, 100, nil, true},
		{"bad json", `{"a":`, 100, nil, true},
		{"at the limit", `{"a":"x"}`, 9, nil, false},
		{"over the limit", `{"a":"x"}`, 8, ErrTooLarge, true},
	} {
		var v body
		err := ReadJSON(httptest.NewRequest("POST", "/", strings.NewReader(c.in)), c.limit, &v)
		switch {
		case c.err != nil && !errors.Is(err, c.err):
			t.Errorf("%s: %v", c.name, err)
		case c.err == nil && (err != nil) != c.bad:
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestDecode(t *testing.T) {
	rec := httptest.NewRecorder()
	var v struct{}
	if Decode(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{`)), 100, "bad json: ", &v) {
		t.Fatal("true")
	}
	if rec.Code != 400 || !strings.HasPrefix(rec.Body.String(), `{"error":"bad json: `) {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestRefusalStatus(t *testing.T) {
	for _, c := range []struct {
		k      RefusalKind
		status int
		name   string
	}{
		{NotFound, 404, "notFound"}, {Conflict, 409, "conflict"}, {Invalid, 400, "invalid"}, {0, 500, "RefusalKind(0)"},
	} {
		if s := (&Refusal{Kind: c.k}).Status(); s != c.status {
			t.Errorf("%s: status %d", c.name, s)
		}
		if c.k.String() != c.name {
			t.Errorf("%s: %q", c.name, c.k)
		}
	}
}

func TestFail(t *testing.T) {
	r := Refuse(Conflict, "no %s", "way")
	for _, c := range []struct {
		err  error
		code int
		body string
	}{
		{r, 409, `{"error":"no way"}`},
		{fmt.Errorf("x: %w", r), 409, `{"error":"no way"}`},
		{errors.New("boom"), 500, `{"error":"boom"}`},
	} {
		rec := httptest.NewRecorder()
		Fail(rec, c.err)
		if rec.Code != c.code || strings.TrimSpace(rec.Body.String()) != c.body {
			t.Errorf("%v: %d %q", c.err, rec.Code, rec.Body.String())
		}
	}
}

func TestCallerIsDaemon(t *testing.T) {
	for _, c := range []struct {
		c    Caller
		want bool
	}{
		{Caller{Kind: "daemon", DeviceID: "D"}, true},
		{Caller{Kind: "app", DeviceID: "D"}, false},
		{Caller{Loopback: true}, false},
		{Caller{}, false},
	} {
		if c.c.IsDaemon() != c.want {
			t.Errorf("%+v", c.c)
		}
	}
}

func TestRefuseDaemon(t *testing.T) {
	called := false
	h := RefuseDaemon("no daemon", func(http.ResponseWriter, *http.Request) { called = true })
	for _, c := range []struct {
		name   string
		caller *Caller
		want   bool
	}{
		{"daemon", &Caller{Kind: "daemon", DeviceID: "D"}, false},
		{"app", &Caller{Kind: "app", DeviceID: "D"}, true},
		{"loopback", &Caller{Loopback: true}, true},
		{"none", nil, true},
	} {
		called = false
		req := httptest.NewRequest("POST", "/", nil)
		if c.caller != nil {
			req = req.WithContext(WithCaller(req.Context(), *c.caller))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if called != c.want {
			t.Errorf("%s: called %v", c.name, called)
		}
		if !c.want && (rec.Code != 403 || !strings.Contains(rec.Body.String(), "no daemon")) {
			t.Errorf("%s: %d %q", c.name, rec.Code, rec.Body.String())
		}
	}
}
