// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
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
//   - scope_key.go:     ScopeKey, ParseScopeKey, ScopeKey.String()
//   - schema.sql:       canonical schema definition
package syncstore

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func validateScopeBlock(block *synctypes.ScopeBlock) error {
	if block.Key.IsZero() {
		return fmt.Errorf("sync: upserting scope block: missing scope key")
	}

	if block.BlockedAt.IsZero() {
		return fmt.Errorf("sync: upserting scope block %s: missing blocked_at", block.Key.String())
	}

	switch block.TimingSource {
	case synctypes.ScopeTimingNone:
		if block.TrialInterval != 0 {
			return fmt.Errorf("sync: upserting scope block %s: timing_source none requires zero trial interval", block.Key.String())
		}
		if !block.NextTrialAt.IsZero() {
			return fmt.Errorf("sync: upserting scope block %s: timing_source none requires zero next_trial_at", block.Key.String())
		}
		if !block.PreserveUntil.IsZero() {
			return fmt.Errorf("sync: upserting scope block %s: timing_source none requires zero preserve_until", block.Key.String())
		}
	case synctypes.ScopeTimingBackoff, synctypes.ScopeTimingServerRetryAfter:
		if block.TrialInterval <= 0 {
			return fmt.Errorf("sync: upserting scope block %s: timed scope requires positive trial interval", block.Key.String())
		}
		if block.NextTrialAt.IsZero() {
			return fmt.Errorf("sync: upserting scope block %s: timed scope requires next_trial_at", block.Key.String())
		}
		if !block.PreserveUntil.IsZero() && block.PreserveUntil.Before(block.NextTrialAt) {
			return fmt.Errorf("sync: upserting scope block %s: preserve_until must not be before next_trial_at", block.Key.String())
		}
	default:
		return fmt.Errorf("sync: upserting scope block %s: invalid timing source %q", block.Key.String(), block.TimingSource)
	}

	return nil
}

// UpsertScopeBlock persists a scope block to the scope_blocks table.
// INSERT OR REPLACE — the scope_key is the primary key, so this handles
// both insert and update. All fields are serialized: ScopeKey.String() for
// the key, UnixNano for timestamps, nanoseconds for Duration.
func (m *SyncStore) UpsertScopeBlock(ctx context.Context, block *synctypes.ScopeBlock) error {
	if err := validateScopeBlock(block); err != nil {
		return err
	}

	nextTrialAtNano := int64(0)
	if !block.NextTrialAt.IsZero() {
		nextTrialAtNano = block.NextTrialAt.UnixNano()
	}
	preserveUntilNano := int64(0)
	if !block.PreserveUntil.IsZero() {
		preserveUntilNano = block.PreserveUntil.UnixNano()
	}

	_, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		block.Key.String(),
		block.IssueType,
		block.TimingSource,
		block.BlockedAt.UnixNano(),
		int64(block.TrialInterval),
		nextTrialAtNano,
		preserveUntilNano,
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
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count
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
			timingSource  string
			blockedAtNano int64
			intervalNano  int64
			nextTrialNano int64
			preserveNano  int64
			trialCount    int
		)

		if scanErr := rows.Scan(
			&wireKey, &issueType, &timingSource, &blockedAtNano,
			&intervalNano, &nextTrialNano, &preserveNano, &trialCount,
		); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning scope block row: %w", scanErr)
		}

		nextTrialAt := time.Time{}
		if nextTrialNano != 0 {
			nextTrialAt = time.Unix(0, nextTrialNano).UTC()
		}
		preserveUntil := time.Time{}
		if preserveNano != 0 {
			preserveUntil = time.Unix(0, preserveNano).UTC()
		}

		block := &synctypes.ScopeBlock{
			Key:           synctypes.ParseScopeKey(wireKey),
			IssueType:     issueType,
			TimingSource:  synctypes.ScopeTimingSource(timingSource),
			BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
			TrialInterval: time.Duration(intervalNano),
			NextTrialAt:   nextTrialAt,
			PreserveUntil: preserveUntil,
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
