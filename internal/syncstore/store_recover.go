package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type RecoverAction string

const (
	RecoverActionNoState RecoverAction = "no_state"
	RecoverActionNoop    RecoverAction = "noop"
	RecoverActionRepair  RecoverAction = "repair"
	RecoverActionRebuild RecoverAction = "rebuild"
	RecoverActionReset   RecoverAction = "reset"
)

type RecoverResult struct {
	Action               RecoverAction
	RepairsApplied       int
	PreservedHeldDeletes int
	PreservedConflicts   int
	PreservedRequests    int
}

type RecoveredState struct {
	HeldDeletes []synctypes.HeldDeleteRecord
	Conflicts   []synctypes.ConflictRecord
	Requests    []synctypes.ConflictRequestRecord
}

func RecoverSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (RecoverResult, error) {
	if !pathExists(dbPath) {
		return RecoverResult{Action: RecoverActionNoState}, nil
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		repairs, repairErr := store.RepairIntegritySafe(ctx)
		if repairErr != nil {
			closeErr := store.Close(context.WithoutCancel(ctx))
			if closeErr != nil {
				return RecoverResult{}, errors.Join(
					fmt.Errorf("repair sync store: %w", repairErr),
					fmt.Errorf("close sync store: %w", closeErr),
				)
			}
			return RecoverResult{}, fmt.Errorf("repair sync store: %w", repairErr)
		}

		report, auditErr := store.AuditIntegrity(ctx)
		closeErr := store.Close(context.WithoutCancel(ctx))
		if auditErr != nil {
			if closeErr != nil {
				return RecoverResult{}, errors.Join(
					fmt.Errorf("audit sync store: %w", auditErr),
					fmt.Errorf("close sync store: %w", closeErr),
				)
			}
			return RecoverResult{}, fmt.Errorf("audit sync store: %w", auditErr)
		}
		if closeErr != nil {
			return RecoverResult{}, fmt.Errorf("close sync store: %w", closeErr)
		}

		if !report.HasFindings() {
			action := RecoverActionNoop
			if repairs > 0 {
				action = RecoverActionRepair
			}
			return RecoverResult{
				Action:         action,
				RepairsApplied: repairs,
			}, nil
		}
	}

	salvaged, rebuildErr := rebuildSyncStore(ctx, dbPath, logger)
	if rebuildErr == nil {
		return RecoverResult{
			Action:               RecoverActionRebuild,
			PreservedHeldDeletes: len(salvaged.HeldDeletes),
			PreservedConflicts:   len(salvaged.Conflicts),
			PreservedRequests:    len(salvaged.Requests),
		}, nil
	}

	if resetErr := resetSyncStore(ctx, dbPath, logger); resetErr != nil {
		return RecoverResult{}, errors.Join(
			fmt.Errorf("rebuild sync store: %w", rebuildErr),
			fmt.Errorf("reset sync store: %w", resetErr),
		)
	}

	return RecoverResult{Action: RecoverActionReset}, nil
}

func rebuildSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (RecoveredState, error) {
	recovered, err := readRecoverableState(ctx, dbPath)
	if err != nil {
		return RecoveredState{}, err
	}

	tempPath, err := tempRecoverDBPath(dbPath)
	if err != nil {
		return RecoveredState{}, err
	}

	store, err := NewSyncStore(ctx, tempPath, logger)
	if err != nil {
		return RecoveredState{}, fmt.Errorf("open rebuilt sync store: %w", err)
	}

	closeCtx := context.WithoutCancel(ctx)
	storeClosed := false
	defer func() {
		if storeClosed {
			return
		}
		if closeErr := store.Close(closeCtx); closeErr != nil {
			logger.Debug("close rebuilt sync store after recovery error", "error", closeErr.Error(), "path", tempPath)
		}
	}()

	if err := importRecoveredState(ctx, store, recovered); err != nil {
		return RecoveredState{}, err
	}
	if err := store.Close(closeCtx); err != nil {
		return RecoveredState{}, fmt.Errorf("close rebuilt sync store: %w", err)
	}
	storeClosed = true

	if err := replaceSyncStoreFiles(tempPath, dbPath); err != nil {
		return RecoveredState{}, err
	}

	return recovered, nil
}

func resetSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) error {
	if err := removeSyncStoreFiles(dbPath); err != nil {
		return err
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("create fresh sync store: %w", err)
	}
	if err := store.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close fresh sync store: %w", err)
	}

	return nil
}

