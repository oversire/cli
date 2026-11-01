package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/oversire/cli/wire"
)

const (
	outboxBatch = 200
	outboxIdle  = 30 * time.Second
	outboxMin   = 2 * time.Second
	outboxMax   = 5 * time.Minute
)

// Home tells where the home daemon is (group.Store). self is true when this daemon is home, or has no other home.
type Home interface {
	HomeEndpoint() (base, token string, self bool)
}

// Doer sends one HTTP request (*http.Client).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Outbox sends the rows of a member to POST /v1/events of the home, at least once, in seq order, with backoff.
// The home drops a repeat by its id. The at time stays the time of the event.
type Outbox struct {
	store *Store
	home  Home
	http  Doer
	log   *slog.Logger
	poke  chan struct{}
}

func NewOutbox(store *Store, home Home, client Doer, log *slog.Logger) *Outbox {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	o := &Outbox{store: store, home: home, http: client, log: log, poke: make(chan struct{}, 1)}
	store.QueueWhen(o.Member)
	store.Subscribe(func([]wire.Event) { o.Poke() })
	return o
}

// Member is true when this daemon is not home: its new rows go to the outbox.
func (o *Outbox) Member() bool {
	_, _, self := o.home.HomeEndpoint()
	return !self
}

func (o *Outbox) Poke() {
	select {
	case o.poke <- struct{}{}:
	default:
	}
}

// Run sends the outbox after each new row and every 30 s. A failure waits 2 s, then twice as long each time, at most 5 min.
func (o *Outbox) Run(ctx context.Context) error {
	wait := outboxMin
	for {
		next := outboxIdle
		if _, err := o.Flush(ctx); err != nil {
			o.log.Debug("events: outbox", "err", err, "retry", wait)
			next, wait = wait, min(wait*2, outboxMax)
		} else {
			wait = outboxMin
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-o.poke:
			t.Stop()
		case <-t.C:
		}
	}
}

// Flush sends the pending rows until the outbox is empty. On the home it clears the outbox: the rows are home already.
func (o *Outbox) Flush(ctx context.Context) (int, error) {
	base, token, self := o.home.HomeEndpoint()
	if self {
		return 0, o.store.clearOutbox(ctx)
	}
	sent := 0
	for {
		rows, err := o.store.pending(ctx, outboxBatch)
		if err != nil || len(rows) == 0 {
			return sent, err
		}
		if err := o.post(ctx, base, token, rows); err != nil {
			return sent, err
		}
		if err := o.store.sent(ctx, rows); err != nil {
			return sent, err
		}
		sent += len(rows)
	}
}

func (o *Outbox) post(ctx context.Context, base, token string, rows []wire.Event) error {
	body := wire.EventAppend{Events: make([]wire.EventDraft, len(rows))}
	for i, r := range rows {
		body.Events[i] = r.EventDraft
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/events", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("events: the home answered %d %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func (s *Store) pending(ctx context.Context, limit int) ([]wire.Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM events WHERE seq IN (SELECT seq FROM outbox) ORDER BY seq LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wire.Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) sent(ctx context.Context, rows []wire.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM outbox WHERE seq <= ?", rows[len(rows)-1].Seq)
	return err
}

func (s *Store) clearOutbox(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, "DELETE FROM outbox")
	return err
}

// Pending counts the rows in the outbox.
func (s *Store) Pending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox").Scan(&n)
	return n, err
}
