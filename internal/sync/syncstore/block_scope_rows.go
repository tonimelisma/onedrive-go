package syncstore

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

type blockScopeRowScanner interface {
	Scan(dest ...any) error
}

const sqlSelectBlockScopeRows = `SELECT scope_key, trial_interval, next_trial_at, slow_start_threshold
	FROM block_scopes`

func scanBlockScopeRow(scanner blockScopeRowScanner) (*syncdomain.BlockScope, error) {
	var (
		wireKey            string
		intervalNano       int64
		nextTrialNano      int64
		slowStartThreshold int
	)

	if err := scanner.Scan(
		&wireKey,
		&intervalNano,
		&nextTrialNano,
		&slowStartThreshold,
	); err != nil {
		return nil, fmt.Errorf("scan block scope row: %w", err)
	}

	key := syncdomain.ParseScopeKey(wireKey)
	if key.IsZero() {
		return &syncdomain.BlockScope{Key: syncdomain.ScopeKey{}}, nil
	}
	if syncdomain.DescribeScopeKey(key).IsZero() {
		return nil, fmt.Errorf("scan block scope row: unknown scope key %q", wireKey)
	}
	if !key.PersistsInBlockScopes() {
		return nil, fmt.Errorf("scan block scope row: read boundary scope %q belongs in observation_issues, not block_scopes", wireKey)
	}

	nextTrialAt := time.Time{}
	if syncdomain.StoredTimeIsSet(nextTrialNano) {
		nextTrialAt = time.Unix(0, nextTrialNano).UTC()
	}

	return &syncdomain.BlockScope{
		Key:                key,
		TrialInterval:      time.Duration(intervalNano),
		NextTrialAt:        nextTrialAt,
		SlowStartThreshold: slowStartThreshold,
	}, nil
}

func queryBlockScopeRowsWithRunner(ctx context.Context, runner sqlTxRunner) ([]*syncdomain.BlockScope, error) {
	rows, err := runner.QueryContext(ctx, sqlSelectBlockScopeRows)
	if err != nil {
		return nil, fmt.Errorf("query block scope rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []*syncdomain.BlockScope
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
		result = []*syncdomain.BlockScope{}
	}

	return result, nil
}
