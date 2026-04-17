package sync

import (
	"context"
	"fmt"
)

const (
	sqlDeletePlannedActions = `DELETE FROM planned_actions`
	sqlInsertPlannedAction  = `INSERT INTO planned_actions
		(action_id, plan_id, path, action_type, old_path,
		 source_identity, target_identity, dependency_key,
		 precondition_local_identity, precondition_remote_identity, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	sqlListPlannedActions = `SELECT
		action_id, plan_id, path, action_type, old_path,
		source_identity, target_identity, dependency_key,
		precondition_local_identity, precondition_remote_identity, status
		FROM planned_actions
		ORDER BY action_id`
)

func (m *SyncStore) ReplacePlannedActions(
	ctx context.Context,
	planID string,
	actions []PlannedActionRow,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning planned actions transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback planned actions transaction")
	}()

	if _, err := tx.ExecContext(ctx, sqlDeletePlannedActions); err != nil {
		return fmt.Errorf("sync: deleting planned actions: %w", err)
	}

	for i := range actions {
		row := actions[i]
		row.PlanID = planID
		if row.Status == "" {
			row.Status = "pending"
		}
		if _, err := tx.ExecContext(ctx, sqlInsertPlannedAction,
			i+1,
			row.PlanID,
			row.Path,
			row.ActionType.String(),
			row.OldPath,
			row.SourceIdentity,
			row.TargetIdentity,
			row.DependencyKey,
			row.PreconditionLocalIdentity,
			row.PreconditionRemoteIdentity,
			row.Status,
		); err != nil {
			return fmt.Errorf("sync: inserting planned action for %s: %w", row.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing planned actions transaction: %w", err)
	}

	return nil
}

func (m *SyncStore) ListPlannedActions(ctx context.Context) ([]PlannedActionRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListPlannedActions)
	if err != nil {
		return nil, fmt.Errorf("sync: querying planned actions: %w", err)
	}
	defer rows.Close()

	var actions []PlannedActionRow
	for rows.Next() {
		var row PlannedActionRow
		var actionID int64
		if err := rows.Scan(
			&actionID,
			&row.PlanID,
			&row.Path,
			&row.ActionType,
			&row.OldPath,
			&row.SourceIdentity,
			&row.TargetIdentity,
			&row.DependencyKey,
			&row.PreconditionLocalIdentity,
			&row.PreconditionRemoteIdentity,
			&row.Status,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning planned action row: %w", err)
		}
		row.ActionID = fmt.Sprintf("%d", actionID)
		actions = append(actions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating planned action rows: %w", err)
	}

	return actions, nil
}

func (m *SyncStore) MaterializePlannedActions(ctx context.Context, planID string) error {
	reconciliationRows, err := m.QueryReconciliationState(ctx)
	if err != nil {
		return err
	}

	actions := make([]PlannedActionRow, 0, len(reconciliationRows))
	for _, row := range reconciliationRows {
		action, ok := plannedActionForReconciliation(row)
		if !ok {
			continue
		}
		action.PlanID = planID
		actions = append(actions, action)
	}

	return m.ReplacePlannedActions(ctx, planID, actions)
}

func plannedActionForReconciliation(row SQLiteReconciliationRow) (PlannedActionRow, bool) {
	action := PlannedActionRow{
		Path:   row.Path,
		Status: "pending",
	}

	switch row.ReconciliationKind {
	case "upload":
		action.ActionType = ActionUpload
	case "download":
		action.ActionType = ActionDownload
	case strLocalDelete:
		action.ActionType = ActionLocalDelete
	case strRemoteDelete:
		action.ActionType = ActionRemoteDelete
	case strLocalMove:
		action.ActionType = ActionLocalMove
	case strRemoteMove:
		action.ActionType = ActionRemoteMove
	case strUpdateSynced:
		action.ActionType = ActionUpdateSynced
	case "conflict_edit_edit":
		action.ActionType = ActionConflict
		action.SourceIdentity = ConflictEditEdit
	case "conflict_create_create":
		action.ActionType = ActionConflict
		action.SourceIdentity = ConflictCreateCreate
	case "conflict_edit_delete":
		action.ActionType = ActionConflict
		action.SourceIdentity = ConflictEditDelete
	case "noop", "baseline_remove":
		return PlannedActionRow{}, false
	default:
		return PlannedActionRow{}, false
	}

	return action, true
}
