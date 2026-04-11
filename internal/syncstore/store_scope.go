package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type remoteScopeRow struct {
	DriveID          string
	ItemID           string
	Path             string
	Hash             sql.NullString
	Status           synctypes.SyncStatus
	FilterGeneration int64
	FilterReason     string
	BaselinePath     sql.NullString
	BaselineHash     sql.NullString
}

// UpsertSyncMetadataEntries writes arbitrary sync_metadata keys in one
// transaction. Scope persistence no longer uses this helper; it remains for
// generic sync report/status metadata.
func (m *SyncStore) UpsertSyncMetadataEntries(ctx context.Context, entries map[string]string) (err error) {
	if len(entries) == 0 {
		return nil
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync metadata begin tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync metadata rollback")
	}()

	const upsertSQL = `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`

	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if _, execErr := tx.ExecContext(ctx, upsertSQL, key, entries[key]); execErr != nil {
			return fmt.Errorf("sync metadata upsert %s: %w", key, execErr)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit sync metadata upsert: %w", err)
	}

	return nil
}

// ReadScopeState returns the durable sync-scope projection. Missing scope
// state is not an error; callers use found=false to bootstrap the first
// generation from current truth.
func (m *SyncStore) ReadScopeState(ctx context.Context) (synctypes.ScopeStateRecord, bool, error) {
	row := m.db.QueryRowContext(ctx, `
		SELECT generation, effective_snapshot_json, observation_plan_hash,
		       observation_mode, websocket_enabled, pending_reentry,
		       last_reconcile_kind, updated_at
		FROM scope_state
		WHERE singleton = 1`,
	)

	var (
		record           synctypes.ScopeStateRecord
		websocketEnabled int
		pendingReentry   int
	)
	if err := row.Scan(
		&record.Generation,
		&record.EffectiveSnapshotJSON,
		&record.ObservationPlanHash,
		&record.ObservationMode,
		&websocketEnabled,
		&pendingReentry,
		&record.LastReconcileKind,
		&record.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows || isMissingTableErr(err) {
			return synctypes.ScopeStateRecord{}, false, nil
		}

		return synctypes.ScopeStateRecord{}, false, fmt.Errorf("read scope state: %w", err)
	}

	record.WebsocketEnabled = websocketEnabled != 0
	record.PendingReentry = pendingReentry != 0

	return record, true, nil
}

