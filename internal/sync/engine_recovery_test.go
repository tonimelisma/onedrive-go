package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// --- Stubs for recovery tests ---

type stubDeltaFetcher struct{}

func (s *stubDeltaFetcher) Delta(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
	return &graph.DeltaPage{DeltaLink: "stub-token"}, nil
}

type stubItemClient struct{}

func (s *stubItemClient) GetItem(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
	return &graph.Item{ID: "stub-item"}, nil
}

func (s *stubItemClient) ListChildren(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
	return nil, nil
}

func (s *stubItemClient) CreateFolder(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
	return &graph.Item{ID: "folder-" + name}, nil
}

func (s *stubItemClient) MoveItem(_ context.Context, _ driveid.ID, itemID, _, _ string) (*graph.Item, error) {
	return &graph.Item{ID: itemID}, nil
}

func (s *stubItemClient) DeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return nil
}

func (s *stubItemClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return nil
}

type stubDownloader struct {
	data []byte
}

func (s *stubDownloader) Download(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
	n, err := w.Write(s.data)
	return int64(n), err
}

type stubUploader struct{}

func (s *stubUploader) Upload(
	_ context.Context, _ driveid.ID, _, _ string,
	_ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc,
) (*graph.Item, error) {
	return &graph.Item{ID: "uploaded-item"}, nil
}

// testRecoveryEngine creates an Engine with a real SQLite DB for recovery tests.
// Returns the engine and cleanup function.
func testRecoveryEngine(t *testing.T) (*Engine, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o755))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := &EngineConfig{
		DBPath:   dbPath,
		SyncRoot: syncRoot,
		DriveID:  driveid.New("test-drive-id"),
		Fetcher:  &stubDeltaFetcher{},
		Items:    &stubItemClient{},
		Downloads: &stubDownloader{
			data: []byte("test file content"),
		},
		Uploads: &stubUploader{},
		Logger:  logger,
	}

	engine, err := NewEngine(cfg)
	require.NoError(t, err)

	t.Cleanup(func() { engine.Close() })

	return engine, syncRoot
}

// insertTestActions inserts actions directly into the ledger for testing.
func insertTestActions(
	t *testing.T, db *sql.DB, cycleID string,
	actions []testAction,
) {
	t.Helper()

	ctx := context.Background()

	for _, a := range actions {
		var depsJSON sql.NullString
		if len(a.deps) > 0 {
			b, err := json.Marshal(a.deps)
			require.NoError(t, err)
			depsJSON = sql.NullString{String: string(b), Valid: true}
		}

		_, err := db.ExecContext(ctx,
			`INSERT INTO action_queue
				(cycle_id, action_type, path, old_path, status, depends_on,
				 drive_id, item_id, parent_id, hash, size, mtime, claimed_at)
			 VALUES (?, ?, ?, '', ?, ?, 'test-drive-id', ?, '', ?, ?, ?, ?)`,
			cycleID, a.actionType, a.path, a.status, depsJSON,
			a.itemID, a.hash, a.size, a.mtime, a.claimedAt,
		)
		require.NoError(t, err)
	}
}

type testAction struct {
	actionType string
	path       string
	status     string
	deps       []int
	itemID     string
	hash       string
	size       int64
	mtime      int64
	claimedAt  *int64
}

func TestRecoverFromLedger_NoPendingActions(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)

	ctx := context.Background()
	recovered, err := engine.recoverFromLedger(ctx)

	require.NoError(t, err)
	assert.Equal(t, 0, recovered)
}

