package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	sqlInsertScratchBaseline = `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash,
		 local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	sqlListScratchRemoteState = `SELECT item_id, path, parent_id, item_type,
		hash, size, mtime, etag, content_identity, previous_path
		FROM remote_state
		ORDER BY path`
	sqlInsertScratchRemoteState = `INSERT INTO remote_state
		(item_id, path, parent_id, item_type, hash, size, mtime, etag,
		 content_identity, previous_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

type scratchPlanningSeed struct {
	observationState *ObservationState
	baselineEntries  []BaselineEntry
	remoteRows       []RemoteStateRow
}

// createScratchPlanningStore opens a temporary SyncStore and seeds it with the
// committed baseline, remote mirror, and observation state needed for dry-run
// planning. The caller owns the returned cleanup function.
func (m *SyncStore) createScratchPlanningStore(
	ctx context.Context,
	baseline *Baseline,
) (_ *SyncStore, cleanup func(context.Context) error, err error) {
	if baseline == nil {
		return nil, nil, fmt.Errorf("sync: scratch planning store requires baseline")
	}

	seed, err := m.readScratchPlanningSeed(ctx, baseline)
	if err != nil {
		return nil, nil, err
	}

	scratchDir, err := os.MkdirTemp("", "onedrive-go-dry-run-*")
	if err != nil {
		return nil, nil, fmt.Errorf("sync: creating scratch planning directory: %w", err)
	}

	scratchPath := filepath.Join(scratchDir, "scratch.db")
	scratch, err := NewSyncStore(ctx, scratchPath, m.logger)
	if err != nil {
		if removeErr := localpath.RemoveAll(scratchDir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove scratch planning directory: %w", removeErr))
		}
		return nil, nil, fmt.Errorf("sync: opening scratch planning store: %w", err)
	}

	cleanup = func(cleanupCtx context.Context) error {
		closeErr := scratch.Close(cleanupCtx)
		removeErr := localpath.RemoveAll(scratchDir)

		switch {
		case closeErr != nil && removeErr != nil:
			return errors.Join(
				fmt.Errorf("close scratch planning store: %w", closeErr),
				fmt.Errorf("remove scratch planning directory: %w", removeErr),
			)
		case closeErr != nil:
			return fmt.Errorf("close scratch planning store: %w", closeErr)
		case removeErr != nil:
			return fmt.Errorf("remove scratch planning directory: %w", removeErr)
		default:
			return nil
		}
	}

	if err := scratch.applyScratchPlanningSeed(ctx, seed); err != nil {
		cleanupErr := cleanup(context.WithoutCancel(ctx))
		if cleanupErr != nil {
			return nil, nil, errors.Join(
				fmt.Errorf("sync: seeding scratch planning store: %w", err),
				cleanupErr,
			)
		}
		return nil, nil, fmt.Errorf("sync: seeding scratch planning store: %w", err)
	}

	return scratch, cleanup, nil
}

func (m *SyncStore) readScratchPlanningSeed(
	ctx context.Context,
	baseline *Baseline,
) (seed scratchPlanningSeed, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return scratchPlanningSeed{}, fmt.Errorf("sync: beginning scratch planning seed read transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback scratch planning seed read transaction")
	}()

	observationState, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return scratchPlanningSeed{}, fmt.Errorf("sync: reading scratch planning observation state: %w", err)
	}

	remoteRows, err := listScratchRemoteStateRows(ctx, tx)
	if err != nil {
		return scratchPlanningSeed{}, err
	}

	return scratchPlanningSeed{
		observationState: observationState,
		baselineEntries:  cloneBaselineEntries(baseline),
		remoteRows:       remoteRows,
	}, nil
}

func (m *SyncStore) applyScratchPlanningSeed(ctx context.Context, seed scratchPlanningSeed) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning scratch planning seed transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback scratch planning seed transaction")
	}()

	if err := m.writeObservationStateTx(ctx, tx, seed.observationState); err != nil {
		return fmt.Errorf("sync: writing scratch planning observation state: %w", err)
	}

	if err := insertScratchBaselineEntries(ctx, tx, seed.baselineEntries); err != nil {
		return err
	}

	if err := insertScratchRemoteStateRows(ctx, tx, seed.remoteRows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing scratch planning seed transaction: %w", err)
	}

	return nil
}

func cloneBaselineEntries(baseline *Baseline) []BaselineEntry {
	if baseline == nil {
		return nil
	}

	entries := make([]BaselineEntry, 0, baseline.Len())
	baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
		if entry == nil {
			return
		}
		entries = append(entries, *entry)
	})

	return entries
}

func insertScratchBaselineEntries(
	ctx context.Context,
	tx sqlTxRunner,
	entries []BaselineEntry,
) error {
	for i := range entries {
		entry := entries[i]
		if _, err := tx.ExecContext(ctx, sqlInsertScratchBaseline,
			entry.ItemID,
			entry.Path,
			nullString(entry.ParentID),
			entry.ItemType,
			nullString(entry.LocalHash),
			nullString(entry.RemoteHash),
			nullKnownInt64(entry.LocalSize, entry.LocalSizeKnown),
			nullKnownInt64(entry.RemoteSize, entry.RemoteSizeKnown),
			nullOptionalInt64(entry.LocalMtime),
			nullOptionalInt64(entry.RemoteMtime),
			nullString(entry.ETag),
		); err != nil {
			return fmt.Errorf("sync: inserting scratch baseline row for %s: %w", entry.Path, err)
		}
	}

	return nil
}

func listScratchRemoteStateRows(ctx context.Context, runner sqlTxRunner) ([]RemoteStateRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListScratchRemoteState)
	if err != nil {
		return nil, fmt.Errorf("sync: querying scratch remote_state seed rows: %w", err)
	}
	defer rows.Close()

	var result []RemoteStateRow
	for rows.Next() {
		var (
			row             RemoteStateRow
			parentID        sql.NullString
			hash            sql.NullString
			size            sql.NullInt64
			mtime           sql.NullInt64
			etag            sql.NullString
			contentIdentity sql.NullString
			previousPath    sql.NullString
		)

		if err := rows.Scan(
			&row.ItemID,
			&row.Path,
			&parentID,
			&row.ItemType,
			&hash,
			&size,
			&mtime,
			&etag,
			&contentIdentity,
			&previousPath,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning scratch remote_state seed row: %w", err)
		}

		row.ParentID = parentID.String
		row.Hash = hash.String
		row.ETag = etag.String
		row.ContentIdentity = contentIdentity.String
		row.PreviousPath = previousPath.String
		if size.Valid {
			row.Size = size.Int64
		}
		if mtime.Valid {
			row.Mtime = mtime.Int64
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating scratch remote_state seed rows: %w", err)
	}

	return result, nil
}

func insertScratchRemoteStateRows(
	ctx context.Context,
	tx sqlTxRunner,
	rows []RemoteStateRow,
) error {
	for i := range rows {
		row := rows[i]
		if _, err := tx.ExecContext(ctx, sqlInsertScratchRemoteState,
			row.ItemID,
			row.Path,
			nullString(row.ParentID),
			row.ItemType,
			nullString(row.Hash),
			nullKnownInt64(row.Size, true),
			nullOptionalInt64(row.Mtime),
			nullString(row.ETag),
			nullString(row.ContentIdentity),
			nullString(row.PreviousPath),
		); err != nil {
			return fmt.Errorf("sync: inserting scratch remote_state row for %s: %w", row.Path, err)
		}
	}

	return nil
}
