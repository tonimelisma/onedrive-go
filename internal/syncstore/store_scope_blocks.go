// store_scope_blocks.go — Persistence for scope blocks (scope_blocks table).
//
// The engine persists active scope rows here for restart/recovery. Watch mode
// loads them into its single-owner runtime working set at startup; there is no
// separate write-through cache subsystem in syncdispatch.
//
// The scope_blocks table is tiny (typically 0-5 rows). No batch optimization
// needed — single-row operations are sufficient.
//
// Related files:
//   - active_scopes.go: stateless active-scope helper functions
//   - scope.go:         ScopeKey, ParseScopeKey, ScopeKey.String()
//   - migrations/:      00003_scope_blocks.sql creates the table
package syncstore

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// UpsertScopeBlock persists a scope block to the scope_blocks table.
// INSERT OR REPLACE — the scope_key is the primary key, so this handles
// both insert and update. All fields are serialized: ScopeKey.String() for
// the key, UnixNano for timestamps, nanoseconds for Duration.
func (m *SyncStore) UpsertScopeBlock(ctx context.Context, block *synctypes.ScopeBlock) error {
	nextTrialAtNano := int64(0)
	if !block.NextTrialAt.IsZero() {
		nextTrialAtNano = block.NextTrialAt.UnixNano()
	}

	_, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO scope_blocks
			(scope_key, issue_type, blocked_at, trial_interval, next_trial_at, trial_count)
		VALUES (?, ?, ?, ?, ?, ?)`,
		block.Key.String(),
		block.IssueType,
		block.BlockedAt.UnixNano(),
		int64(block.TrialInterval),
		nextTrialAtNano,
		block.TrialCount,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting scope block %s: %w", block.Key.String(), err)
	}

	return nil
}

// DeleteScopeBlock removes a scope block from the scope_blocks table.
// No-op if the row doesn't exist (DELETE WHERE is a natural no-op).
func (m *SyncStore) DeleteScopeBlock(ctx context.Context, key synctypes.ScopeKey) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`,
		key.String(),
	)
	if err != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", key.String(), err)
	}

	return nil
}

// ListScopeBlocks returns all persisted scope blocks. Used at startup to
// populate the engine-owned active scope working set. Returns an empty slice
// (not nil) if no rows exist.
func (m *SyncStore) ListScopeBlocks(ctx context.Context) ([]*synctypes.ScopeBlock, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT scope_key, issue_type, blocked_at, trial_interval, next_trial_at, trial_count
		FROM scope_blocks`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing scope blocks: %w", err)
	}
	defer rows.Close()

	var result []*synctypes.ScopeBlock

	for rows.Next() {
		var (
			wireKey       string
			issueType     string
			blockedAtNano int64
			intervalNano  int64
			nextTrialNano int64
			trialCount    int
		)

		if scanErr := rows.Scan(
			&wireKey, &issueType, &blockedAtNano,
			&intervalNano, &nextTrialNano, &trialCount,
		); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning scope block row: %w", scanErr)
		}

		nextTrialAt := time.Time{}
		if nextTrialNano != 0 {
			nextTrialAt = time.Unix(0, nextTrialNano).UTC()
		}

		block := &synctypes.ScopeBlock{
			Key:           synctypes.ParseScopeKey(wireKey),
			IssueType:     issueType,
			BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
			TrialInterval: time.Duration(intervalNano),
			NextTrialAt:   nextTrialAt,
			TrialCount:    trialCount,
		}
		result = append(result, block)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating scope block rows: %w", err)
	}

	// Return empty slice, not nil, for consistent caller behavior.
	if result == nil {
		result = []*synctypes.ScopeBlock{}
	}

	return result, nil
}
