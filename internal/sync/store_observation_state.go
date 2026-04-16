package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	sqlReadObservationState = `SELECT configured_drive_id, cursor, last_full_remote_reconcile_at
		FROM observation_state WHERE singleton_id = 1`
	sqlEnsureObservationStateRow = `INSERT INTO observation_state
		(singleton_id, configured_drive_id, cursor, last_full_remote_reconcile_at)
		VALUES (1, '', '', 0)
		ON CONFLICT(singleton_id) DO NOTHING`
	sqlUpsertObservationState = `INSERT INTO observation_state
		(singleton_id, configured_drive_id, cursor, last_full_remote_reconcile_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			configured_drive_id = excluded.configured_drive_id,
			cursor = excluded.cursor,
			last_full_remote_reconcile_at = excluded.last_full_remote_reconcile_at`
)

type ObservationState struct {
	ConfiguredDriveID         driveid.ID
	Cursor                    string
	LastFullRemoteReconcileAt int64
}

func (m *SyncStore) configuredDriveIDForRead(
	ctx context.Context,
	fallback driveid.ID,
) (driveid.ID, error) {
	if !fallback.IsZero() {
		m.rememberConfiguredDriveID(fallback)
		return fallback, nil
	}

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
		&state.LastFullRemoteReconcileAt,
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

	if _, execErr := m.db.ExecContext(ctx, sqlUpsertObservationState,
		state.ConfiguredDriveID.String(),
		state.Cursor,
		state.LastFullRemoteReconcileAt,
	); execErr != nil {
		return fmt.Errorf("sync: clearing observation cursor: %w", execErr)
	}

	return nil
}

func (m *SyncStore) MarkFullRemoteReconcile(
	ctx context.Context,
	driveID driveid.ID,
	at time.Time,
) error {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning full reconcile state transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback full reconcile state transaction")
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, driveID, state); ensureErr != nil {
		return ensureErr
	}

	state.LastFullRemoteReconcileAt = at.UnixNano()
	if writeErr := m.writeObservationStateTx(ctx, tx, state); writeErr != nil {
		return writeErr
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing full reconcile state transaction: %w", err)
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
		&state.LastFullRemoteReconcileAt,
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
	if _, err := tx.ExecContext(ctx, sqlUpsertObservationState,
		state.ConfiguredDriveID.String(),
		state.Cursor,
		state.LastFullRemoteReconcileAt,
	); err != nil {
		return fmt.Errorf("sync: writing observation_state: %w", err)
	}

	if !state.ConfiguredDriveID.IsZero() {
		m.rememberConfiguredDriveID(state.ConfiguredDriveID)
	}

	return nil
}