func readRecoverableState(ctx context.Context, dbPath string) (RecoveredState, error) {
	db, err := openReadOnlySyncStoreDB(dbPath)
	if err != nil {
		return RecoveredState{}, fmt.Errorf("open existing sync store for recovery: %w", err)
	}
	defer db.Close()

	recovered := RecoveredState{}

	hasHeldDeletes, tableErr := tableExists(ctx, db, "held_deletes")
	if tableErr != nil {
		return RecoveredState{}, tableErr
	}
	if hasHeldDeletes {
		recovered.HeldDeletes, err = queryHeldDeletesForAudit(ctx, db)
		if err != nil {
			return RecoveredState{}, err
		}
	}

	recovered.Conflicts, err = readUnresolvedConflictsForRecovery(ctx, db)
	if err != nil {
		return RecoveredState{}, err
	}

	recovered.Requests, err = readConflictRequestsForRecovery(ctx, db)
	if err != nil {
		return RecoveredState{}, err
	}

	return recovered, nil
}

func readUnresolvedConflictsForRecovery(ctx context.Context, db *sql.DB) ([]synctypes.ConflictRecord, error) {
	return queryConflictRows(
		ctx,
		db,
		sqlListConflicts,
		"read unresolved conflicts for recovery",
		"iterate unresolved conflicts for recovery",
	)
}

func queryConflictRows(
	ctx context.Context,
	db *sql.DB,
	query string,
	queryOp string,
	iterOp string,
) ([]synctypes.ConflictRecord, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", queryOp, err)
	}
	defer rows.Close()

	var conflicts []synctypes.ConflictRecord
	for rows.Next() {
		record, scanErr := scanConflictRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		conflicts = append(conflicts, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterOp, err)
	}

	return conflicts, nil
}

func readConflictRequestsForRecovery(ctx context.Context, db *sql.DB) ([]synctypes.ConflictRequestRecord, error) {
	hasRequests, err := tableExists(ctx, db, "conflict_requests")
	if err != nil {
		return nil, err
	}
	if hasRequests {
		return readSplitConflictRequestsForRecovery(ctx, db)
	}

	return readLegacyConflictRequestsForRecovery(ctx, db)
}

func readSplitConflictRequestsForRecovery(ctx context.Context, db *sql.DB) ([]synctypes.ConflictRequestRecord, error) {
	columns, err := tableColumns(ctx, db, "conflict_requests")
	if err != nil {
		return nil, err
	}

	applyingColumn := "applying_at"
	if !columns[applyingColumn] {
		applyingColumn = "resolving_at"
	}
	lastErrorColumn := "last_error"
	if !columns[lastErrorColumn] {
		lastErrorColumn = "resolution_error"
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT conflict_id, requested_resolution, state, requested_at, %s, %s
		FROM conflict_requests
		ORDER BY requested_at, conflict_id`, applyingColumn, lastErrorColumn))
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read conflict requests for recovery: %w", err)
	}
	defer rows.Close()

	return scanRecoveredConflictRequests(
		rows,
		"scan conflict request for recovery",
		"iterate conflict requests for recovery",
	)
}

func readLegacyConflictRequestsForRecovery(ctx context.Context, db *sql.DB) ([]synctypes.ConflictRequestRecord, error) {
	columns, err := tableColumns(ctx, db, "conflicts")
	if err != nil {
		return nil, err
	}
	if !columns["state"] || !columns["requested_resolution"] {
		return nil, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, requested_resolution, state, requested_at, resolving_at, resolution_error
		FROM conflicts
		WHERE resolution = 'unresolved' AND state IN ('resolution_requested', 'resolving', 'resolve_failed')
		ORDER BY requested_at, id`)
	if err != nil {
		return nil, fmt.Errorf("read legacy conflict requests for recovery: %w", err)
	}
	defer rows.Close()

	return scanRecoveredConflictRequests(
		rows,
		"scan legacy conflict request for recovery",
		"iterate legacy conflict requests for recovery",
	)
}

