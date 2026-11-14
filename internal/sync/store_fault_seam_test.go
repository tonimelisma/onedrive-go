package sync

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A fault-injecting database/sql driver around the real SQLite driver. A
// store opened through it behaves exactly like production until a test arms
// one failure: the next statement containing a SQL fragment, or the next
// transaction commit, fails with the armed error. Every
// `fmt.Errorf("sync: …: %w")` branch of the store was otherwise unreachable,
// and the invariant that a failed write leaves the database and the
// in-memory baseline cache agreeing was unproven.

const faultDriverName = "sqlite-fault"

// faultCommit is the fragment that arms the next transaction commit.
const faultCommit = "COMMIT"

var errInjectedFault = errors.New("injected fault: disk I/O error (SQLITE_FULL)")

type sqlFault struct {
	mu    stdsync.Mutex
	match string
	// skip is how many matching statements still run before the fault fires.
	skip int
	err  error
	hits int
}

// arm makes the next statement whose SQL contains match (or the next commit,
// for faultCommit) fail with err. One shot: the fault disarms when it fires.
func (f *sqlFault) arm(match string, err error) {
	f.armAfter(match, 0, err)
}

// armAfter is arm for the statement after the next skip matching ones, so a
// test can fail a statement in the middle of a run of identical ones.
func (f *sqlFault) armAfter(match string, skip int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.skip, f.err = match, skip, err
}

func (f *sqlFault) take(query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.match == "" || !strings.Contains(query, f.match) {
		return nil
	}
	if f.skip > 0 {
		f.skip--
		return nil
	}
	err := f.err
	f.match, f.err = "", nil
	f.hits++
	return err
}

func (f *sqlFault) fired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

//nolint:gochecknoglobals // database/sql registers a driver once per process, and its connections find their fault by database path.
var (
	faultDriverOnce stdsync.Once
	faultsMu        stdsync.Mutex
	faultsByPath    = map[string]*sqlFault{}
)

func faultForPath(dbPath string) *sqlFault {
	faultsMu.Lock()
	defer faultsMu.Unlock()
	f, ok := faultsByPath[dbPath]
	if !ok {
		f = &sqlFault{}
		faultsByPath[dbPath] = f
	}
	return f
}

// faultPathFromDSN recovers the database path from the store's DSN, the
// percent-encoded `file:` URI sqliteFileURI builds, which keys the fault a
// connection consults.
func faultPathFromDSN(dsn string) (string, error) {
	uri, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("fault driver: parse DSN: %w", err)
	}
	return uri.Path, nil
}

type faultDriver struct{ inner driver.Driver }

func (d *faultDriver) Open(name string) (driver.Conn, error) {
	path, err := faultPathFromDSN(name)
	if err != nil {
		return nil, err
	}
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, fmt.Errorf("fault driver: open: %w", err)
	}
	return &faultConn{Conn: conn, fault: faultForPath(path)}, nil
}

// faultConn forwards to the real connection, consulting the fault first.
// database/sql prefers the context forms, which the SQLite driver
// implements, so these are the paths every store statement takes.
type faultConn struct {
	driver.Conn
	fault *sqlFault
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("fault driver: exec: %w", err)
	}
	return result, nil
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, fmt.Errorf("fault driver: query: %w", err)
	}
	return rows, nil
}

func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.fault.take(query); err != nil {
		return nil, err
	}
	var (
		stmt driver.Stmt
		err  error
	)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.Prepare(query)
	}
	if err != nil {
		return nil, fmt.Errorf("fault driver: prepare: %w", err)
	}
	return stmt, nil
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var (
		tx  driver.Tx
		err error
	)
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		tx, err = beginner.BeginTx(ctx, opts)
	} else {
		tx, err = c.Begin()
	}
	if err != nil {
		return nil, fmt.Errorf("fault driver: begin: %w", err)
	}
	return &faultTx{Tx: tx, fault: c.fault}, nil
}

func (c *faultConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		if err := pinger.Ping(ctx); err != nil {
			return fmt.Errorf("fault driver: ping: %w", err)
		}
	}
	return nil
}

type faultTx struct {
	driver.Tx
	fault *sqlFault
}

func (t *faultTx) Commit() error {
	if err := t.fault.take(faultCommit); err != nil {
		// The statements ran; the commit is what failed. Roll back so the
		// connection is reusable, as SQLite would after a failed commit.
		if rbErr := t.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err := t.Tx.Commit(); err != nil {
		return fmt.Errorf("fault driver: commit: %w", err)
	}
	return nil
}

// newFaultStore opens a fresh store through the fault driver and returns the
// fault that governs its connections.
func newFaultStore(t *testing.T) (*SyncStore, *sqlFault) {
	t.Helper()

	faultDriverOnce.Do(func() {
		probe, err := sql.Open(sqliteDriverName, ":memory:")
		require.NoError(t, err)
		inner := probe.Driver()
		require.NoError(t, probe.Close())
		sql.Register(faultDriverName, &faultDriver{inner: inner})
	})

	dbPath := filepath.Join(t.TempDir(), "fault.db")
	store, err := openSyncStoreWithDriver(synctest.TestContext(t), dbPath, synctest.TestLogger(t), true, faultDriverName)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.Background())) })

	return store, faultForPath(dbPath)
}

