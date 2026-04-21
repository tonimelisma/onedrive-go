package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	sqlReadObservationState = `SELECT
			configured_drive_id,
			cursor,
			remote_refresh_mode,
			last_full_remote_refresh_at,
			next_full_remote_refresh_at,
			local_refresh_mode,
			last_full_local_refresh_at,
			next_full_local_refresh_at
		FROM observation_state WHERE singleton_id = 1`
	sqlEnsureObservationStateRow = `INSERT INTO observation_state
		(singleton_id, configured_drive_id, cursor,
		 remote_refresh_mode, last_full_remote_refresh_at, next_full_remote_refresh_at,
		 local_refresh_mode, last_full_local_refresh_at, next_full_local_refresh_at)
		VALUES (1, '', '', '', 0, 0, '', 0, 0)
		ON CONFLICT(singleton_id) DO NOTHING`
	sqlUpsertObservationState = `INSERT INTO observation_state
		(singleton_id, configured_drive_id, cursor,
		 remote_refresh_mode, last_full_remote_refresh_at, next_full_remote_refresh_at,
		 local_refresh_mode, last_full_local_refresh_at, next_full_local_refresh_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			configured_drive_id = excluded.configured_drive_id,
			cursor = excluded.cursor,
			remote_refresh_mode = excluded.remote_refresh_mode,
			last_full_remote_refresh_at = excluded.last_full_remote_refresh_at,
			next_full_remote_refresh_at = excluded.next_full_remote_refresh_at,
			local_refresh_mode = excluded.local_refresh_mode,
			last_full_local_refresh_at = excluded.last_full_local_refresh_at,
			next_full_local_refresh_at = excluded.next_full_local_refresh_at`
)

const (
	remoteRefreshModeDeltaHealthy  = "delta_healthy"
	remoteRefreshModeDeltaDegraded = "delta_degraded"
	localRefreshModeWatchHealthy   = "watch_healthy"
	localRefreshModeWatchDegraded  = "watch_degraded"

	remoteRefreshDegradedInterval = time.Hour
	localRefreshDegradedInterval  = time.Hour
)

type ObservationState struct {
	ConfiguredDriveID       driveid.ID
	Cursor                  string
	RemoteRefreshMode       string
	LastFullRemoteRefreshAt int64
	NextFullRemoteRefreshAt int64
	LocalRefreshMode        string
	LastFullLocalRefreshAt  int64
	NextFullLocalRefreshAt  int64
}

func normalizeRemoteRefreshMode(mode string) string {
	switch mode {
	case remoteRefreshModeDeltaHealthy, remoteRefreshModeDeltaDegraded:
		return mode
	default:
		return remoteRefreshModeDeltaHealthy
	}
}

func normalizeLocalRefreshMode(mode string) string {
	switch mode {
	case localRefreshModeWatchHealthy, localRefreshModeWatchDegraded:
		return mode
	default:
		return localRefreshModeWatchHealthy
	}
}

func remoteRefreshIntervalForMode(mode string) time.Duration {
	switch normalizeRemoteRefreshMode(mode) {
	case remoteRefreshModeDeltaDegraded:
		return remoteRefreshDegradedInterval
	default:
		return fullRemoteRefreshInterval
	}
}

func localRefreshIntervalForMode(mode string) time.Duration {
	switch normalizeLocalRefreshMode(mode) {
	case localRefreshModeWatchDegraded:
		return localRefreshDegradedInterval
	default:
		return localFullScanInterval
	}
}

func applyRemoteRefreshSchedule(state *ObservationState, at time.Time, mode string) {
	state.RemoteRefreshMode = normalizeRemoteRefreshMode(mode)
	state.LastFullRemoteRefreshAt = at.UnixNano()
	state.NextFullRemoteRefreshAt = at.Add(remoteRefreshIntervalForMode(state.RemoteRefreshMode)).UnixNano()
}

func applyLocalRefreshSchedule(state *ObservationState, at time.Time, mode string) {
	state.LocalRefreshMode = normalizeLocalRefreshMode(mode)
	state.LastFullLocalRefreshAt = at.UnixNano()
	state.NextFullLocalRefreshAt = at.Add(localRefreshIntervalForMode(state.LocalRefreshMode)).UnixNano()
}

