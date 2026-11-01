package events

import (
	"context"
	"encoding/json"

	"github.com/oversire/cli/eventlog"
	"github.com/oversire/cli/wire"
)

// keepHints is the number of ProviderEvent hints that the table keeps.
const keepHints = eventlog.DefaultCapacity

// SaveHint keeps a ProviderEvent hint of GET /v1/events. It implements eventlog.Keeper.
func (s *Store) SaveHint(ctx context.Context, seq int64, e wire.ProviderEvent) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO hints (seq, body) VALUES (?, ?)`, seq, string(body)); err != nil {
		return err
	}
	if seq%100 == 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM hints WHERE seq <= ?`, seq-keepHints)
	}
	return err
}

// Hints gives the last limit hints in seq order.
func (s *Store) Hints(ctx context.Context, limit int) ([]eventlog.Hint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, body FROM (SELECT seq, body FROM hints ORDER BY seq DESC LIMIT ?) ORDER BY seq`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []eventlog.Hint
	for rows.Next() {
		var h eventlog.Hint
		var body string
		if err := rows.Scan(&h.Seq, &body); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(body), &h.Event) == nil {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}