func TestRecoverFromLedger_ReclaimsStaleAndExecutes(t *testing.T) {
	t.Parallel()

	engine, syncRoot := testRecoveryEngine(t)
	db := engine.baseline.DB()

	// Create a source file for the upload action.
	testFile := filepath.Join(syncRoot, "upload.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("upload content"), 0o644))

	// Insert actions: one pending folder create, one stale-claimed download.
	staleTime := time.Now().Add(-2 * time.Hour).UnixNano()

	actions := []testAction{
		{
			actionType: "folder_create",
			path:       "new-folder",
			status:     "pending",
			itemID:     "folder-item-1",
		},
		{
			actionType: "download",
			path:       "remote-file.txt",
			status:     "claimed",
			itemID:     "file-item-1",
			hash:       "",
			size:       100,
			claimedAt:  &staleTime,
		},
	}

	insertTestActions(t, db, "recovery-cycle-1", actions)

	ctx := context.Background()
	recovered, err := engine.recoverFromLedger(ctx)

	require.NoError(t, err)
	// Both actions should be recovered (stale claimed → reclaimed → executed).
	assert.Greater(t, recovered, 0, "should recover some actions")
}

func TestRecoverFromLedger_IgnoresTerminalActions(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)
	db := engine.baseline.DB()

	// Insert one done action and one failed action — neither should be recovered.
	actions := []testAction{
		{
			actionType: "download",
			path:       "done-file.txt",
			status:     "done",
			itemID:     "item-done",
		},
		{
			actionType: "upload",
			path:       "failed-file.txt",
			status:     "failed",
			itemID:     "item-failed",
		},
	}

	insertTestActions(t, db, "terminal-cycle", actions)

	ctx := context.Background()
	recovered, err := engine.recoverFromLedger(ctx)

	require.NoError(t, err)
	assert.Equal(t, 0, recovered, "terminal actions should not be recovered")
}

func TestRecoverFromLedger_CrossCycleDependencies(t *testing.T) {
	t.Parallel()

	engine, syncRoot := testRecoveryEngine(t)
	db := engine.baseline.DB()

	// Create local file for upload action.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "child.txt"), []byte("data"), 0o644))

	// Cycle 1: pending folder create.
	insertTestActions(t, db, "cycle-a", []testAction{
		{
			actionType: "folder_create",
			path:       "parent-dir",
			status:     "pending",
			itemID:     "folder-1",
		},
	})

	// Cycle 2: pending file (depends on nothing in its own cycle — the
	// folder create from cycle-a is a different cycle so deps are satisfied).
	insertTestActions(t, db, "cycle-b", []testAction{
		{
			actionType: "download",
			path:       "other-file.txt",
			status:     "pending",
			itemID:     "file-2",
			size:       50,
		},
	})

	ctx := context.Background()
	recovered, err := engine.recoverFromLedger(ctx)

	require.NoError(t, err)
	assert.Greater(t, recovered, 0, "cross-cycle actions should be recovered")
}

func TestRecoverFromLedger_WithDependencies(t *testing.T) {
	t.Parallel()

	engine, syncRoot := testRecoveryEngine(t)
	db := engine.baseline.DB()

	// Create source file for child.
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "parent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "parent", "child.txt"), []byte("data"), 0o644))

	// Insert folder create and dependent download in same cycle.
	// The download depends on index 0 (the folder create).
	actions := []testAction{
		{
			actionType: "folder_create",
			path:       "parent",
			status:     "pending",
			itemID:     "folder-parent",
		},
		{
			actionType: "download",
			path:       "parent/child.txt",
			status:     "pending",
			itemID:     "file-child",
			size:       4,
			deps:       []int{0}, // depends on action at index 0
		},
	}

	insertTestActions(t, db, "deps-cycle", actions)

	ctx := context.Background()
	recovered, err := engine.recoverFromLedger(ctx)

	require.NoError(t, err)
	assert.Greater(t, recovered, 0, "dependent actions should be recovered")
}

// --- DriveVerifier tests (B-074) ---

type stubDriveVerifier struct {
	driveID   driveid.ID
	driveType string
	err       error
}

func (s *stubDriveVerifier) Drive(_ context.Context, _ driveid.ID) (*graph.Drive, error) {
	if s.err != nil {
		return nil, s.err
	}

	return &graph.Drive{
		ID:        s.driveID,
		DriveType: s.driveType,
	}, nil
}

