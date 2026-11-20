package syncstore

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The store's test seams: what the tests of the engine, the planner and the
// observers reach in a store they built. A ...ForTest function is used only by
// tests and the sync test-helper packages (devtool's test-conventions check).

// RawDBForTest is the store's database, for tests that read or seed tables
// directly.
func (s *SyncStore) RawDBForTest() *sql.DB {
	return s.rawDB()
}

// SetCrashHookForTest installs the hook the store's crash points call.
func (s *SyncStore) SetCrashHookForTest(hook func(point string) error) {
	s.crashAt = hook
}

// NowForTest is the store's clock reading.
func (s *SyncStore) NowForTest() time.Time {
	return s.nowFunc()
}

// SetLoggerForTest replaces the store's logger.
func (s *SyncStore) SetLoggerForTest(logger *slog.Logger) {
	s.logger = logger
}

// ReloadBaselineCacheForTest reloads the cached baseline from the database.
func (s *SyncStore) ReloadBaselineCacheForTest(ctx context.Context) error {
	return s.reloadBaselineCache(ctx)
}

// UpdateObservationStateForTest applies update to the stored observation
// state in one transaction.
func (s *SyncStore) UpdateObservationStateForTest(ctx context.Context, update func(*syncdomain.ObservationState)) error {
	return s.updateObservationState(ctx, update)
}

// DropBaselineCacheForTest forgets the cached baseline, so the next read
// loads it from the database.
func (s *SyncStore) DropBaselineCacheForTest() {
	s.baselineMu.Lock()
	s.baseline = nil
	s.baselineMu.Unlock()
}

// OpenSyncStoreForTest opens the store at dbPath (openSyncStore).
func OpenSyncStoreForTest(ctx context.Context, dbPath string, logger *slog.Logger, ensureSchema bool) (*SyncStore, error) {
	return openSyncStore(ctx, dbPath, logger, ensureSchema)
}

// OpenSyncStoreWithDriverForTest opens the store at dbPath through the named
// database/sql driver (openSyncStoreWithDriver).
func OpenSyncStoreWithDriverForTest(
	ctx context.Context, dbPath string, logger *slog.Logger, ensureSchema bool, driverName string,
) (*SyncStore, error) {
	return openSyncStoreWithDriver(ctx, dbPath, logger, ensureSchema, driverName)
}

// UpsertBaselineRowTxForTest writes one baseline row in tx.
func UpsertBaselineRowTxForTest(ctx context.Context, tx SqlTxRunner, entry *syncdomain.BaselineEntry) error {
	return upsertBaselineRowTx(ctx, tx, entry)
}

// ReplaceLocalStateTxForTest replaces the local_state rows in tx
// (replaceLocalStateTx).
func ReplaceLocalStateTxForTest(ctx context.Context, tx SqlTxRunner, rows []syncdomain.LocalStateRow) (bool, error) {
	return replaceLocalStateTx(ctx, tx, rows)
}

// CanonicalSchemaSQLForTest is the schema a new store is created with.
func CanonicalSchemaSQLForTest() string {
	return canonicalSchemaSQL
}

// SQLCreatePlannerVisibleLocalStateForTest creates the planner's view of
// local_state.
func SQLCreatePlannerVisibleLocalStateForTest() string {
	return sqlCreatePlannerVisibleLocalState
}

// SQLCreatePlannerVisibleRemoteStateForTest creates the planner's view of
// remote_state.
func SQLCreatePlannerVisibleRemoteStateForTest() string {
	return sqlCreatePlannerVisibleRemoteState
}
