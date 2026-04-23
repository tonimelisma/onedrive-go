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
				next_full_remote_refresh_at
			FROM observation_state
			LIMIT 1`
	sqlEnsureObservationStateRow = `INSERT INTO observation_state
		(configured_drive_id, cursor, next_full_remote_refresh_at)
		SELECT '', '', 0
		WHERE NOT EXISTS (SELECT 1 FROM observation_state)`
)

const (
	remoteRefreshEnumerateInterval = time.Hour
)

type ObservationState struct {
	ConfiguredDriveID       driveid.ID
	Cursor                  string
	NextFullRemoteRefreshAt int64
}

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

func applyRemoteRefreshSchedule(state *ObservationState, at time.Time, mode remoteObservationMode) {
	state.NextFullRemoteRefreshAt = at.Add(remoteRefreshIntervalForMode(mode)).UnixNano()
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
		`SELECT configured_drive_id FROM observation_state LIMIT 1`,
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
		&state.NextFullRemoteRefreshAt,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}

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
	return m.replaceObservationState(ctx, state)
}

func (m *SyncStore) MarkFullRemoteRefresh(
	ctx context.Context,
	driveID driveid.ID,
	at time.Time,
	mode remoteObservationMode,
) error {
	return m.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full remote refresh transaction",
		"sync: rollback full remote refresh transaction",
		"sync: committing full remote refresh transaction",
		func(state *ObservationState) {
			applyRemoteRefreshSchedule(state, at, mode)
		},
	)
}

func (m *SyncStore) ClampFullRemoteRefreshDeadline(
	ctx context.Context,
	driveID driveid.ID,
	notAfter time.Time,
) (bool, error) {
	if notAfter.IsZero() {
		return false, nil
	}

	deadline := notAfter.UnixNano()
	changed := false
	err := m.markObservationRefresh(
		ctx,
		driveID,
		"sync: beginning full remote refresh clamp transaction",
		"sync: rollback full remote refresh clamp transaction",
		"sync: committing full remote refresh clamp transaction",
		func(state *ObservationState) {
			if state.NextFullRemoteRefreshAt == 0 || state.NextFullRemoteRefreshAt > deadline {
				state.NextFullRemoteRefreshAt = deadline
				changed = true
			}
		},
	)
	return changed, err
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
		&state.NextFullRemoteRefreshAt,
	); err != nil {
		return nil, fmt.Errorf("sync: reading observation_state: %w", err)
	}

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
	if _, err := tx.ExecContext(ctx, `DELETE FROM observation_state`); err != nil {
		return fmt.Errorf("sync: clearing observation_state before write: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO observation_state
			(configured_drive_id, cursor, next_full_remote_refresh_at)
		VALUES (?, ?, ?)`,
		state.ConfiguredDriveID.String(),
		state.Cursor,
		state.NextFullRemoteRefreshAt,
	); err != nil {
		return fmt.Errorf("sync: writing observation_state: %w", err)
	}

	if !state.ConfiguredDriveID.IsZero() {
		m.rememberConfiguredDriveID(state.ConfiguredDriveID)
	}

	return nil
}

func (m *SyncStore) replaceObservationState(ctx context.Context, state *ObservationState) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin observation-state replace tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback observation-state replace tx")
	}()

	if writeErr := m.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit observation-state replace tx: %w", err)
	}

	return nil
}
