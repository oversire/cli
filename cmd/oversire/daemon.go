package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
)

// daemonClient calls the loopback API of the local daemon.
type daemonClient struct {
	base string
	http *http.Client
}

// daemonError is a failed daemon call. Status 0: the daemon did not answer.
type daemonError struct {
	Status int
	Msg    string
}

func (e *daemonError) Error() string { return e.Msg }

func (e *daemonError) Unwrap() error {
	if e.Status == 0 {
		return ticket.ErrNoDaemon
	}
	return nil
}

func (c *cli) daemonClient() (daemonClient, error) {
	cfg, _, err := c.paths.LoadConfig()
	if err != nil {
		return daemonClient{}, err
	}
	port := cfg.ServeOrDefault().ListenPort()
	return daemonClient{base: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// call sends body as JSON and decodes the answer into out.
func (d daemonClient) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return &daemonError{Msg: fmt.Sprintf("The daemon does not answer on %s. Start it with: oversire daemon install", strings.TrimPrefix(d.base, "http://"))}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &daemonError{Status: resp.StatusCode, Msg: e.Error}
		}
		return &daemonError{Status: resp.StatusCode, Msg: fmt.Sprintf("The daemon answered %s: %s", resp.Status, strings.TrimSpace(string(data)))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("The daemon answer is not valid: %w", err)
		}
	}
	return nil
}

// Sessions reads GET /v1/sessions.
func (d daemonClient) Sessions(ctx context.Context) ([]wire.DaemonSession, error) {
	var p wire.DaemonSessionsPage
	if err := d.call(ctx, http.MethodGet, "/v1/sessions", nil, &p); err != nil {
		return nil, err
	}
	return p.Sessions, nil
}

// AppendEvents posts rows to POST /v1/events, at most events.MaxAppend in one call.
func (d daemonClient) AppendEvents(ctx context.Context, rows []wire.EventDraft) (wire.EventAppendResult, error) {
	var sum wire.EventAppendResult
	for len(rows) > 0 {
		n := min(len(rows), events.MaxAppend)
		var r wire.EventAppendResult
		if err := d.call(ctx, http.MethodPost, "/v1/events", wire.EventAppend{Events: rows[:n]}, &r); err != nil {
			return sum, err
		}
		sum.Accepted += r.Accepted
		sum.Head = r.Head
		rows = rows[n:]
	}
	return sum, nil
}
