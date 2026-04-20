package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const sqlSelectObservationIssueCols = `path, action_type, issue_type, item_id, last_error, first_seen_at, ` +
	`last_seen_at, file_size, local_hash, scope_key`

func (m *SyncStore) ReconcileObservationFindings(
	ctx context.Context,
	batch *ObservationFindingsBatch,
	now time.Time,
) (err error) {
	if batch == nil {
		return nil
	}

	nowNano := now.UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin observation findings reconcile: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation findings reconcile")
	}()

	state, err := loadObservationReconcileStateTx(ctx, tx)
	if err != nil {
		return err
	}
	plan := buildObservationReconcilePlan(batch, state)

	if applyErr := m.applyObservationFindingsReconcilePlanTx(ctx, tx, plan, nowNano); applyErr != nil {
		return applyErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit observation findings reconcile: %w", err)
	}

	return nil
}

func (m *SyncStore) applyObservationFindingsReconcilePlanTx(
	ctx context.Context,
	tx sqlTxRunner,
	plan observationReconcilePlan,
	nowNano int64,
) error {
	if err := m.upsertObservationIssuesTx(ctx, tx, plan.issueUpserts, nowNano); err != nil {
		return err
	}
	if err := deleteObservationIssuesTx(ctx, tx, plan.issueDeletes); err != nil {
		return err
	}

	return nil
}

func loadObservationReconcileStateTx(
	ctx context.Context,
	tx sqlTxRunner,
) (observationReconcileState, error) {
	issues, err := queryObservationIssueRowsDB(ctx, tx)
	if err != nil {
		return observationReconcileState{}, fmt.Errorf("sync: listing observation issues for observation reconcile: %w", err)
	}

	return observationReconcileState{
		issues: issues,
	}, nil
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

type observationIssueScanner interface {
	Scan(dest ...any) error
}

func (m *SyncStore) upsertObservationIssuesTx(
	ctx context.Context,
	tx sqlTxRunner,
	issues []ObservationIssue,
	nowNano int64,
) error {
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

	if len(issues) == 0 {
		return nil
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

	return nil
}

func deleteObservationIssuesTx(
	ctx context.Context,
	tx sqlTxRunner,
	deletes []observationIssueDelete,
) error {
	if len(deletes) == 0 {
		return nil
	}

	stmt, err := tx.PrepareContext(ctx,
		`DELETE FROM observation_issues WHERE path = ? AND issue_type = ?`)
	if err != nil {
		return fmt.Errorf("sync: prepare observation issue delete: %w", err)
	}
	defer stmt.Close()

	for i := range deletes {
		deletePlan := deletes[i]
		if _, execErr := stmt.ExecContext(ctx, deletePlan.path, deletePlan.issueType); execErr != nil {
			return fmt.Errorf("sync: deleting observation issue %s (%s): %w", deletePlan.path, deletePlan.issueType, execErr)
		}
	}

	return nil
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
