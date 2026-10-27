package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	sqlReadObservationState = `SELECT
				content_drive_id,
				cursor,
				next_full_remote_refresh_at,
				local_truth_complete,
				local_truth_recovery_reason,
				sync_root_bound,
				sync_root_device,
				sync_root_inode,
				sync_root_fsid
			FROM observation_state
			LIMIT 1`
	sqlEnsureObservationStateRow = `INSERT INTO observation_state
		(content_drive_id, cursor, next_full_remote_refresh_at, local_truth_complete, local_truth_recovery_reason)
		SELECT '', '', 0, 0, ''
		WHERE NOT EXISTS (SELECT 1 FROM observation_state)`
)

const (
	remoteRefreshEnumerateInterval = time.Hour
)

type observationState struct {
	ContentDriveID           driveid.ID
	Cursor                   string
	NextFullRemoteRefreshAt  int64
	LocalTruthComplete       bool
	LocalTruthRecoveryReason string
	// SyncRootIdentity is the fingerprint of the local folder this state was
	// built against, recorded with the first committed local snapshot. Nil
	// until then, and again after an explicit rebind.
	SyncRootIdentity *synctree.RootIdentity
}

const (
	localTruthRecoveryDroppedEvents  = "dropped_local_events"
	localTruthRecoveryWatcherError   = "watcher_error"
	localTruthRecoveryFullScanFailed = "full_local_scan_failed"
)

func remoteRefreshIntervalForMode(mode remoteObservationMode) time.Duration {
	switch mode {
	case remoteObservationModeDelta:
		return fullRemoteRefreshInterval
	case remoteObservationModeEnumerate:
		return remoteRefreshEnumerateInterval
	default:
		return fullRemoteRefreshInterval
	}
}

func applyRemoteRefreshSchedule(state *observationState, at time.Time, mode remoteObservationMode) {
	state.NextFullRemoteRefreshAt = at.Add(remoteRefreshIntervalForMode(mode)).UnixNano()
}

func (s *SyncStore) contentDriveIDForRead(
	ctx context.Context,
	fallback driveid.ID,
) (driveid.ID, error) {
	if cached := s.contentDriveID(); !cached.IsZero() {
		return cached, nil
	}

	contentDriveID, err := contentDriveIDForDB(ctx, s.db)
	if err != nil {
		return driveid.ID{}, err
	}
	if !contentDriveID.IsZero() {
		s.rememberContentDriveID(contentDriveID)
		return contentDriveID, nil
	}

	if !fallback.IsZero() {
		s.rememberContentDriveID(fallback)
		return fallback, nil
	}

	return driveid.ID{}, nil
}

func contentDriveIDForDB(ctx context.Context, runner sqlTxRunner) (driveid.ID, error) {
	var raw string
	if err := runner.QueryRowContext(ctx,
		`SELECT content_drive_id FROM observation_state LIMIT 1`,
	).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return driveid.ID{}, nil
		}
		return driveid.ID{}, fmt.Errorf("sync: reading content drive ID: %w", err)
	}
	if raw == "" {
		return driveid.ID{}, nil
	}

	return driveid.New(raw), nil
}

func ensureMatchingContentDriveID(expected, actual driveid.ID) error {
	if expected.IsZero() || actual.IsZero() {
		return nil
	}
	if expected.Equal(actual) {
		return nil
	}

	return fmt.Errorf("sync: the saved sync state belongs to a different OneDrive drive (stored %s, now %s); "+
		"reset it with onedrive-go drive reset-sync-state", actual, expected)
}

func (s *SyncStore) ReadObservationState(ctx context.Context) (*observationState, error) {
	if _, err := s.db.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring observation_state row: %w", err)
	}

	var (
		contentDriveID string
		state          observationState
		localComplete  int
		rootBound      int
		rootDevice     int64
		rootInode      int64
		rootFSID       int64
	)

	if err := s.db.QueryRowContext(ctx, sqlReadObservationState).Scan(
		&contentDriveID,
		&state.Cursor,
		&state.NextFullRemoteRefreshAt,
		&localComplete,
		&state.LocalTruthRecoveryReason,
		&rootBound,
		&rootDevice,
		&rootInode,
		&rootFSID,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}
	state.LocalTruthComplete = localComplete != 0
	state.SyncRootIdentity = syncRootIdentityFromColumns(rootBound, rootDevice, rootInode, rootFSID)

	if contentDriveID != "" {
		state.ContentDriveID = driveid.New(contentDriveID)
		s.rememberContentDriveID(state.ContentDriveID)
	}

	return &state, nil
}

