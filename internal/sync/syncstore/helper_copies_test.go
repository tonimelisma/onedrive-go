package syncstore

// Private copies of test helpers whose home (syncstoretest) these tests
// cannot import, because it imports this package or is another package's
// tests.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// registerFaultDriver registers the fault driver under faultDriverName, once
// per test binary.
func registerFaultDriver(t *testing.T) {
	t.Helper()

	syncfixture.FaultDriverOnce.Do(func() {
		probe, err := sql.Open(SqliteDriverName, ":memory:")
		require.NoError(t, err)
		inner := probe.Driver()
		require.NoError(t, probe.Close())
		sql.Register(syncfixture.FaultDriverName, &syncfixture.FaultDriver{Inner: inner})
	})
}

// seedObservationIssueRowForTest adds one issue to the stored set and keeps
// every other. A batch replaces all stored issues of the types it manages, so
// the batch reports the stored issues of the seeded type again next to the
// new one.
func seedObservationIssueRowForTest(t *testing.T, store *SyncStore, issue *syncdomain.ObservationIssue) {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)
	batch := syncdomain.ObservationFindingsBatch{
		Issues:            []syncdomain.ObservationIssue{*issue},
		ManagedIssueTypes: []syncdomain.IssueType{issue.IssueType},
	}
	for i := range rows {
		if rows[i].IssueType != issue.IssueType || rows[i].Path == issue.Path {
			continue
		}
		batch.Issues = append(batch.Issues, syncdomain.ObservationIssue{
			Path:      rows[i].Path,
			DriveID:   rows[i].DriveID,
			IssueType: rows[i].IssueType,
			ScopeKey:  rows[i].ScopeKey,
		})
	}
	require.NoError(t, store.ReconcileObservationFindings(t.Context(), &batch))
}

func seedObservationIssueForTest(
	t *testing.T,
	store *SyncStore,
	path string,
	issueType syncdomain.IssueType,
	scopeKey syncdomain.ScopeKey,
) {
	t.Helper()

	seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      path,
		DriveID:   driveid.New(syncfixture.TestDriveID),
		IssueType: issueType,
		ScopeKey:  scopeKey,
	})
}

func observationIssuePathsForTest(t *testing.T, store *SyncStore) []string {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

func readBaselineRow(t *testing.T, mgr *SyncStore, itemID string) (path string, localSize int64, found bool) {
	t.Helper()

	err := mgr.RawDBForTest().QueryRowContext(t.Context(),
		`SELECT path, coalesce(local_size, -1) FROM baseline WHERE item_id = ?`, itemID).Scan(&path, &localSize)
	if err != nil {
		return "", 0, false
	}

	return path, localSize, true
}

func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *syncdomain.RemoteStateRow {
	t.Helper()

	var (
		rawDriveID string
		row        syncdomain.RemoteStateRow
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		`SELECT `+sqlSelectRemoteStateCols+`
		FROM remote_state WHERE item_id = ?`,
		itemID,
	).Scan(
		&rawDriveID, &row.ItemID, &row.Path, &row.ItemType,
		&hash, &size, &mtime, &etag,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err)

	row.DriveID = remoteStateDriveID(rawDriveID, driveid.ID{})
	row.Hash = hash.String
	row.ETag = etag.String
	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row
}

func twinIssuePaths(t *testing.T, mgr *SyncStore) []string {
	t.Helper()

	issues, err := mgr.ListObservationIssues(t.Context())
	require.NoError(t, err)
	var paths []string
	for i := range issues {
		if issues[i].IssueType == syncdomain.IssueRemoteNameTwin {
			paths = append(paths, issues[i].Path)
		}
	}

	return paths
}

// walDonor creates a WAL-mode database in dir whose one table lives only in
// its write-ahead log (no checkpoint has run), and returns the database's
// path. The donor stays open until the test ends, so its files can be copied
// as an interrupted writer would leave them.
func walDonor(t *testing.T, dir string) string {
	t.Helper()

	donorPath := filepath.Join(dir, "donor.db")
	donor, err := sql.Open(SqliteDriverName,
		SqliteFileURI(donorPath, url.Values{SqlitePragmaParam: {"journal_mode(WAL)", "wal_autocheckpoint(0)"}}))
	require.NoError(t, err)
	donor.SetMaxOpenConns(1)
	t.Cleanup(func() {
		assert.NoError(t, donor.Close())
	})
	_, err = donor.ExecContext(t.Context(), "CREATE TABLE "+syncfixture.StaleMarkerTable+" (x INTEGER)")
	require.NoError(t, err)

	return donorPath
}

// openPlainSQLite opens the database at dbPath as SQLite does by default,
// without the store's schema or pragmas.
func openPlainSQLite(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := sql.Open(SqliteDriverName, SqliteFileURI(dbPath, nil))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, db.Close())
	})

	return db
}

