package sync

import (
	"context"
	"fmt"
)

func queryObservationIssueRowsDB(ctx context.Context, runner sqlTxRunner) ([]ObservationIssueRow, error) {
	configuredDriveID, err := configuredDriveIDForDB(ctx, runner)
	if err != nil {
		return nil, err
	}

	rows, err := runner.QueryContext(ctx,
		`SELECT `+sqlSelectObservationIssueCols+` FROM observation_issues
		ORDER BY last_seen_at DESC, path`)
	if err != nil {
		return nil, fmt.Errorf("query observation issues: %w", err)
	}
	defer rows.Close()

	return scanObservationIssueRows(rows, configuredDriveID)
}

func queryBlockScopesDB(ctx context.Context, runner sqlTxRunner) ([]*BlockScope, error) {
	rows, err := runner.QueryContext(ctx,
		`SELECT scope_key, scope_family, scope_access, subject_kind, subject_value,
		        condition_type, timing_source, blocked_at, trial_interval, next_trial_at, trial_count
			FROM block_scopes`)
	if err != nil {
		return nil, fmt.Errorf("query block scopes: %w", err)
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

func queryBlockedRetryWorkRowsDB(ctx context.Context, runner sqlTxRunner) ([]RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("query blocked retry_work rows: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}
