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

func queryBlockedRetryWorkRowsDB(ctx context.Context, runner sqlTxRunner) ([]RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("query blocked retry_work rows: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}
