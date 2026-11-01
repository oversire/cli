package events

import (
	"context"
	"log/slog"
	"time"
)

// Keep is the time that the payload and the private text stay on the daemon.
const Keep = 90 * 24 * time.Hour

// Prune clears the payload, and the summary of a private row, on rows older than Keep. The row stays.
func (s *Store) Prune(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE events SET payload = NULL, summary = CASE WHEN private = 1 THEN '' ELSE summary END
		WHERE at < ? AND (payload IS NOT NULL OR (private = 1 AND summary != ''))`, now.Add(-Keep).UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RunRetention prunes at the start and then each day. It returns when ctx is done.
func (s *Store) RunRetention(ctx context.Context, log *slog.Logger) error {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if _, err := s.Prune(ctx, s.clock()); err != nil {
			log.Warn("events: prune", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