func scanRecoveredConflictRequests(
	rows *sql.Rows,
	scanOp string,
	iterOp string,
) ([]synctypes.ConflictRequestRecord, error) {
	var requests []synctypes.ConflictRequestRecord
	for rows.Next() {
		var (
			record      synctypes.ConflictRequestRecord
			requestedAt sql.NullInt64
			applyingAt  sql.NullInt64
			lastErr     sql.NullString
		)
		if err := rows.Scan(
			&record.ID,
			&record.RequestedResolution,
			&record.State,
			&requestedAt,
			&applyingAt,
			&lastErr,
		); err != nil {
			return nil, fmt.Errorf("%s: %w", scanOp, err)
		}
		record.State = normalizeRecoveredConflictRequestState(record.State)
		if requestedAt.Valid {
			record.RequestedAt = requestedAt.Int64
		}
		if applyingAt.Valid && record.State == synctypes.ConflictStateApplying {
			record.ApplyingAt = applyingAt.Int64
		}
		record.LastError = lastErr.String
		requests = append(requests, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", iterOp, err)
	}

	return requests, nil
}

func normalizeRecoveredConflictRequestState(state string) string {
	switch state {
	case "resolution_requested", "resolve_failed", synctypes.ConflictStateQueued:
		return synctypes.ConflictStateQueued
	case "resolving", synctypes.ConflictStateApplying:
		return synctypes.ConflictStateApplying
	default:
		return state
	}
}

func importRecoveredState(ctx context.Context, store *SyncStore, recovered RecoveredState) error {
	for i := range recovered.Conflicts {
		row := recovered.Conflicts[i]
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO conflicts (
				id, drive_id, item_id, path, conflict_type, detected_at,
				local_hash, remote_hash, local_mtime, remote_mtime,
				resolution, resolved_at, resolved_by
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)`,
			row.ID,
			row.DriveID.String(),
			nullString(row.ItemID),
			row.Path,
			row.ConflictType,
			row.DetectedAt,
			nullString(row.LocalHash),
			nullString(row.RemoteHash),
			nullOptionalInt64(row.LocalMtime),
			nullOptionalInt64(row.RemoteMtime),
			synctypes.ResolutionUnresolved,
		)
		if err != nil {
			return fmt.Errorf("import unresolved conflict %s: %w", row.ID, err)
		}
	}

	if len(recovered.HeldDeletes) > 0 {
		if err := store.UpsertHeldDeletes(ctx, recovered.HeldDeletes); err != nil {
			return fmt.Errorf("import held deletes: %w", err)
		}
	}

	for i := range recovered.Requests {
		row := recovered.Requests[i]
		_, err := store.DB().ExecContext(ctx, `
			INSERT INTO conflict_requests (
				conflict_id, requested_resolution, state, requested_at, applying_at, last_error
			) VALUES (?, ?, ?, ?, ?, ?)`,
			row.ID,
			row.RequestedResolution,
			row.State,
			nullOptionalInt64(row.RequestedAt),
			nullOptionalInt64(row.ApplyingAt),
			nullString(row.LastError),
		)
		if err != nil {
			return fmt.Errorf("import conflict request %s: %w", row.ID, err)
		}
	}

	return nil
}

func tempRecoverDBPath(dbPath string) (string, error) {
	tempFile, err := localpath.CreateTemp(filepath.Dir(dbPath), "recover-*.db")
	if err != nil {
		return "", fmt.Errorf("create temporary recovery DB: %w", err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close temporary recovery DB: %w", err)
	}
	if err := localpath.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove temporary recovery placeholder: %w", err)
	}

	return tempPath, nil
}

func replaceSyncStoreFiles(tempPath, dbPath string) error {
	if err := removeSyncStoreFiles(dbPath); err != nil {
		return err
	}
	if err := localpath.Rename(tempPath, dbPath); err != nil {
		return fmt.Errorf("replace sync store DB: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		tempSidecar := tempPath + suffix
		if !pathExists(tempSidecar) {
			continue
		}
		if err := localpath.Rename(tempSidecar, dbPath+suffix); err != nil {
			return fmt.Errorf("replace sync store sidecar %s: %w", suffix, err)
		}
	}

	return nil
}

func removeSyncStoreFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove sync store file %s: %w", candidate, err)
		}
	}

	return nil
}

func tableColumns(ctx context.Context, db *sql.DB, tableName string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return nil, fmt.Errorf("inspect columns for %s: %w", tableName, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan columns for %s: %w", tableName, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns for %s: %w", tableName, err)
	}

	return columns, nil
}

func pathExists(path string) bool {
	_, err := localpath.Stat(path)
	return err == nil
}