// ApplyScopeState persists one new durable sync-scope record and re-evaluates
// already-known remote_state rows against the effective snapshot in the same
// transaction. This is the sole durable authority for filtered-row activation
// and deactivation.
func (m *SyncStore) ApplyScopeState(ctx context.Context, req synctypes.ScopeStateApplyRequest) (err error) {
	snapshot, err := syncscope.UnmarshalSnapshot(req.State.EffectiveSnapshotJSON)
	if err != nil {
		return fmt.Errorf("decode scope snapshot: %w", err)
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("scope state begin tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "scope state rollback")
	}()

	if upsertErr := upsertScopeStateRow(ctx, tx, req.State); upsertErr != nil {
		return upsertErr
	}

	allRows, err := loadRemoteScopeRows(ctx, tx)
	if err != nil {
		return err
	}
	if _, applyErr := applyRemoteScopeRows(ctx, tx, snapshot, req.State.Generation, allRows); applyErr != nil {
		return applyErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("scope state commit tx: %w", err)
	}

	return nil
}

func loadRemoteScopeRows(ctx context.Context, tx sqlTxRunner) ([]remoteScopeRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT
			rs.drive_id,
			rs.item_id,
			rs.path,
			rs.hash,
			rs.sync_status,
			rs.filter_generation,
			rs.filter_reason,
			b.path,
			b.remote_hash
		FROM remote_state rs
		LEFT JOIN baseline b
		  ON b.drive_id = rs.drive_id
		 AND b.item_id = rs.item_id
		WHERE rs.sync_status NOT IN (?, ?, ?, ?)`,
		synctypes.SyncStatusDeleted,
		synctypes.SyncStatusPendingDelete,
		synctypes.SyncStatusDownloading,
		synctypes.SyncStatusDeleting,
	)
	if err != nil {
		return nil, fmt.Errorf("scope state query remote rows: %w", err)
	}
	defer rows.Close()

	var allRows []remoteScopeRow
	for rows.Next() {
		var row remoteScopeRow
		if err := rows.Scan(
			&row.DriveID,
			&row.ItemID,
			&row.Path,
			&row.Hash,
			&row.Status,
			&row.FilterGeneration,
			&row.FilterReason,
			&row.BaselinePath,
			&row.BaselineHash,
		); err != nil {
			return nil, fmt.Errorf("scope state scan remote row: %w", err)
		}

		allRows = append(allRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scope state iterate remote rows: %w", err)
	}

	return allRows, nil
}

func applyRemoteScopeRows(
	ctx context.Context,
	tx sqlTxRunner,
	snapshot syncscope.Snapshot,
	generation int64,
	allRows []remoteScopeRow,
) (int, error) {
	changed := 0
	for i := range allRows {
		rowChanged, err := applyRemoteScopeRow(ctx, tx, snapshot, generation, &allRows[i])
		if err != nil {
			return 0, err
		}
		if rowChanged {
			changed++
		}
	}

	return changed, nil
}

func applyRemoteScopeRow(
	ctx context.Context,
	tx sqlTxRunner,
	snapshot syncscope.Snapshot,
	generation int64,
	row *remoteScopeRow,
) (bool, error) {
	reason := snapshot.ExclusionReason(row.Path)
	if reason != syncscope.ExclusionNone {
		if row.Status == synctypes.SyncStatusFiltered &&
			row.FilterGeneration == generation &&
			row.FilterReason == string(reason) {
			return false, nil
		}

		if _, err := tx.ExecContext(
			ctx,
			`UPDATE remote_state
			 SET sync_status = ?, filter_generation = ?, filter_reason = ?
			 WHERE drive_id = ? AND item_id = ?`,
			synctypes.SyncStatusFiltered,
			generation,
			string(reason),
			row.DriveID,
			row.ItemID,
		); err != nil {
			return false, fmt.Errorf("scope state filter %s: %w", row.Path, err)
		}

		return true, nil
	}

	if row.Status != synctypes.SyncStatusFiltered {
		return false, nil
	}

	nextStatus := reactivatedRemoteStatus(
		row.Path,
		row.Hash.String,
		row.BaselinePath.Valid,
		row.BaselinePath.String,
		row.BaselineHash.String,
	)
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE remote_state
		 SET sync_status = ?, filter_generation = 0, filter_reason = ''
		 WHERE drive_id = ? AND item_id = ?`,
		nextStatus,
		row.DriveID,
		row.ItemID,
	); err != nil {
		return false, fmt.Errorf("scope state reactivate %s: %w", row.Path, err)
	}

	return true, nil
}

