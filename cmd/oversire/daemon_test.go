package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oversire/cli/ticket"
)

func TestDaemonCallStatus(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	d := daemonClient{base: srv.URL, http: srv.Client()}
	err := d.call(context.Background(), http.MethodGet, "/v1/x", nil, nil)
	var de *daemonError
	if !errors.As(err, &de) || de.Status != 404 || !strings.Contains(err.Error(), "The daemon answered 404") {
		t.Fatal(err)
	}
	if errors.Is(err, ticket.ErrNoDaemon) {
		t.Fatal("a 404 is not ErrNoDaemon")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	d = daemonClient{base: "http://" + addr, http: http.DefaultClient}
	if err := d.call(context.Background(), http.MethodGet, "/v1/x", nil, nil); !errors.Is(err, ticket.ErrNoDaemon) ||
		!strings.Contains(err.Error(), "does not answer") {
		t.Fatal(err)
	}
}

func TestDaemonReview404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"no review A-1"}`))
	}))
	defer srv.Close()
	d := daemonClient{base: srv.URL, http: srv.Client()}
	if _, err := d.Review(context.Background(), "A-1"); !errors.Is(err, ticket.ErrNoReview) {
		t.Fatal(err)
	}
}