func (m *SyncStore) configuredDriveIDForRead(
	ctx context.Context,
	fallback driveid.ID,
) (driveid.ID, error) {
	if cached := m.configuredDriveID(); !cached.IsZero() {
		return cached, nil
	}

	configuredDriveID, err := configuredDriveIDForDB(ctx, m.db)
	if err != nil {
		return driveid.ID{}, err
	}
	if !configuredDriveID.IsZero() {
		m.rememberConfiguredDriveID(configuredDriveID)
		return configuredDriveID, nil
	}

	if !fallback.IsZero() {
		m.rememberConfiguredDriveID(fallback)
		return fallback, nil
	}

	return driveid.ID{}, nil
}

func configuredDriveIDForDB(ctx context.Context, runner sqlTxRunner) (driveid.ID, error) {
	var raw string
	if err := runner.QueryRowContext(ctx,
		`SELECT configured_drive_id FROM observation_state WHERE singleton_id = 1`,
	).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return driveid.ID{}, nil
		}
		return driveid.ID{}, fmt.Errorf("sync: reading configured drive ID: %w", err)
	}
	if raw == "" {
		return driveid.ID{}, nil
	}

	return driveid.New(raw), nil
}

func ensureMatchingConfiguredDriveID(expected, actual driveid.ID) error {
	if expected.IsZero() || actual.IsZero() {
		return nil
	}
	if expected.Equal(actual) {
		return nil
	}

	return fmt.Errorf("sync: state DB drive mismatch: configured %s, attempted %s", actual, expected)
}

func (m *SyncStore) ReadObservationState(ctx context.Context) (*ObservationState, error) {
	if _, err := m.db.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring observation_state row: %w", err)
	}

	var (
		configuredDriveID string
		state             ObservationState
	)

	if err := m.db.QueryRowContext(ctx, sqlReadObservationState).Scan(
		&configuredDriveID,
		&state.Cursor,
		&state.RemoteRefreshMode,
		&state.LastFullRemoteRefreshAt,
		&state.NextFullRemoteRefreshAt,
		&state.LocalRefreshMode,
		&state.LastFullLocalRefreshAt,
		&state.NextFullLocalRefreshAt,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}
	state.RemoteRefreshMode = normalizeRemoteRefreshMode(state.RemoteRefreshMode)
	state.LocalRefreshMode = normalizeLocalRefreshMode(state.LocalRefreshMode)

	if configuredDriveID != "" {
		state.ConfiguredDriveID = driveid.New(configuredDriveID)
		m.rememberConfiguredDriveID(state.ConfiguredDriveID)
	}

	return &state, nil
}

func (m *SyncStore) CommitObservationCursor(
	ctx context.Context,
	driveID driveid.ID,
	cursor string,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning observation cursor transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation cursor transaction")
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}

	state.Cursor = cursor
	if writeErr := m.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing observation cursor transaction: %w", err)
	}

	return nil
}

func (m *SyncStore) ClearObservationCursor(ctx context.Context) error {
	state, err := m.ReadObservationState(ctx)
	if err != nil {
		return err
	}
	state.Cursor = ""

	if _, execErr := m.db.ExecContext(ctx, sqlUpsertObservationState,
		state.ConfiguredDriveID.String(),
		state.Cursor,
		state.RemoteRefreshMode,
		state.LastFullRemoteRefreshAt,
		state.NextFullRemoteRefreshAt,
		state.LocalRefreshMode,
		state.LastFullLocalRefreshAt,
		state.NextFullLocalRefreshAt,
	); execErr != nil {
		return fmt.Errorf("sync: clearing observation cursor: %w", execErr)
	}

	return nil
}

func (m *SyncStore) MarkFullRemoteRefresh(
	ctx context.Context,
	driveID driveid.ID,
	at time.Time,
) error {
	currentState, err := m.ReadObservationState(ctx)
	if err != nil {
		return err
	}
	currentMode := currentState.RemoteRefreshMode

	return m.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full remote refresh transaction",
		"sync: rollback full remote refresh transaction",
		"sync: committing full remote refresh transaction",
		func(state *ObservationState) {
			applyRemoteRefreshSchedule(state, at, currentMode)
		},
	)
}