func readObservationStateForTest(
	tb testing.TB,
	store *SyncStore,
	ctx context.Context,
) syncdomain.ObservationState {
	tb.Helper()

	state, err := store.ReadObservationState(ctx)
	require.NoError(tb, err)
	require.NotNil(tb, state)
	return *state
}

func saveObservationCursorForTest(t *testing.T, mgr *SyncStore, ctx context.Context, driveID string, cursor string) {
	t.Helper()
	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(driveID), cursor))
}

func readObservationCursorForTest(t *testing.T, mgr *SyncStore, ctx context.Context, expectedDriveID string) string {
	t.Helper()

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	if expectedDriveID != "" && (!state.ContentDriveID.IsZero() || state.Cursor != "") {
		require.Equal(t, driveid.New(expectedDriveID).String(), state.ContentDriveID.String())
	}

	return state.Cursor
}

//nolint:gochecknoglobals // sync tests share one schema template cache to amortize repeated store initialization cost.
var (
	testStoreTemplateOnce stdsync.Once
	testStoreTemplateData []byte
	errTestStoreTemplate  error
)

func newTestStore(tb testing.TB) *SyncStore {
	tb.Helper()

	dbPath := filepath.Join(tb.TempDir(), "test.db")
	ctx := synctest.TestContext(tb)
	require.NoError(tb, os.WriteFile(dbPath, loadTestStoreTemplate(tb), 0o600), "WriteFile(%q)", dbPath)

	mgr, err := OpenSyncStoreForTest(ctx, dbPath, synctest.TestLogger(tb), false)
	require.NoError(tb, err, "NewSyncStore(%q)", dbPath)

	tb.Cleanup(func() {
		assert.NoError(tb, mgr.Close(context.Background()), "Close()")
	})

	return mgr
}

func loadTestStoreTemplate(tb testing.TB) []byte {
	tb.Helper()

	testStoreTemplateOnce.Do(func() {
		templateDir, err := os.MkdirTemp("", "sync-store-template-")
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template dir: %w", err)
			return
		}
		defer os.RemoveAll(templateDir)

		templatePath := filepath.Join(templateDir, "template.db")
		store, err := NewSyncStore(context.Background(), templatePath, slog.New(slog.DiscardHandler))
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("create sync store template: %w", err)
			return
		}
		if closeErr := store.Close(context.Background()); closeErr != nil {
			errTestStoreTemplate = fmt.Errorf("close sync store template: %w", closeErr)
			return
		}

		//nolint:gosec // templatePath is created by this helper inside a private temp dir.
		testStoreTemplateData, err = os.ReadFile(templatePath)
		if err != nil {
			errTestStoreTemplate = fmt.Errorf("read sync store template: %w", err)
		}
	})

	require.NoError(tb, errTestStoreTemplate)

	return append([]byte(nil), testStoreTemplateData...)
}

func requireStoreTestRawDB(t *testing.T, store *SyncStore) *sql.DB {
	t.Helper()

	db := store.RawDBForTest()
	require.NotNil(t, db)

	return db
}
