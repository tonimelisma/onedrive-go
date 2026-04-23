package sync

import (
	"context"
	"fmt"
	"time"
)

type blockScopeRowScanner interface {
	Scan(dest ...any) error
}

const sqlSelectBlockScopeRows = `SELECT scope_key, trial_interval, next_trial_at
	FROM block_scopes`

func scanBlockScopeRow(scanner blockScopeRowScanner) (*BlockScope, error) {
	var (
		wireKey       string
		intervalNano  int64
		nextTrialNano int64
	)

	if err := scanner.Scan(
		&wireKey,
		&intervalNano,
		&nextTrialNano,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	key := ParseScopeKey(wireKey)
	if key.IsZero() {
		return &BlockScope{Key: ScopeKey{}}, nil
	}
	if DescribeScopeKey(key).IsZero() {
		return nil, fmt.Errorf("scan block scope row: unknown scope key %q", wireKey)
	}
	if !key.PersistsInBlockScopes() {
		return nil, fmt.Errorf("scan block scope row: read boundary scope %q belongs in observation_issues, not block_scopes", wireKey)
	}

	nextTrialAt := time.Time{}
	if nextTrialNano != 0 {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}

	return &BlockScope{
		Key:           key,
		TrialInterval: time.Duration(intervalNano),
		NextTrialAt:   nextTrialAt,
	}, nil
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
