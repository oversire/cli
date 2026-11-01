package events

import (
	"context"

	"github.com/oversire/cli/wire"
)

// SetLinks keeps the parent of each item and sets parent_item again on the old rows of the item.
// An empty parent clears it. It gives the number of rows that changed.
func (s *Store) SetLinks(ctx context.Context, links []wire.ItemLink) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	changed := 0
	for _, l := range links {
		item, parent := upperKey(l.Item), upperKey(l.Parent)
		if item == "" || item == parent {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO item_links (item, parent, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(item) DO UPDATE SET parent = excluded.parent, updated_at = excluded.updated_at`, item, null(parent), now); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, "UPDATE events SET parent_item = ? WHERE item = ? AND parent_item IS NOT ?", null(parent), item, null(parent))
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		changed += int(n)
	}
	return changed, tx.Commit()
}

// Parent gives the parent of item from item_links.
func (s *Store) Parent(ctx context.Context, item string) string {
	var p *string
	if s.db.QueryRowContext(ctx, "SELECT parent FROM item_links WHERE item = ?", upperKey(item)).Scan(&p) != nil || p == nil {
		return ""
	}
	return *p
}
