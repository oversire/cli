package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oversire/cli/events"
	"github.com/oversire/cli/orders"
	"github.com/oversire/cli/ticket"
	"github.com/oversire/cli/wire"
	"github.com/oversire/cli/worker"
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

// Turns reads GET /v1/sessions/{id}/turns: one page of the turns after seq `after`.
func (d daemonClient) Turns(ctx context.Context, id string, after, limit int) (wire.SessionTurnsPage, error) {
	var p wire.SessionTurnsPage
	q := url.Values{"after": {strconv.Itoa(after)}, "limit": {strconv.Itoa(limit)}}
	err := d.call(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id)+"/turns?"+q.Encode(), nil, &p)
	return p, err
}

// Worker reads GET /v1/workers/{key}. The record stays after the worker stops.
func (d daemonClient) Worker(ctx context.Context, key string) (worker.Record, error) {
	var r worker.Record
	err := d.call(ctx, http.MethodGet, "/v1/workers/"+url.PathEscape(key), nil, &r)
	return r, err
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

// Reviews reads GET /v1/reviews.
func (d daemonClient) Reviews(ctx context.Context) (wire.ReviewsPage, error) {
	var p wire.ReviewsPage
	err := d.call(ctx, http.MethodGet, "/v1/reviews", nil, &p)
	return p, err
}

// Review reads the code review of key. A 404 gives ticket.ErrNoReview.
func (d daemonClient) Review(ctx context.Context, key string) (wire.ReviewDetail, error) {
	var r wire.ReviewDetail
	err := d.call(ctx, http.MethodGet, "/v1/reviews/"+url.PathEscape(key)+"?kind=code", nil, &r)
	if de, ok := errors.AsType[*daemonError](err); ok && de.Status == http.StatusNotFound {
		return r, ticket.ErrNoReview
	}
	return r, err
}

// sessionStopWait is how long Stop waits for the daemon to end the session.
var sessionStopWait = 30 * time.Second

// Stop ends a daemon session with a sessionStop order and waits for its result.
func (d daemonClient) Stop(ctx context.Context, s wire.DaemonSession) error {
	now := time.Now().UTC()
	ref := wire.SessionRef{Workspace: wire.WorkspaceRef{Account: wire.AccountID{Provider: s.Runtime, ID: "local"}, ID: s.Workspace}, ID: s.ID}
	if s.Terminal != "" {
		ref.ID = s.Terminal
	}
	// A direct order names itself as its suggestion, as the app does.
	id := fmt.Sprintf("ticket-stop-%d", now.UnixMilli())
	o := wire.WorkOrder{ID: id, Runner: s.Machine, Runtime: s.Runtime, Suggestion: id, CreatedAt: now,
		ExpiresAt: now.Add(5 * time.Minute), Payload: wire.StopPayload{Session: ref}}
	if err := d.call(ctx, http.MethodPost, "/v1/orders", o, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(sessionStopWait)
	for {
		var st orders.OrderState
		if err := d.call(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(o.ID), nil, &st); err != nil {
			return err
		}
		switch st.State {
		case wire.StateDone:
			return nil
		case wire.StateFailed, wire.StateExpired:
			reason := string(st.State)
			if st.Result != nil && st.Result.Error != "" {
				reason = st.Result.Error
			}
			return fmt.Errorf("The daemon did not stop the session: %s", reason)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("The daemon did not stop the session in %s.", sessionStopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