func faultSeedOutcome(path, itemID, localHash string) *BaselineMutation {
	return mutationFromActionOutcome(&actionOutcome{
		Action: ActionDownload, Success: true, Path: path,
		DriveID: driveid.New(engineTestDriveID), ItemID: itemID,
		ParentID: "root", ItemType: ItemTypeFile,
		LocalHash: localHash, RemoteHash: localHash,
		LocalSize: 100, LocalSizeKnown: true, RemoteSize: 100, RemoteSizeKnown: true,
	})
}

// Validates: R-6.5.1, R-6.5.2
//
// A statement failing inside an outcome commit, and a commit itself failing,
// each leave the database and the in-memory baseline cache agreeing, with
// the failed outcome in neither; the same outcome commits once the fault is
// gone, so nothing is lost and nothing is double-applied.
func TestFault_CommitMutationFailureLeavesCacheAndDatabaseAgreeing(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome("a.txt", "item-a", "hash-a")))

	for _, tc := range []struct {
		name  string
		match string
		path  string
		item  string
	}{
		{name: "statement", match: "INTO baseline", path: "b.txt", item: "item-b"},
		{name: "commit", match: faultCommit, path: "c.txt", item: "item-c"},
	} {
		fault.arm(tc.match, errInjectedFault)
		err := store.CommitMutation(ctx, faultSeedOutcome(tc.path, tc.item, "hash-"+tc.item))
		require.ErrorIs(t, err, errInjectedFault, tc.name)
		require.Equal(t, 1, fault.fired(), "%s: the fault fired once and disarmed", tc.name)
		fault.hits = 0

		mismatches, checkErr := store.CheckCacheConsistency(ctx)
		require.NoError(t, checkErr, tc.name)
		assert.Zero(t, mismatches, "%s: cache and database agree after the failed commit", tc.name)
		_, cached := store.baseline.GetByPath(tc.path)
		assert.False(t, cached, "%s: the failed outcome is not in the cache", tc.name)

		require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome(tc.path, tc.item, "hash-"+tc.item)), tc.name)
		entry, ok := store.baseline.GetByPath(tc.path)
		require.True(t, ok, "%s: the retried outcome lands", tc.name)
		assert.Equal(t, "hash-"+tc.item, entry.LocalHash)
		mismatches, checkErr = store.CheckCacheConsistency(ctx)
		require.NoError(t, checkErr)
		assert.Zero(t, mismatches, tc.name)
	}
}

// Validates: R-6.5.2
//
// A local-baseline refresh whose commit fails leaves the old row in both the
// database and the cache: no half-refreshed row, no cache ahead of the disk.
func TestFault_RefreshLocalBaselineCommitFailureLeavesTheOldRow(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome("a.txt", "item-a", "hash-old")))

	fault.arm(faultCommit, errInjectedFault)
	err := store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "item-a", ItemType: ItemTypeFile,
		LocalHash: "hash-new", LocalSize: 101, LocalSizeKnown: true, LocalMtime: 42,
	})
	require.ErrorIs(t, err, errInjectedFault)

	entry, ok := store.baseline.GetByPath("a.txt")
	require.True(t, ok)
	assert.Equal(t, "hash-old", entry.LocalHash, "the cache was not updated ahead of the failed commit")
	mismatches, checkErr := store.CheckCacheConsistency(ctx)
	require.NoError(t, checkErr)
	assert.Zero(t, mismatches, "the database holds the same old row")

	require.NoError(t, store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "item-a", ItemType: ItemTypeFile,
		LocalHash: "hash-new", LocalSize: 101, LocalSizeKnown: true, LocalMtime: 42,
	}))
	entry, _ = store.baseline.GetByPath("a.txt")
	assert.Equal(t, "hash-new", entry.LocalHash)
}

// Validates: R-6.5.2
//
// A failed read is returned, not answered with an empty result: a store that
// cannot read its held deletes must not report "nothing held".
func TestFault_ReadFailureIsReturnedNotEmpty(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.ReplaceHeldDeletes(ctx, []heldDelete{{Path: "x.txt", Side: deleteSafetySideRemote, Reason: deleteSafetyReasonTooMany, ItemType: ItemTypeFile}}))

	fault.arm("FROM held_deletes", errInjectedFault)
	held, err := store.listHeldDeletes(ctx)
	require.ErrorIs(t, err, errInjectedFault)
	assert.Nil(t, held)

	held, err = store.listHeldDeletes(ctx)
	require.NoError(t, err)
	assert.Len(t, held, 1, "the read works again once the fault is gone")
}

// Validates: R-2.5.2
//
// A retry-work prune whose delete fails in the middle of its run deletes
// nothing: the rows it had already deleted come back with the rollback.
func TestFault_RetryWorkPruneThatFailsMidwayDeletesNothing(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	for _, path := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow(path, "", ActionUpload, 1, 1)))
	}

	fault.armAfter(sqlDeleteRetryWorkByPathType, 1, errInjectedFault)
	err := store.PruneRetryWorkToCurrentActions(ctx, nil)
	require.ErrorIs(t, err, errInjectedFault)
	require.Equal(t, 1, fault.fired(), "the second delete failed")

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "no row is gone")

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, nil))
	rows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "the prune completes once the fault is gone")
}