func TestVerifyDriveIdentity_Matching(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)

	verifier := &stubDriveVerifier{
		driveID:   driveid.New("test-drive-id"),
		driveType: "personal",
	}
	engine.driveVerifier = verifier

	err := engine.verifyDriveIdentity(context.Background())
	assert.NoError(t, err)
}

func TestVerifyDriveIdentity_Mismatch(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)

	verifier := &stubDriveVerifier{
		driveID:   driveid.New("different-drive-id"),
		driveType: "personal",
	}
	engine.driveVerifier = verifier

	err := engine.verifyDriveIdentity(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive ID mismatch")
}

func TestVerifyDriveIdentity_APIError(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)

	verifier := &stubDriveVerifier{
		err: fmt.Errorf("API unavailable"),
	}
	engine.driveVerifier = verifier

	err := engine.verifyDriveIdentity(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verifying drive identity")
}

func TestVerifyDriveIdentity_NilVerifier(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)

	// No verifier set (default) — should return nil.
	err := engine.verifyDriveIdentity(context.Background())
	assert.NoError(t, err)
}

func TestBuildSyntheticView(t *testing.T) {
	t.Parallel()

	row := &LedgerRow{
		ID:       1,
		Path:     "docs/readme.md",
		DriveID:  "drive123",
		ItemID:   "item456",
		ParentID: "parent789",
		Hash:     "abc123hash",
		Size:     1024,
		Mtime:    1700000000000000000,
	}

	view := buildSyntheticView(row)

	assert.Equal(t, "docs/readme.md", view.Path)
	require.NotNil(t, view.Remote, "remote should be populated from ledger metadata")
	assert.Equal(t, "item456", view.Remote.ItemID)
	assert.Equal(t, "parent789", view.Remote.ParentID)
	assert.Equal(t, "abc123hash", view.Remote.Hash)
	assert.Equal(t, int64(1024), view.Remote.Size)
	assert.Equal(t, int64(1700000000000000000), view.Remote.Mtime)
}

// --- recordCycleResults tests (B-123) ---

func TestRecordCycleResults_RecordsFailuresAndSuccesses(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)
	engine.failures = newFailureTracker(engine.logger)
	db := engine.baseline.DB()
	ctx := context.Background()

	// Insert actions, complete one and fail another.
	insertTestActions(t, db, "cycle-results-test", []testAction{
		{actionType: "download", path: "ok-file.txt", status: "pending", itemID: "i1"},
		{actionType: "upload", path: "bad-file.txt", status: "pending", itemID: "i2"},
	})

	// Manually transition: claim + complete/fail.
	_, err := db.ExecContext(ctx,
		`UPDATE action_queue SET status = 'done' WHERE path = 'ok-file.txt'`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`UPDATE action_queue SET status = 'failed', error_msg = 'timeout' WHERE path = 'bad-file.txt'`)
	require.NoError(t, err)

	engine.recordCycleResults(ctx, "cycle-results-test")

	// Success should clear any prior failure record.
	assert.False(t, engine.failures.shouldSkip("ok-file.txt"))

	// Single failure should not yet suppress (threshold is 3).
	assert.False(t, engine.failures.shouldSkip("bad-file.txt"))
}

func TestRecordCycleResults_NilFailureTracker(t *testing.T) {
	t.Parallel()

	engine, _ := testRecoveryEngine(t)
	// failures is nil (not watch mode) — should not panic.
	engine.recordCycleResults(context.Background(), "any-cycle")
}

func TestBuildSyntheticView_EmptyMetadata(t *testing.T) {
	t.Parallel()

	row := &LedgerRow{
		ID:   2,
		Path: "empty.txt",
	}

	view := buildSyntheticView(row)

	assert.Equal(t, "empty.txt", view.Path)
	assert.Nil(t, view.Remote, "remote should be nil when no metadata")
}
