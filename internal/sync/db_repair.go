package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type StateDBRepairAction string

const (
	StateDBRepairNoState StateDBRepairAction = "no_state"
	StateDBRepairNoop    StateDBRepairAction = "noop"
	StateDBRepairRepair  StateDBRepairAction = "repair"
	StateDBRepairRebuild StateDBRepairAction = "rebuild"
	StateDBRepairReset   StateDBRepairAction = "reset"
)

type StateDBRepairResult struct {
	Action               StateDBRepairAction
	RepairsApplied       int
	PreservedHeldDeletes int
	PreservedConflicts   int
	PreservedRequests    int
}

type salvagedState struct {
	HeldDeletes []HeldDeleteRecord
	Conflicts   []ConflictRecord
	Requests    []ConflictRequestRecord
}

func RepairStateDB(ctx context.Context, dbPath string, logger *slog.Logger) (StateDBRepairResult, error) {
	if !pathExists(dbPath) {
		return StateDBRepairResult{Action: StateDBRepairNoState}, nil
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		repairs, repairErr := store.RepairIntegritySafe(ctx)
		if repairErr != nil {
			closeErr := store.Close(context.WithoutCancel(ctx))
			if closeErr != nil {
				return StateDBRepairResult{}, errors.Join(
					fmt.Errorf("repair state DB: %w", repairErr),
					fmt.Errorf("close state DB: %w", closeErr),
				)
			}
			return StateDBRepairResult{}, fmt.Errorf("repair state DB: %w", repairErr)
		}

		report, auditErr := store.AuditIntegrity(ctx)
		closeErr := store.Close(context.WithoutCancel(ctx))
		if auditErr != nil {
			if closeErr != nil {
				return StateDBRepairResult{}, errors.Join(
					fmt.Errorf("audit state DB: %w", auditErr),
					fmt.Errorf("close state DB: %w", closeErr),
				)
			}
			return StateDBRepairResult{}, fmt.Errorf("audit state DB: %w", auditErr)
		}
		if closeErr != nil {
			return StateDBRepairResult{}, fmt.Errorf("close state DB: %w", closeErr)
		}

		if !report.HasFindings() {
			action := StateDBRepairNoop
			if repairs > 0 {
				action = StateDBRepairRepair
			}
			return StateDBRepairResult{
				Action:         action,
				RepairsApplied: repairs,
			}, nil
		}
	}

	salvaged, rebuildErr := rebuildStateDB(ctx, dbPath, logger)
	if rebuildErr == nil {
		return StateDBRepairResult{
			Action:               StateDBRepairRebuild,
			PreservedHeldDeletes: len(salvaged.HeldDeletes),
			PreservedConflicts:   len(salvaged.Conflicts),
			PreservedRequests:    len(salvaged.Requests),
		}, nil
	}

	if resetErr := resetStateDB(ctx, dbPath, logger); resetErr != nil {
		return StateDBRepairResult{}, errors.Join(
			fmt.Errorf("rebuild state DB: %w", rebuildErr),
			fmt.Errorf("reset state DB: %w", resetErr),
		)
	}

	return StateDBRepairResult{Action: StateDBRepairReset}, nil
}

func rebuildStateDB(ctx context.Context, dbPath string, logger *slog.Logger) (salvagedState, error) {
	salvaged, err := readSalvageableState(ctx, dbPath)
	if err != nil {
		return salvagedState{}, err
	}

	tempPath, err := tempStateDBRepairPath(dbPath)
	if err != nil {
		return salvagedState{}, err
	}

	store, err := NewSyncStore(ctx, tempPath, logger)
	if err != nil {
		return salvagedState{}, fmt.Errorf("open rebuilt state DB: %w", err)
	}

	closeCtx := context.WithoutCancel(ctx)
	storeClosed := false
	defer func() {
		if storeClosed {
			return
		}
		if closeErr := store.Close(closeCtx); closeErr != nil {
			logger.Debug("close rebuilt state DB after repair error", "error", closeErr.Error(), "path", tempPath)
		}
	}()

	if err := importSalvagedState(ctx, store, salvaged); err != nil {
		return salvagedState{}, err
	}
	if err := store.Close(closeCtx); err != nil {
		return salvagedState{}, fmt.Errorf("close rebuilt state DB: %w", err)
	}
	storeClosed = true

	if err := replaceStateDBFiles(tempPath, dbPath); err != nil {
		return salvagedState{}, err
	}

	return salvaged, nil
}

func resetStateDB(ctx context.Context, dbPath string, logger *slog.Logger) error {
	if err := removeStateDBFiles(dbPath); err != nil {
		return err
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("create fresh state DB: %w", err)
	}
	if err := store.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close fresh state DB: %w", err)
	}

	return nil
}