func (m *SyncStore) MarkFullLocalRefresh(
	ctx context.Context,
	driveID driveid.ID,
	at time.Time,
	mode string,
) (err error) {
	return m.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full local refresh transaction",
		"sync: rollback full local refresh transaction",
		"sync: committing full local refresh transaction",
		func(state *ObservationState) {
			applyLocalRefreshSchedule(state, at, mode)
		},
	)
}

func (m *SyncStore) markObservationRefresh(
	ctx context.Context,
	driveID driveid.ID,
	beginMessage string,
	rollbackMessage string,
	commitMessage string,
	update func(*ObservationState),
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("%s: %w", beginMessage, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, rollbackMessage)
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}

	update(state)
	if writeErr := m.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("%s: %w", commitMessage, commitErr)
	}

	return nil
}

func (m *SyncStore) configuredDriveID() driveid.ID {
	m.configuredDriveMu.RLock()
	defer m.configuredDriveMu.RUnlock()

	return m.cachedConfiguredDriveID
}

func (m *SyncStore) rememberConfiguredDriveID(id driveid.ID) {
	if id.IsZero() {
		return
	}

	m.configuredDriveMu.Lock()
	defer m.configuredDriveMu.Unlock()

	m.cachedConfiguredDriveID = id
}

func (m *SyncStore) readObservationStateTx(
	ctx context.Context,
	tx sqlTxRunner,
) (*ObservationState, error) {
	if _, err := tx.ExecContext(ctx, sqlEnsureObservationStateRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring observation_state row: %w", err)
	}

	var (
		configuredDriveID string
		state             ObservationState
	)

	if err := tx.QueryRowContext(ctx, sqlReadObservationState).Scan(
		&configuredDriveID,
		&state.Cursor,
		&state.RemoteRefreshMode,
		&state.LastFullRemoteRefreshAt,
		&state.NextFullRemoteRefreshAt,
		&state.LocalRefreshMode,
		&state.LastFullLocalRefreshAt,
		&state.NextFullLocalRefreshAt,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}
	state.RemoteRefreshMode = normalizeRemoteRefreshMode(state.RemoteRefreshMode)
	state.LocalRefreshMode = normalizeLocalRefreshMode(state.LocalRefreshMode)

	if configuredDriveID != "" {
		state.ConfiguredDriveID = driveid.New(configuredDriveID)
	}

	return &state, nil
}

func (m *SyncStore) ensureConfiguredDriveIDTx(
	ctx context.Context,
	tx sqlTxRunner,
	driveID driveid.ID,
	state *ObservationState,
) error {
	if driveID.IsZero() {
		return nil
	}

	if state.ConfiguredDriveID.IsZero() {
		state.ConfiguredDriveID = driveID
		if err := m.writeObservationStateTx(ctx, tx, state); err != nil {
			return err
		}
		m.rememberConfiguredDriveID(driveID)
		return nil
	}

	if err := ensureMatchingConfiguredDriveID(driveID, state.ConfiguredDriveID); err != nil {
		return err
	}

	m.rememberConfiguredDriveID(state.ConfiguredDriveID)
	return nil
}

func (m *SyncStore) writeObservationStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	state *ObservationState,
) error {
	if _, err := tx.ExecContext(ctx, sqlUpsertObservationState,
		state.ConfiguredDriveID.String(),
		state.Cursor,
		normalizeRemoteRefreshMode(state.RemoteRefreshMode),
		state.LastFullRemoteRefreshAt,
		state.NextFullRemoteRefreshAt,
		normalizeLocalRefreshMode(state.LocalRefreshMode),
		state.LastFullLocalRefreshAt,
		state.NextFullLocalRefreshAt,
	); err != nil {
		return fmt.Errorf("sync: writing observation_state: %w", err)
	}

	if !state.ConfiguredDriveID.IsZero() {
		m.rememberConfiguredDriveID(state.ConfiguredDriveID)
	}

	return nil
}
