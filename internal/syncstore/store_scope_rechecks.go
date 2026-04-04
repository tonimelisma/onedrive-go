package syncstore

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (m *SyncStore) RequestScopeRecheck(ctx context.Context, scopeKey synctypes.ScopeKey) error {
	if !scopeKey.IsPermRemote() {
		return fmt.Errorf("sync: requesting scope recheck for %s: only perm:remote scopes support explicit recheck", scopeKey.String())
	}

	_, err := m.db.ExecContext(ctx,
		`INSERT INTO scope_recheck_requests (scope_key, requested_at)
		VALUES (?, ?)
		ON CONFLICT(scope_key) DO UPDATE SET requested_at = excluded.requested_at`,
		scopeKey.String(), m.nowFunc().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("sync: requesting scope recheck for %s: %w", scopeKey.String(), err)
	}

	return nil
}

func (m *SyncStore) ClearScopeRecheckRequest(ctx context.Context, scopeKey synctypes.ScopeKey) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM scope_recheck_requests WHERE scope_key = ?`,
		scopeKey.String(),
	)
	if err != nil {
		return fmt.Errorf("sync: clearing scope recheck request for %s: %w", scopeKey.String(), err)
	}

	return nil
}

func (m *SyncStore) ListRequestedScopeRechecks(ctx context.Context) ([]synctypes.ScopeKey, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT scope_key FROM scope_recheck_requests ORDER BY requested_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing scope recheck requests: %w", err)
	}
	defer rows.Close()

	var keys []synctypes.ScopeKey
	for rows.Next() {
		var wire string
		if err := rows.Scan(&wire); err != nil {
			return nil, fmt.Errorf("sync: scanning scope recheck request: %w", err)
		}
		keys = append(keys, synctypes.ParseScopeKey(wire))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating scope recheck requests: %w", err)
	}

	return keys, nil
}
