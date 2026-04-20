package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const sqlSelectObservationIssueCols = `path, action_type, issue_type, item_id, last_error, first_seen_at, ` +
	`last_seen_at, file_size, local_hash, scope_key`

func (m *SyncStore) UpsertObservationIssue(ctx context.Context, issue *ObservationIssue) error {
	if issue == nil {
		return fmt.Errorf("sync: upsert observation issue: nil issue")
	}

	return m.UpsertObservationIssues(ctx, []ObservationIssue{*issue})
}

func (m *SyncStore) UpsertObservationIssues(
	ctx context.Context,
	issues []ObservationIssue,
) (err error) {
	if len(issues) == 0 {
		return nil
	}

	nowNano := m.nowFunc().UnixNano()
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin observation issue upsert: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation issue upsert")
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	for i := range issues {
		if issues[i].DriveID.IsZero() {
			continue
		}
		if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, issues[i].DriveID, state); ensureErr != nil {
			return ensureErr
		}
		break
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observation_issues
			(path, action_type, issue_type, item_id, last_error, first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			action_type = excluded.action_type,
			issue_type = excluded.issue_type,
			item_id = excluded.item_id,
			last_error = excluded.last_error,
			last_seen_at = excluded.last_seen_at,
			file_size = excluded.file_size,
			local_hash = excluded.local_hash,
			scope_key = excluded.scope_key`)
	if err != nil {
		return fmt.Errorf("sync: prepare observation issue upsert: %w", err)
	}
	defer stmt.Close()

	for i := range issues {
		issue := &issues[i]
		if issue.Path == "" {
			return fmt.Errorf("sync: upsert observation issue: missing path")
		}
		if _, valueErr := issue.ActionType.Value(); valueErr != nil {
			return fmt.Errorf("sync: upsert observation issue for %s: invalid action type: %w", issue.Path, valueErr)
		}

		if _, execErr := stmt.ExecContext(ctx,
			issue.Path,
			issue.ActionType.String(),
			issue.IssueType,
			issue.ItemID,
			issue.Error,
			nowNano,
			nowNano,
			issue.FileSize,
			issue.LocalHash,
			issue.ScopeKey.String(),
		); execErr != nil {
			return fmt.Errorf("sync: upsert observation issue for %s: %w", issue.Path, execErr)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit observation issue upsert: %w", err)
	}

	return nil
}

func (m *SyncStore) ListObservationIssues(ctx context.Context) ([]ObservationIssueRow, error) {
	configuredDriveID, err := m.configuredDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("sync: reading configured drive for observation issues: %w", err)
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectObservationIssueCols+` FROM observation_issues ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	defer rows.Close()

	return scanObservationIssueRows(rows, configuredDriveID)
}

func (m *SyncStore) ClearObservationIssuesByPaths(
	ctx context.Context,
	issueType string,
	paths []string,
) error {
	if issueType == "" || len(paths) == 0 {
		return nil
	}

	encodedPaths, err := json.Marshal(paths)
	if err != nil {
		return fmt.Errorf("sync: marshal observation issue paths for %s: %w", issueType, err)
	}

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM observation_issues
			WHERE issue_type = ?
				AND path IN (SELECT value FROM json_each(?))`,
		issueType,
		string(encodedPaths),
	); err != nil {
		return fmt.Errorf("sync: clearing observation issues by paths for %s: %w", issueType, err)
	}

	return nil
}

func (m *SyncStore) ClearResolvedObservationIssues(
	ctx context.Context,
	issueType string,
	currentPaths []string,
) error {
	if issueType == "" {
		return nil
	}

	if len(currentPaths) == 0 {
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM observation_issues WHERE issue_type = ?`,
			issueType,
		); err != nil {
			return fmt.Errorf("sync: clearing resolved observation issues for %s: %w", issueType, err)
		}

		return nil
	}

	encodedPaths, err := json.Marshal(currentPaths)
	if err != nil {
		return fmt.Errorf("sync: marshal resolved observation issue paths for %s: %w", issueType, err)
	}

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM observation_issues
			WHERE issue_type = ?
				AND path NOT IN (SELECT value FROM json_each(?))`,
		issueType,
		string(encodedPaths),
	); err != nil {
		return fmt.Errorf("sync: clearing resolved observation issues for %s: %w", issueType, err)
	}

	return nil
}

type observationIssueScanner interface {
	Scan(dest ...any) error
}

func scanObservationIssueRow(
	scanner observationIssueScanner,
	row *ObservationIssueRow,
	configuredDriveID driveid.ID,
) error {
	if row == nil {
		return fmt.Errorf("sync: scanning observation issue row: nil destination")
	}

	var scopeKey string
	if err := scanner.Scan(
		&row.Path,
		&row.ActionType,
		&row.IssueType,
		&row.ItemID,
		&row.LastError,
		&row.FirstSeenAt,
		&row.LastSeenAt,
		&row.FileSize,
		&row.LocalHash,
		&scopeKey,
	); err != nil {
		return fmt.Errorf("sync: scanning observation issue row: %w", err)
	}

	row.DriveID = configuredDriveID
	row.ScopeKey = ParseScopeKey(scopeKey)
	return nil
}

func scanObservationIssueRows(rows *sql.Rows, configuredDriveID driveid.ID) ([]ObservationIssueRow, error) {
	var result []ObservationIssueRow

	for rows.Next() {
		var row ObservationIssueRow
		if err := scanObservationIssueRow(rows, &row, configuredDriveID); err != nil {
			return nil, fmt.Errorf("sync: scanning observation issue row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating observation issue rows: %w", err)
	}

	return result, nil
}
