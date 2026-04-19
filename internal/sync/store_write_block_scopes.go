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
	"time"
)

func validateBlockScope(block *BlockScope) error {
	if block.Key.IsZero() {
		return fmt.Errorf("sync: upserting block scope: missing scope key")
	}

	if block.BlockedAt.IsZero() {
		return fmt.Errorf("sync: upserting block scope %s: missing blocked_at", block.Key.String())
	}

	switch block.TimingSource {
	case ScopeTimingNone:
		if block.TrialInterval != 0 {
			return fmt.Errorf("sync: upserting block scope %s: timing_source none requires zero trial interval", block.Key.String())
		}
		if !block.NextTrialAt.IsZero() {
			return fmt.Errorf("sync: upserting block scope %s: timing_source none requires zero next_trial_at", block.Key.String())
		}
	case ScopeTimingBackoff, ScopeTimingServerRetryAfter:
		if block.TrialInterval <= 0 {
			return fmt.Errorf("sync: upserting block scope %s: timed scope requires positive trial interval", block.Key.String())
		}
		if block.NextTrialAt.IsZero() {
			return fmt.Errorf("sync: upserting block scope %s: timed scope requires next_trial_at", block.Key.String())
		}
	default:
		return fmt.Errorf("sync: upserting block scope %s: invalid timing source %q", block.Key.String(), block.TimingSource)
	}

	return nil
}

// UpsertBlockScope persists a block scope to the block_scopes table.
// INSERT OR REPLACE — the scope_key is the primary key, so this handles
// both insert and update. All fields are serialized: ScopeKey.String() for
// the key, UnixNano for timestamps, nanoseconds for Duration.
func (m *SyncStore) UpsertBlockScope(ctx context.Context, block *BlockScope) error {
	if err := validateBlockScope(block); err != nil {
		return err
	}

	nextTrialAtNano := int64(0)
	if !block.NextTrialAt.IsZero() {
		nextTrialAtNano = block.NextTrialAt.UnixNano()
	}

	_, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO block_scopes
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, trial_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		block.Key.String(),
		block.IssueType,
		block.TimingSource,
		block.BlockedAt.UnixNano(),
		int64(block.TrialInterval),
		nextTrialAtNano,
		block.TrialCount,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting block scope %s: %w", block.Key.String(), err)
	}

	return nil
}

// DeleteBlockScope removes a block scope from the block_scopes table.
// No-op if the row doesn't exist (DELETE WHERE is a natural no-op).
func (m *SyncStore) DeleteBlockScope(ctx context.Context, key ScopeKey) error {
	_, err := m.db.ExecContext(ctx,
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
	rows, err := m.db.QueryContext(ctx,
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, trial_count
		FROM block_scopes`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing block scopes: %w", err)
	}
	defer rows.Close()

	var result []*BlockScope

	for rows.Next() {
		var (
			wireKey       string
			issueType     string
			timingSource  string
			blockedAtNano int64
			intervalNano  int64
			nextTrialNano int64
			trialCount    int
		)

		if scanErr := rows.Scan(
			&wireKey, &issueType, &timingSource, &blockedAtNano,
			&intervalNano, &nextTrialNano, &trialCount,
		); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning block scope row: %w", scanErr)
		}

		nextTrialAt := time.Time{}
		if nextTrialNano != 0 {
			nextTrialAt = time.Unix(0, nextTrialNano).UTC()
		}

		block := &BlockScope{
			Key:           ParseScopeKey(wireKey),
			IssueType:     issueType,
			TimingSource:  ScopeTimingSource(timingSource),
			BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
			TrialInterval: time.Duration(intervalNano),
			NextTrialAt:   nextTrialAt,
			TrialCount:    trialCount,
		}
		if block.Key.IsZero() {
			// Old or unknown persisted scope keys are no longer part of the
			// steady-state runtime model. Skip them here so startup diagnosis never
			// treats an unrecognized wire key as an empty scope.
			continue
		}
		result = append(result, block)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating block scope rows: %w", err)
	}

	// Return empty slice, not nil, for consistent caller behavior.
	if result == nil {
		result = []*BlockScope{}
	}

	return result, nil
}
