package sync

import (
	"context"
	"fmt"
	"time"
)

type blockScopeRowScanner interface {
	Scan(dest ...any) error
}

const sqlSelectBlockScopeRows = `SELECT scope_key, scope_family, scope_access, subject_kind, subject_value,
	        condition_type, timing_source, blocked_at, trial_interval, next_trial_at, trial_count
	FROM block_scopes`

func scanBlockScopeRow(scanner blockScopeRowScanner) (*BlockScope, error) {
	var (
		wireKey       string
		scopeFamily   string
		scopeAccess   string
		subjectKind   string
		subjectValue  string
		conditionType string
		timingSource  string
		blockedAtNano int64
		intervalNano  int64
		nextTrialNano int64
		trialCount    int
	)

	if err := scanner.Scan(
		&wireKey,
		&scopeFamily,
		&scopeAccess,
		&subjectKind,
		&subjectValue,
		&conditionType,
		&timingSource,
		&blockedAtNano,
		&intervalNano,
		&nextTrialNano,
		&trialCount,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	metadata, err := decodePersistedScopeMetadata(
		wireKey,
		scopeFamily,
		scopeAccess,
		subjectKind,
		subjectValue,
	)
	if err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}
	if metadata.Key.IsZero() {
		return &BlockScope{Key: ScopeKey{}}, nil
	}

	nextTrialAt := time.Time{}
	if nextTrialNano != 0 {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}

	return newBlockScopeFromPersistedMetadata(
		&metadata,
		conditionType,
		ScopeTimingSource(timingSource),
		time.Unix(0, blockedAtNano).UTC(),
		time.Duration(intervalNano),
		nextTrialAt,
		trialCount,
	), nil
}

func queryBlockScopeRowsWithRunner(ctx context.Context, runner sqlTxRunner) ([]*BlockScope, error) {
	rows, err := runner.QueryContext(ctx, sqlSelectBlockScopeRows)
	if err != nil {
		return nil, fmt.Errorf("query block scope rows: %w", err)
	}
	defer rows.Close()

	var result []*BlockScope
	for rows.Next() {
		block, err := scanBlockScopeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan block scope row: %w", err)
		}
		if block.Key.IsZero() {
			continue
		}
		result = append(result, block)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate block scope rows: %w", err)
	}

	if result == nil {
		result = []*BlockScope{}
	}

	return result, nil
}