func (s *SyncStore) CommitObservationCursor(
	ctx context.Context,
	driveID driveid.ID,
	cursor string,
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning observation cursor transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation cursor transaction")
	}()

	state, err := s.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}

	state.Cursor = cursor
	if writeErr := s.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation cursor transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) ClearObservationCursor(ctx context.Context) error {
	state, err := s.ReadObservationState(ctx)
	if err != nil {
		return err
	}
	state.Cursor = ""
	return s.replaceObservationState(ctx, state)
}

func (s *SyncStore) MarkFullRemoteRefresh(
	ctx context.Context,
	driveID driveid.ID,
	at time.Time,
	mode remoteObservationMode,
) error {
	return s.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full remote refresh transaction",
		"sync: rollback full remote refresh transaction",
		"sync: committing full remote refresh transaction",
		func(state *observationState) {
			applyRemoteRefreshSchedule(state, at, mode)
		},
	)
}

func (s *SyncStore) ClampFullRemoteRefreshDeadline(
	ctx context.Context,
	driveID driveid.ID,
	notAfter time.Time,
) (bool, error) {
	if notAfter.IsZero() {
		return false, nil
	}

	deadline := notAfter.UnixNano()
	changed := false
	err := s.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full remote refresh clamp transaction",
		"sync: rollback full remote refresh clamp transaction",
		"sync: committing full remote refresh clamp transaction",
		func(state *observationState) {
			if state.NextFullRemoteRefreshAt == 0 || state.NextFullRemoteRefreshAt > deadline {
				state.NextFullRemoteRefreshAt = deadline
				changed = true
			}
		},
	)
	return changed, err
}

func (s *SyncStore) markObservationRefresh(
	ctx context.Context,
	driveID driveid.ID,
	beginMessage string,
	rollbackMessage string,
	commitMessage string,
	update func(*observationState),
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("%s: %w", beginMessage, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, rollbackMessage)
	}()

	state, err := s.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}

	update(state)
	if writeErr := s.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("%s: %w", commitMessage, commitErr)
	}

	return nil
}

func (s *SyncStore) contentDriveID() driveid.ID {
	s.contentDriveMu.RLock()
	defer s.contentDriveMu.RUnlock()

	return s.cachedContentDriveID
}

func (s *SyncStore) rememberContentDriveID(id driveid.ID) {
	if id.IsZero() {
		return
	}

	s.contentDriveMu.Lock()
	defer s.contentDriveMu.Unlock()

	s.cachedContentDriveID = id
}

func (s *SyncStore) readObservationStateTx(
	ctx context.Context,
	tx sqlTxRunner,
) (*observationState, error) {
	return readObservationStateFromTx(ctx, tx)
}

func readObservationStateFromTx(
	ctx context.Context,
	tx sqlTxRunner,
) (*observationState, error) {
	if _, err := tx.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring observation_state row: %w", err)
	}

	var (
		contentDriveID string
		state          observationState
		localComplete  int
		rootBound      int
		rootDevice     int64
		rootInode      int64
		rootFSID       int64
	)

	if err := tx.QueryRowContext(ctx, sqlReadObservationState).Scan(
		&contentDriveID,
		&state.Cursor,
		&state.NextFullRemoteRefreshAt,
		&localComplete,
		&state.LocalTruthRecoveryReason,
		&rootBound,
		&rootDevice,
		&rootInode,
		&rootFSID,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}
	state.LocalTruthComplete = localComplete != 0
	state.SyncRootIdentity = syncRootIdentityFromColumns(rootBound, rootDevice, rootInode, rootFSID)

	if contentDriveID != "" {
		state.ContentDriveID = driveid.New(contentDriveID)
	}

	return &state, nil
}

