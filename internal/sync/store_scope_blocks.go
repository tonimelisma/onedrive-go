// store_scope_blocks.go — Persistence for scope blocks (scope_blocks table).
//
// ScopeGate uses ScopeBlockStore for write-through caching: the in-memory
// map is the hot path for reads (Admit), while all mutations persist to
// SQLite synchronously. On startup, LoadFromStore reads all rows into memory.
//
// The scope_blocks table is tiny (typically 0-5 rows). No batch optimization
// needed — single-row operations are sufficient.
//
// Related files:
//   - scope_gate.go:    ScopeGate type and ScopeBlockStore interface
//   - scope.go:         ScopeKey, ParseScopeKey, ScopeKey.String()
//   - tracker.go:       ScopeBlock struct definition (moves to scope_gate.go in Phase 5)
//   - migrations/:      00003_scope_blocks.sql creates the table
package sync

import (
	"context"
	"fmt"
	"time"
)

// UpsertScopeBlock persists a scope block to the scope_blocks table.
// INSERT OR REPLACE — the scope_key is the primary key, so this handles
// both insert and update. All fields are serialized: ScopeKey.String() for
// the key, UnixNano for timestamps, nanoseconds for Duration.
func (m *SyncStore) UpsertScopeBlock(ctx context.Context, block *ScopeBlock) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO scope_blocks
			(scope_key, issue_type, blocked_at, trial_interval, next_trial_at, trial_count)
		VALUES (?, ?, ?, ?, ?, ?)`,
		block.Key.String(),
		block.IssueType,
		block.BlockedAt.UnixNano(),
		int64(block.TrialInterval),
		block.NextTrialAt.UnixNano(),
		block.TrialCount,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting scope block %s: %w", block.Key.String(), err)
	}

	return nil
}

// DeleteScopeBlock removes a scope block from the scope_blocks table.
// No-op if the row doesn't exist (DELETE WHERE is a natural no-op).
func (m *SyncStore) DeleteScopeBlock(ctx context.Context, key ScopeKey) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`,
		key.String(),
	)
	if err != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", key.String(), err)
	}

	return nil
}

// ListScopeBlocks returns all persisted scope blocks. Used by ScopeGate at
// startup to populate the in-memory cache. Returns an empty slice (not nil)
// if no rows exist.
func (m *SyncStore) ListScopeBlocks(ctx context.Context) ([]*ScopeBlock, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT scope_key, issue_type, blocked_at, trial_interval, next_trial_at, trial_count
		FROM scope_blocks`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing scope blocks: %w", err)
	}
	defer rows.Close()

	var result []*ScopeBlock

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

		block := &ScopeBlock{
			Key:           ParseScopeKey(wireKey),
			IssueType:     issueType,
			BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
			TrialInterval: time.Duration(intervalNano),
			NextTrialAt:   time.Unix(0, nextTrialNano).UTC(),
			TrialCount:    trialCount,
		}
		result = append(result, block)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating scope block rows: %w", err)
	}

	// Return empty slice, not nil, for consistent caller behavior.
	if result == nil {
		result = []*ScopeBlock{}
	}

	return result, nil
}