func readSalvageableState(ctx context.Context, dbPath string) (salvagedState, error) {
	db, err := openReadOnlySyncStoreDB(dbPath)
	if err != nil {
		return salvagedState{}, fmt.Errorf("open existing state DB for repair: %w", err)
	}
	defer db.Close()

	salvaged := salvagedState{}

	hasHeldDeletes, tableErr := tableExists(ctx, db, "held_deletes")
	if tableErr != nil {
		return salvagedState{}, tableErr
	}
	if hasHeldDeletes {
		salvaged.HeldDeletes, err = queryHeldDeletesForAudit(ctx, db)
		if err != nil {
			return salvagedState{}, err
		}
	}

	salvaged.Conflicts, err = readUnresolvedConflictsForRepair(ctx, db)
	if err != nil {
		return salvagedState{}, err
	}

	salvaged.Requests, err = readConflictRequestsForRepair(ctx, db)
	if err != nil {
		return salvagedState{}, err
	}

	return salvaged, nil
}

func readUnresolvedConflictsForRepair(ctx context.Context, db *sql.DB) ([]ConflictRecord, error) {
	return queryConflictRows(
		ctx,
		db,
		sqlListConflicts,
		"read unresolved conflicts for state DB repair",
		"iterate unresolved conflicts for state DB repair",
	)
}

func queryConflictRows(
	ctx context.Context,
	db *sql.DB,
	query string,
	queryOp string,
	iterOp string,
) ([]ConflictRecord, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", queryOp, err)
	}
	defer rows.Close()

	var conflicts []ConflictRecord
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

func readConflictRequestsForRepair(ctx context.Context, db *sql.DB) ([]ConflictRequestRecord, error) {
	hasRequests, err := tableExists(ctx, db, "conflict_requests")
	if err != nil {
		return nil, err
	}
	if hasRequests {
		return readSplitConflictRequestsForRepair(ctx, db)
	}

	return readLegacyConflictRequestsForRepair(ctx, db)
}

func readSplitConflictRequestsForRepair(ctx context.Context, db *sql.DB) ([]ConflictRequestRecord, error) {
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
		return nil, fmt.Errorf("read conflict requests for state DB repair: %w", err)
	}
	defer rows.Close()

	return scanSalvagedConflictRequests(
		rows,
		"scan conflict request for state DB repair",
		"iterate conflict requests for state DB repair",
	)
}

func readLegacyConflictRequestsForRepair(ctx context.Context, db *sql.DB) ([]ConflictRequestRecord, error) {
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
		return nil, fmt.Errorf("read legacy conflict requests for state DB repair: %w", err)
	}
	defer rows.Close()

	return scanSalvagedConflictRequests(
		rows,
		"scan legacy conflict request for state DB repair",
		"iterate legacy conflict requests for state DB repair",
	)
}

func scanSalvagedConflictRequests(
	rows *sql.Rows,
	scanOp string,
	iterOp string,
) ([]ConflictRequestRecord, error) {
	var requests []ConflictRequestRecord
	for rows.Next() {
		var (
			record      ConflictRequestRecord
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
		record.State = normalizeSalvagedConflictRequestState(record.State)
		if requestedAt.Valid {
			record.RequestedAt = requestedAt.Int64
		}
		if applyingAt.Valid && record.State == ConflictStateApplying {
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

func normalizeSalvagedConflictRequestState(state string) string {
	switch state {
	case "resolution_requested", "resolve_failed", ConflictStateQueued:
		return ConflictStateQueued
	case "resolving", ConflictStateApplying:
		return ConflictStateApplying
	default:
		return state
	}
}

func importSalvagedState(ctx context.Context, store *SyncStore, salvaged salvagedState) error {
	for i := range salvaged.Conflicts {
		row := salvaged.Conflicts[i]
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
			ResolutionUnresolved,
		)
		if err != nil {
			return fmt.Errorf("import unresolved conflict %s: %w", row.ID, err)
		}
	}

	if len(salvaged.HeldDeletes) > 0 {
		if err := store.UpsertHeldDeletes(ctx, salvaged.HeldDeletes); err != nil {
			return fmt.Errorf("import held deletes: %w", err)
		}
	}

	for i := range salvaged.Requests {
		row := salvaged.Requests[i]
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

func tempStateDBRepairPath(dbPath string) (string, error) {
	tempFile, err := localpath.CreateTemp(filepath.Dir(dbPath), "state-db-repair-*.db")
	if err != nil {
		return "", fmt.Errorf("create temporary state DB: %w", err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close temporary state DB: %w", err)
	}
	if err := localpath.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove temporary state DB placeholder: %w", err)
	}

	return tempPath, nil
}

func replaceStateDBFiles(tempPath, dbPath string) error {
	if err := removeStateDBFiles(dbPath); err != nil {
		return err
	}
	if err := localpath.Rename(tempPath, dbPath); err != nil {
		return fmt.Errorf("replace state DB: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		tempSidecar := tempPath + suffix
		if !pathExists(tempSidecar) {
			continue
		}
		if err := localpath.Rename(tempSidecar, dbPath+suffix); err != nil {
			return fmt.Errorf("replace state DB sidecar %s: %w", suffix, err)
		}
	}

	return nil
}

func removeStateDBFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
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
