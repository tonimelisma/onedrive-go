package syncstore

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The store's test seams: what the tests of the engine, the planner and the
// observers reach in a store they built. A ...ForTest function is used only by
// tests and the sync test-helper packages (devtool's test-conventions check).

// Hooks are the store's test seams, passed in when the store is opened. The
// zero value is production behavior, and production code passes a Hooks
// value through without setting a field (R-6.10.29).
type Hooks struct {
	// CrashAt is called at each crash point before a commit
	// (crash_points.go): an error rolls the transaction back as a crash
	// would.
	CrashAt func(point string) error
	// Driver names the database/sql driver the store opens its database
	// through; empty is the SQLite driver. Tests name a fault-injecting
	// wrapper of it.
	Driver string
	// SkipFsync opens the store with synchronous=OFF: a commit is written
	// to the database and its write-ahead log but SQLite does not wait for
	// the disk to hold it. A test's store dies with its temp directory and
	// no test cuts the power, so the waits buy a test nothing and were most
	// of what its commits cost. Production keeps synchronous=FULL.
	SkipFsync bool
}

// driverName is the database/sql driver the store opens its database
// through.
func (h Hooks) driverName() string {
	if h.Driver == "" {
		return SqliteDriverName
	}

	return h.Driver
}

// synchronousPragma is the synchronous setting the store opens its
// connections with: FULL, which makes a commit durable before it returns,
// unless a test skips the waits for the disk.
func (h Hooks) synchronousPragma() string {
	if h.SkipFsync {
		return "synchronous(OFF)"
	}

	return "synchronous(FULL)"
}

// RawDBForTest is the store's database, for tests that read or seed tables
// directly.
func (s *SyncStore) RawDBForTest() *sql.DB {
	return s.rawDB()
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
	patch, err := s.reloadPatchTx(ctx, s.db)
	if err != nil {
		return err
	}
	s.applyBaselineCachePatch(patch)

	return nil
}

// BaselineSnapshotForTest reads the baseline table as a snapshot, the form
// planning takes it in.
func (s *SyncStore) BaselineSnapshotForTest(ctx context.Context) (*syncdomain.BaselineSnapshot, error) {
	contentDriveID, err := s.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, err
	}

	return loadBaselineWithRunner(ctx, s.db, contentDriveID)
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

// OpenSyncStoreForTest opens the store at dbPath, applying the schema only
// when ensureSchema is set (a test copies a template database in), on the
// clock now (nil is the wall clock) and with hooks.
func OpenSyncStoreForTest(
	ctx context.Context, dbPath string, logger *slog.Logger, ensureSchema bool, now func() time.Time, hooks Hooks,
) (*SyncStore, error) {
	return openSyncStoreWithHooks(ctx, dbPath, logger, ensureSchema, now, hooks)
}

// UpsertBaselineRowTxForTest writes one baseline row in tx.
func UpsertBaselineRowTxForTest(ctx context.Context, tx sqlTxRunner, entry *syncdomain.BaselineEntry) error {
	return upsertBaselineRowTx(ctx, tx, entry)
}

// ReplaceLocalStateTxForTest replaces the local_state rows in tx
// (replaceLocalStateTx).
func ReplaceLocalStateTxForTest(ctx context.Context, tx sqlTxRunner, rows []syncdomain.LocalStateRow) (bool, error) {
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