func (s *SyncStore) ensureContentDriveIDTx(
	ctx context.Context,
	tx sqlTxRunner,
	driveID driveid.ID,
	state *observationState,
) error {
	if driveID.IsZero() {
		return nil
	}

	if state.ContentDriveID.IsZero() {
		state.ContentDriveID = driveID
		if err := s.writeObservationStateTx(ctx, tx, state); err != nil {
			return err
		}
		s.rememberContentDriveID(driveID)
		return nil
	}

	if err := ensureMatchingContentDriveID(driveID, state.ContentDriveID); err != nil {
		return err
	}

	s.rememberContentDriveID(state.ContentDriveID)
	return nil
}

func (s *SyncStore) writeObservationStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	state *observationState,
) error {
	if err := writeObservationStateToTx(ctx, tx, state); err != nil {
		return err
	}

	if !state.ContentDriveID.IsZero() {
		s.rememberContentDriveID(state.ContentDriveID)
	}

	return nil
}

func writeObservationStateToTx(
	ctx context.Context,
	tx sqlTxRunner,
	state *observationState,
) error {
	rootIdentity := state.SyncRootIdentity
	if rootIdentity == nil {
		rootIdentity = &synctree.RootIdentity{}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM observation_state`); err != nil {
		return fmt.Errorf("sync: clearing observation_state before write: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO observation_state
			(content_drive_id, cursor, next_full_remote_refresh_at, local_truth_complete, local_truth_recovery_reason,
			 sync_root_bound, sync_root_device, sync_root_inode, sync_root_fsid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		state.ContentDriveID.String(),
		state.Cursor,
		state.NextFullRemoteRefreshAt,
		boolInt(state.LocalTruthComplete),
		state.LocalTruthRecoveryReason,
		boolInt(state.SyncRootIdentity != nil),
		int64(rootIdentity.Device),       //nolint:gosec // SQLite INTEGER stores the uint64 bit pattern
		int64(rootIdentity.Inode),        //nolint:gosec // SQLite INTEGER stores the uint64 bit pattern
		int64(rootIdentity.FileSystemID), //nolint:gosec // SQLite INTEGER stores the uint64 bit pattern
	); err != nil {
		return fmt.Errorf("sync: writing observation_state: %w", err)
	}

	return nil
}

func syncRootIdentityFromColumns(bound int, device, inode, fsid int64) *synctree.RootIdentity {
	if bound == 0 {
		return nil
	}

	return &synctree.RootIdentity{
		Device:       uint64(device), //nolint:gosec // restores the stored uint64 bit pattern
		Inode:        uint64(inode),  //nolint:gosec // restores the stored uint64 bit pattern
		FileSystemID: uint64(fsid),   //nolint:gosec // restores the stored uint64 bit pattern
	}
}

// BindSyncRootIdentity records the fingerprint of the local folder this state
// belongs to. It never overwrites an existing binding: replacing it is the
// explicit rebind decision (ClearSyncRootIdentity), never a side effect of a
// scan that may itself be looking at the wrong folder.
func (s *SyncStore) BindSyncRootIdentity(ctx context.Context, identity synctree.RootIdentity) error {
	return s.updateObservationState(ctx, func(state *observationState) {
		if state.SyncRootIdentity != nil {
			return
		}
		bound := identity
		state.SyncRootIdentity = &bound
	})
}

// ClearSyncRootIdentity forgets the recorded sync-folder fingerprint, so the
// next engine start binds the folder it finds. It is the explicit rebind the
// user asks for after moving or restoring the sync folder on purpose.
func (s *SyncStore) ClearSyncRootIdentity(ctx context.Context) error {
	return s.updateObservationState(ctx, func(state *observationState) {
		state.SyncRootIdentity = nil
	})
}

func (s *SyncStore) MarkLocalTruthSuspect(ctx context.Context, reason string) error {
	return s.updateObservationState(ctx, func(state *observationState) {
		state.LocalTruthComplete = false
		state.LocalTruthRecoveryReason = reason
	})
}

func (s *SyncStore) updateObservationState(
	ctx context.Context,
	update func(*observationState),
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: begin observation-state update tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation-state update tx")
	}()

	state, err := s.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	update(state)
	if writeErr := s.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit observation-state update tx: %w", err)
	}

	return nil
}

func (s *SyncStore) replaceObservationState(ctx context.Context, state *observationState) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: begin observation-state replace tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation-state replace tx")
	}()

	if writeErr := s.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit observation-state replace tx: %w", err)
	}

	return nil
}