func upsertScopeStateRow(ctx context.Context, tx sqlTxRunner, state synctypes.ScopeStateRecord) error {
	if state.EffectiveSnapshotJSON == "" {
		state.EffectiveSnapshotJSON = `{"version":1}`
	}
	if state.ObservationMode == "" {
		state.ObservationMode = synctypes.ScopeObservationRootDelta
	}
	if state.LastReconcileKind == "" {
		state.LastReconcileKind = synctypes.ScopeReconcileNone
	}
	if state.UpdatedAt <= 0 {
		return fmt.Errorf("scope state updated_at must be > 0")
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO scope_state (
			singleton, generation, effective_snapshot_json, observation_plan_hash,
			observation_mode, websocket_enabled, pending_reentry,
			last_reconcile_kind, updated_at
		) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			generation = excluded.generation,
			effective_snapshot_json = excluded.effective_snapshot_json,
			observation_plan_hash = excluded.observation_plan_hash,
			observation_mode = excluded.observation_mode,
			websocket_enabled = excluded.websocket_enabled,
			pending_reentry = excluded.pending_reentry,
			last_reconcile_kind = excluded.last_reconcile_kind,
			updated_at = excluded.updated_at
	`,
		state.Generation,
		state.EffectiveSnapshotJSON,
		state.ObservationPlanHash,
		state.ObservationMode,
		boolToInt(state.WebsocketEnabled),
		boolToInt(state.PendingReentry),
		state.LastReconcileKind,
		state.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("scope state upsert: %w", err)
	}

	return nil
}

type scopeStateRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readScopeStateTx(ctx context.Context, tx sqlTxRunner) (synctypes.ScopeStateRecord, bool, error) {
	return readScopeStateRecord(ctx, tx, "read scope state")
}

func readScopeStateRecord(
	ctx context.Context,
	querier scopeStateRowQuerier,
	errAction string,
) (synctypes.ScopeStateRecord, bool, error) {
	row := querier.QueryRowContext(ctx, `
		SELECT generation, effective_snapshot_json, observation_plan_hash,
		       observation_mode, websocket_enabled, pending_reentry,
		       last_reconcile_kind, updated_at
		FROM scope_state
		WHERE singleton = 1`,
	)

	var (
		record           synctypes.ScopeStateRecord
		websocketEnabled int
		pendingReentry   int
	)
	if err := row.Scan(
		&record.Generation,
		&record.EffectiveSnapshotJSON,
		&record.ObservationPlanHash,
		&record.ObservationMode,
		&websocketEnabled,
		&pendingReentry,
		&record.LastReconcileKind,
		&record.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows || isMissingTableErr(err) {
			return synctypes.ScopeStateRecord{}, false, nil
		}

		return synctypes.ScopeStateRecord{}, false, fmt.Errorf("%s: %w", errAction, err)
	}

	record.WebsocketEnabled = websocketEnabled != 0
	record.PendingReentry = pendingReentry != 0

	return record, true, nil
}

func repairScopeStateConsistencyTx(ctx context.Context, tx sqlTxRunner) (int, error) {
	state, found, err := readScopeStateTx(ctx, tx)
	if err != nil {
		return 0, err
	}

	allRows, err := loadRemoteScopeRows(ctx, tx)
	if err != nil {
		return 0, err
	}

	if !found {
		return applyRemoteScopeRows(ctx, tx, syncscope.Snapshot{}, 0, allRows)
	}

	snapshot, err := syncscope.UnmarshalSnapshot(state.EffectiveSnapshotJSON)
	if err != nil {
		droppedState, clearErr := clearScopeStateTx(ctx, tx)
		if clearErr != nil {
			return 0, clearErr
		}

		reactivatedRows, reactivateErr := applyRemoteScopeRows(ctx, tx, syncscope.Snapshot{}, 0, allRows)
		if reactivateErr != nil {
			return 0, reactivateErr
		}

		return droppedState + reactivatedRows, nil
	}

	return applyRemoteScopeRows(ctx, tx, snapshot, state.Generation, allRows)
}

func (m *SyncStore) repairScopeStateConsistencyOnOpen(ctx context.Context) (repairsApplied int, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return 0, fmt.Errorf("sync: begin scope-state repair tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback scope-state repair tx")
	}()

	repairsApplied, err = repairScopeStateConsistencyTx(ctx, tx)
	if err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("sync: commit scope-state repair: %w", err)
	}

	return repairsApplied, nil
}

func clearScopeStateTx(ctx context.Context, tx sqlTxRunner) (int, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM scope_state WHERE singleton = 1`)
	if err != nil {
		return 0, fmt.Errorf("delete invalid scope state: %w", err)
	}

	return rowsAffected(result), nil
}

func reactivatedRemoteStatus(path, hash string, baselineFound bool, baselinePath, baselineHash string) synctypes.SyncStatus {
	if baselineFound && baselinePath == path && baselineHash == hash {
		return synctypes.SyncStatusSynced
	}

	return synctypes.SyncStatusPendingDownload
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
