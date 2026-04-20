// Package sync persists sync baseline, observation, failure, scope-block, and
// run-status state.
//
// The engine persists active scope rows here for restart/recovery. Watch mode
// loads them into its single-owner runtime working set at startup; there is no
// separate write-through cache subsystem outside the sync runtime.
//
// The block_scopes table is tiny (typically 0-5 rows). No batch optimization
// needed — single-row operations are sufficient.
//
// Related files:
//   - active_scopes.go: stateless active-scope helper functions
//   - scope_key.go:     ScopeKey, ParseScopeKey, ScopeKey.String()
package sync

import (
	"context"
	"fmt"
)

func validateBlockScope(block *BlockScope) error {
	if block.Key.IsZero() {
		return fmt.Errorf("sync: upserting block scope: missing scope key")
	}
	if DescribeScopeKey(block.Key).IsZero() {
		return fmt.Errorf("sync: upserting block scope %s: unknown scope key", block.Key.String())
	}
	if !isTimedBlockScopeKey(block.Key) {
		return fmt.Errorf("sync: upserting block scope %s: read boundaries belong in observation_issues, not block_scopes", block.Key.String())
	}

	if block.BlockedAt.IsZero() {
		return fmt.Errorf("sync: upserting block scope %s: missing blocked_at", block.Key.String())
	}
	if block.TrialInterval <= 0 {
		return fmt.Errorf("sync: upserting block scope %s: timed scope requires positive trial interval", block.Key.String())
	}
	if block.NextTrialAt.IsZero() {
		return fmt.Errorf("sync: upserting block scope %s: timed scope requires next_trial_at", block.Key.String())
	}

	return nil
}

// UpsertBlockScope persists a block scope to the block_scopes table.
// INSERT OR REPLACE — the scope_key is the primary key, so this handles
// both insert and update. All fields are serialized: ScopeKey.String() for
// the key, UnixNano for timestamps, nanoseconds for Duration.
func (m *SyncStore) UpsertBlockScope(ctx context.Context, block *BlockScope) error {
	return upsertBlockScopeWithRunner(ctx, m.db, block)
}

func upsertBlockScopeWithRunner(ctx context.Context, runner sqlTxRunner, block *BlockScope) error {
	if err := validateBlockScope(block); err != nil {
		return err
	}

	nextTrialAtNano := int64(0)
	if !block.NextTrialAt.IsZero() {
		nextTrialAtNano = block.NextTrialAt.UnixNano()
	}

	_, err := runner.ExecContext(ctx,
		`INSERT OR REPLACE INTO block_scopes
			(scope_key, blocked_at, trial_interval, next_trial_at)
		VALUES (?, ?, ?, ?)`,
		block.Key.String(),
		block.BlockedAt.UnixNano(),
		int64(block.TrialInterval),
		nextTrialAtNano,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting block scope %s: %w", block.Key.String(), err)
	}

	return nil
}

// DeleteBlockScope removes a block scope from the block_scopes table.
// No-op if the row doesn't exist (DELETE WHERE is a natural no-op).
func (m *SyncStore) DeleteBlockScope(ctx context.Context, key ScopeKey) error {
	return deleteBlockScopeWithRunner(ctx, m.db, key)
}

func deleteBlockScopeWithRunner(ctx context.Context, runner sqlTxRunner, key ScopeKey) error {
	_, err := runner.ExecContext(ctx,
		`DELETE FROM block_scopes WHERE scope_key = ?`,
		key.String(),
	)
	if err != nil {
		return fmt.Errorf("sync: deleting block scope %s: %w", key.String(), err)
	}

	return nil
}

// ListBlockScopes returns all persisted block scopes. Used at startup to
// populate the engine-owned active scope working set. Returns an empty slice
// (not nil) if no rows exist.
func (m *SyncStore) ListBlockScopes(ctx context.Context) ([]*BlockScope, error) {
	result, err := queryBlockScopeRowsWithRunner(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("sync: listing block scopes: %w", err)
	}

	return result, nil
}
