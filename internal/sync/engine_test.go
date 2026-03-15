package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Composite mock implementing DeltaFetcher + ItemClient + Downloader + Uploader
//
// Engine requires all 4 interfaces (unlike Executor, which takes them
// individually), so a single mock is pragmatic here. Executor tests split
// mocks by interface because each test exercises only 1-2 interfaces.
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ DeltaFetcher        = (*engineMockClient)(nil)
	_ ItemClient          = (*engineMockClient)(nil)
	_ driveops.Downloader = (*engineMockClient)(nil)
	_ driveops.Uploader   = (*engineMockClient)(nil)
)

type engineMockClient struct {
	// DeltaFetcher
	deltaFn func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)

	// ItemClient
	getItemFn      func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn     func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn   func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *engineMockClient) Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
	if m.deltaFn != nil {
		return m.deltaFn(ctx, driveID, token)
	}

	return &graph.DeltaPage{DeltaLink: "delta-token-1"}, nil
}

func (m *engineMockClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *engineMockClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *engineMockClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return &graph.Item{ID: "new-folder-id"}, nil
}

func (m *engineMockClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *engineMockClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *engineMockClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return nil
}

func (m *engineMockClient) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	// Write some content so the file has data.
	n, err := w.Write([]byte("downloaded-content"))

	return int64(n), err
}

func (m *engineMockClient) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return &graph.Item{
		ID:           "uploaded-item-id",
		Name:         name,
		Size:         size,
		QuickXorHash: "abc123hash",
	}, nil
}

// ---------------------------------------------------------------------------
// Test helpers (shared across engine, drive_runner, and orchestrator tests)
// ---------------------------------------------------------------------------

func testCanonicalID(t *testing.T, s string) driveid.CanonicalID {
	t.Helper()

	cid, err := driveid.NewCanonicalID(s)
	require.NoError(t, err)

	return cid
}

const engineTestDriveID = "0000000000000001"

// newTestEngine creates an Engine backed by a temp dir with real SQLite
// and the given mock client. Returns the engine and sync root path.
func newTestEngine(t *testing.T, mock *engineMockClient) (*Engine, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o755), "creating sync root")

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err, "NewEngine")

	t.Cleanup(func() {
		assert.NoError(t, eng.Close(), "Engine.Close")
	})

	return eng, syncRoot
}

// setupWatchEngine initializes an engine with DepGraph + readyCh for
// processBatch tests. Returns the readyCh for reading dispatched actions.
func setupWatchEngine(t *testing.T, eng *Engine) <-chan *TrackedAction {
	t.Helper()

	eng.depGraph = NewDepGraph(eng.logger)
	eng.readyCh = make(chan *TrackedAction, 1024)

	return eng.readyCh
}

// deltaPageWithItems returns a DeltaPage with the given items and a delta link.
func deltaPageWithItems(items []graph.Item, deltaLink string) *graph.DeltaPage {
	return &graph.DeltaPage{
		Items:     items,
		DeltaLink: deltaLink,
	}
}

// writeLocalFile creates a file in syncRoot for local observer to find.
func writeLocalFile(t *testing.T, syncRoot, relPath, content string) {
	t.Helper()

	absPath := filepath.Join(syncRoot, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755), "MkdirAll")
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o644), "WriteFile")
}

// seedBaseline commits outcomes and an optional delta token to the baseline,
// using the per-outcome CommitOutcome API (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []Outcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		require.NoError(t, mgr.CommitOutcome(ctx, &outcomes[i]), "seed CommitOutcome[%d]", i)
	}

	if deltaToken != "" {
		require.NoError(t, mgr.CommitDeltaToken(ctx, deltaToken, engineTestDriveID, "", engineTestDriveID), "seed CommitDeltaToken")
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewEngine_ZeroDriveID_ReturnsError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o755))

	mock := &engineMockClient{}
	logger := testLogger(t)

	_, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DriveID:   driveid.ID{}, // zero — should be rejected
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-zero drive ID")
}

func TestRunOnce_NoChanges(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			// Return root only — no content changes.
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")

	assert.Equal(t, SyncBidirectional, report.Mode)

	total := report.Downloads + report.Uploads + report.LocalDeletes +
		report.RemoteDeletes + report.FolderCreates + report.Moves +
		report.Conflicts + report.SyncedUpdates + report.Cleanups
	assert.Equal(t, 0, total, "expected zero actions")
	assert.Equal(t, 0, report.Succeeded, "succeeded")
	assert.Equal(t, 0, report.Failed, "failed")
}

// Validates: R-2.1.3
func TestRunOnce_DownloadOnly_SkipsLocalScan(t *testing.T) {
	t.Parallel()

	// Place a local file that would generate an upload event if scanned.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local-only.txt", "should not be uploaded")

	ctx := t.Context()

	report, err := eng.RunOnce(ctx, SyncDownloadOnly, RunOpts{})
	require.NoError(t, err, "RunOnce")

	// The local file should not appear in uploads because local scan was skipped.
	assert.Equal(t, 0, report.Uploads, "local scan should be skipped in download-only mode")
}

// Validates: R-2.1.4
func TestRunOnce_UploadOnly_SkipsDelta(t *testing.T) {
	t.Parallel()

	deltaCalled := false
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalled = true
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	_, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{})
	require.NoError(t, err, "RunOnce")
	assert.False(t, deltaCalled, "Delta should not be called in upload-only mode")
}

// Validates: R-2.1.1
func TestRunOnce_Bidirectional_FullRun(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "remote-file-1", Name: "remote.txt", ParentID: "root",
					DriveID: driveID, Size: 42, QuickXorHash: "remotehash1",
				},
			}, "token-after"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote-content"))
			return int64(n), err
		},
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID: "uploaded-id", Name: name, Size: 13, QuickXorHash: "localhash1",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create a local-only file.
	writeLocalFile(t, syncRoot, "local.txt", "local-content")

	ctx := t.Context()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Expect at least one download (remote.txt) and one upload (local.txt).
	assert.GreaterOrEqual(t, report.Downloads, 1, "downloads")
	assert.GreaterOrEqual(t, report.Uploads, 1, "uploads")
	assert.Equal(t, 0, report.Failed, "failed; errors: %v", report.Errors)

	// Verify baseline was updated: reload and check entries exist.
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load baseline")

	_, ok := bl.GetByPath("remote.txt")
	assert.True(t, ok, "remote.txt not in baseline after sync")

	_, ok = bl.GetByPath("local.txt")
	assert.True(t, ok, "local.txt not in baseline after sync")
}

// Validates: R-2.1.5
func TestRunOnce_DryRun_NoExecution(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	executorCalled := false

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "newfile.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash1",
				},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			executorCalled = true
			return 0, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{DryRun: true})
	require.NoError(t, err, "RunOnce")

	assert.True(t, report.DryRun, "report.DryRun")
	assert.GreaterOrEqual(t, report.Downloads, 1, "plan should be computed")
	assert.False(t, executorCalled, "executor should not be called during dry-run")
	assert.Equal(t, 0, report.Succeeded, "succeeded")
	assert.Equal(t, 0, report.Failed, "failed")

	// Verify baseline is unchanged (no commit in dry-run).
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load baseline")
	assert.Equal(t, 0, bl.Len(), "dry-run should not commit")

	// Verify delta token is not saved (dry-run must not advance the token).
	savedToken, err := eng.baseline.GetDeltaToken(ctx, eng.driveID.String(), "")
	require.NoError(t, err, "GetDeltaToken")
	assert.Empty(t, savedToken, "dry-run should not save delta token")
}

func TestRunOnce_BigDelete_WithoutForce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Upload-only mode with no local files → local observer sees all baseline
	// entries as deleted → EF6 → ActionRemoteDelete. With threshold=10,
	// 20 remote deletes > 10 → ErrBigDeleteTriggered.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.bigDeleteThreshold = 10 // low threshold for test
	ctx := t.Context()

	seedOutcomes := make([]Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	_, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{})
	assert.ErrorIs(t, err, ErrBigDeleteTriggered)
}

func TestRunOnce_BigDelete_WithForce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Same scenario as WithoutForce: upload-only, no local files, 20 baseline
	// entries → 20 RemoteDeletes. Force bypasses the safety threshold.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.bigDeleteThreshold = 10 // low threshold for test
	ctx := t.Context()

	seedOutcomes := make([]Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	report, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{Force: true})
	require.NoError(t, err, "RunOnce with force")
	assert.GreaterOrEqual(t, report.RemoteDeletes, 1, "force should bypass big-delete")
}

func TestRunOnce_ExecutorPartialFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "good.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash1",
				},
				{
					ID: "f2", Name: "bad.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash2",
				},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			if itemID == "f2" {
				// Use 403 (non-retryable) to avoid retry delays in tests.
				return 0, &graph.GraphError{StatusCode: 403, Message: "forbidden"}
			}

			n, err := w.Write([]byte("good"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	// DAG executor handles individual failures gracefully — RunOnce succeeds
	// but reports the failure in Stats.
	require.NoError(t, err, "RunOnce")

	// At least 1 succeeded and at least 1 failed.
	assert.GreaterOrEqual(t, report.Succeeded, 1, "succeeded")
	assert.GreaterOrEqual(t, report.Failed, 1, "failed")

	// Verify the successful file is in baseline.
	bl, loadErr := eng.baseline.Load(ctx)
	require.NoError(t, loadErr, "Load")

	_, ok := bl.GetByPath("good.txt")
	assert.True(t, ok, "good.txt not in baseline after partial commit")
}

func TestRunOnce_ContextCancellation(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, context.Canceled
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.Error(t, err, "expected error from canceled context")
}

func TestRunOnce_DeltaTokenPersisted(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "file.txt", ParentID: "root",
					DriveID: driveID, Size: 5, QuickXorHash: "hash1",
				},
			}, "new-delta-token"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Verify delta token was saved.
	token, err := eng.baseline.GetDeltaToken(ctx, engineTestDriveID, "")
	require.NoError(t, err, "GetDeltaToken")
	assert.Equal(t, "new-delta-token", token)
}

func TestRunOnce_BaselineUpdatedAfterRun(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "item-a", Name: "alpha.txt", ParentID: "root",
					DriveID: driveID, Size: 7, QuickXorHash: "alphahash",
				},
			}, "token-v2"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("alpha!!"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Reload and verify.
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	entry, ok := bl.GetByPath("alpha.txt")
	require.True(t, ok, "alpha.txt not in baseline")
	assert.Equal(t, "item-a", entry.ItemID)
	assert.Equal(t, driveID, entry.DriveID)
}

func TestNewEngine_InvalidDBPath(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)

	_, err := NewEngine(&EngineConfig{
		DBPath:    "/nonexistent/deeply/nested/path/test.db",
		SyncRoot:  t.TempDir(),
		DriveID:   driveid.New(engineTestDriveID),
		Fetcher:   &engineMockClient{},
		Items:     &engineMockClient{},
		Downloads: &engineMockClient{},
		Uploads:   &engineMockClient{},
		Logger:    logger,
	})

	require.Error(t, err, "expected error for invalid DB path")
}

func TestRunOnce_DeltaExpired_AutoRetry(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	callCount := 0

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			callCount++
			// First call (with saved token) returns expired.
			if callCount == 1 {
				return nil, graph.ErrGone
			}

			// Second call (empty token) succeeds.
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "fresh-token"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a stale delta token.
	seedOutcomes := []Outcome{{
		Action:  ActionDownload,
		Success: true,
		Path:    "seed.txt",
		DriveID: driveID,
		ItemID:  "seed-1",
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "stale-token")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Delta should have been called twice (expired + retry).
	assert.Equal(t, 2, callCount, "delta call count")

	// Report should reflect no content changes (only root in delta).
	total := report.Downloads + report.Uploads
	assert.Equal(t, 0, total, "downloads+uploads")
}

// TestRunOnce_EmptyPlan_NoPanic verifies that when changes exist but all
// classify to no-op actions (producing an empty plan), the engine does not
// deadlock. Regression test for: empty plan caused NewDepGraph with total=0,
// Done() channel never closed, pool.Wait() blocked forever.
func TestRunOnce_EmptyPlan_NoPanic(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Seed a baseline entry that matches the delta response exactly.
	// The planner will see no diff → all changes classify to EF1/ED1 (no-op)
	// → empty action plan.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "unchanged.txt", ParentID: "root",
					DriveID: driveID, Size: 5, QuickXorHash: "matchhash",
				},
			}, "token-empty"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed baseline so the file appears as already synced with matching hash.
	seedOutcomes := []Outcome{{
		Action:     ActionDownload,
		Success:    true,
		Path:       "unchanged.txt",
		DriveID:    driveID,
		ItemID:     "f1",
		ItemType:   ItemTypeFile,
		RemoteHash: "matchhash",
		LocalHash:  "matchhash",
		Size:       5,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	// Write a matching local file so the local observer also sees no change.
	writeLocalFile(t, syncRoot, "unchanged.txt", "hello")

	// This should complete without deadlock — use a timeout to detect hangs.
	done := make(chan struct{})
	var report *SyncReport
	var runErr error

	go func() {
		report, runErr = eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
		close(done)
	}()

	select {
	case <-done:
		// Good — completed.
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunOnce deadlocked on empty action plan")
	}

	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 0, report.Failed, "failed")
}

// TestRunOnce_DeltaTokenCommittedWithObservations verifies that the delta token
// is committed atomically with observations in CommitObservation, even when
// subsequent actions fail. Failed items are tracked in remote_state for retry
// rather than relying on delta token rollback.
func TestRunOnce_DeltaTokenCommittedWithObservations(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "will-fail.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash1",
				},
			}, "new-token-after-observation"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, fmt.Errorf("simulated network error")
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a known delta token.
	seedBaseline(t, eng.baseline, ctx, nil, "old-token")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err, "RunOnce")
	require.GreaterOrEqual(t, report.Failed, 1, "should have failures")

	// Delta token IS advanced — committed atomically with observations.
	// Failed items are tracked in remote_state, not by rolling back the token.
	token, tokenErr := eng.baseline.GetDeltaToken(ctx, engineTestDriveID, "")
	require.NoError(t, tokenErr, "GetDeltaToken")
	assert.Equal(t, "new-token-after-observation", token,
		"delta token should advance with observations even when actions fail")
}

// Validates: R-6.5.3, R-2.5.3
// TestRunOnce_CrashRecovery_ResetsInProgressStates verifies that RunOnce
// resets downloading/deleting states to their pending equivalents at startup,
// ensuring crash recovery picks up interrupted work.
func TestRunOnce_CrashRecovery_ResetsInProgressStates(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Simulate a crash by inserting rows with in-progress states directly.
	now := time.Now().Unix()
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, 'item-dl', '/downloading.txt', 'file', 'downloading', ?),
		       (?, 'item-del', '/deleting.txt', 'file', 'deleting', ?)`,
		engineTestDriveID, now, engineTestDriveID, now)
	require.NoError(t, err, "seed in-progress rows")

	// RunOnce should reset these at startup.
	_, runErr := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, runErr, "RunOnce")

	// Verify the states were reset.
	var dlStatus, delStatus string
	err = eng.baseline.rawDB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-dl'`).Scan(&dlStatus)
	require.NoError(t, err)
	assert.Equal(t, "pending_download", dlStatus, "downloading should be reset")

	// deleting → deleted because the file doesn't exist on disk (crash
	// recovery checks filesystem to determine target state).
	err = eng.baseline.rawDB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-del'`).Scan(&delStatus)
	require.NoError(t, err)
	assert.Equal(t, "deleted", delStatus, "deleting with no local file should be marked deleted")
}

func TestResolveSafetyConfig_Default(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: defaultBigDeleteThreshold}
	cfg := eng.resolveSafetyConfig(RunOpts{})

	assert.Equal(t, defaultBigDeleteThreshold, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_Force(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: defaultBigDeleteThreshold}
	cfg := eng.resolveSafetyConfig(RunOpts{Force: true})

	assert.Equal(t, forceSafetyMax, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_UsesConfiguredThreshold(t *testing.T) {
	t.Parallel()

	// Verify the config bug is fixed: engine uses the configured threshold,
	// not a hardcoded default.
	eng := &Engine{bigDeleteThreshold: 500}
	cfg := eng.resolveSafetyConfig(RunOpts{})

	assert.Equal(t, 500, cfg.BigDeleteThreshold)
}

// ---------------------------------------------------------------------------
// Conflict resolution tests
// ---------------------------------------------------------------------------

// Validates: R-2.3.4
func TestResolveConflict_KeepBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "conflict-file.txt",
		DriveID:      driveID,
		ItemID:       "item-c",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Get conflict ID.
	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	// Resolve as keep_both.
	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepBoth), "ResolveConflict")

	// Verify it's no longer unresolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
}

func TestResolveConflict_NotFound(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	err := eng.ResolveConflict(ctx, "nonexistent-id", ResolutionKeepBoth)
	require.Error(t, err, "expected error for nonexistent conflict")
}

func TestResolveConflict_UnknownStrategy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "bad-strategy.txt",
		DriveID:      driveID,
		ItemID:       "item-x",
		ItemType:     ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")

	err = eng.ResolveConflict(ctx, conflicts[0].ID, "invalid_strategy")
	require.Error(t, err, "expected error for unknown strategy")
}

func TestListConflicts_Engine(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Empty initially.
	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	assert.Empty(t, conflicts)
}

// Validates: R-2.3.4
func TestResolveConflict_KeepLocal(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	uploadCalled := false

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:           "uploaded-resolve",
				Name:         name,
				ETag:         "etag-resolved",
				QuickXorHash: "resolve-hash",
				Size:         5,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "keep-local.txt",
		DriveID:      driveID,
		ItemID:       "item-kl",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that will be uploaded.
	writeLocalFile(t, syncRoot, "keep-local.txt", "local")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal), "ResolveConflict")
	assert.True(t, uploadCalled, "expected Upload to be called for keep_local resolution")

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
}

// Validates: R-2.3.4
func TestResolveConflict_KeepRemote(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	downloadContent := "remote-version"

	mock := &engineMockClient{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, writeErr := w.Write([]byte(downloadContent))
			return int64(n), writeErr
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "keep-remote.txt",
		DriveID:      driveID,
		ItemID:       "item-kr",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepRemote), "ResolveConflict")

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")

	// Verify the local file has remote content.
	data, readErr := os.ReadFile(filepath.Join(syncRoot, "keep-remote.txt"))
	require.NoError(t, readErr, "reading resolved file")
	assert.Equal(t, downloadContent, string(data))
}

// ---------------------------------------------------------------------------
// RunWatch tests
// ---------------------------------------------------------------------------

// Validates: R-2.8
// TestRunWatch_ContextCancel verifies that canceling the context causes
// RunWatch to return nil (clean shutdown).
func TestRunWatch_ContextCancel(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncBidirectional, WatchOpts{
			// Use very long intervals so observers don't fire during test.
			PollInterval: 1 * time.Hour,
			Debounce:     1 * time.Hour,
		})
	}()

	// Give RunWatch time to start (initial sync + observers).
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout after context cancel")
	}
}

// TestRunWatch_UploadOnly_SkipsRemoteObserver verifies that upload-only mode
// does not start a remote observer (no delta polling).
func TestRunWatch_UploadOnly_SkipsRemoteObserver(t *testing.T) {
	t.Parallel()

	deltaCalledAfterInit := 0
	initDone := make(chan struct{})

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			select {
			case <-initDone:
				// After initial sync, any delta call means remote observer started.
				deltaCalledAfterInit++
			default:
			}
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Mark init as done just before RunWatch's observer phase.
		// Since RunWatch calls RunOnce first, the delta call during init is expected.
		close(initDone)
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOpts{
			PollInterval: 50 * time.Millisecond,
			Debounce:     10 * time.Millisecond,
		})
	}()

	// Wait for the watch loop to be running.
	time.Sleep(300 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}

	// In upload-only mode, no delta calls should happen after initial sync.
	assert.Equal(t, 0, deltaCalledAfterInit, "delta should not be called after init in upload-only mode")

	// remoteObs should not be set in upload-only mode.
	assert.Nil(t, eng.remoteObs, "remoteObs should be nil in upload-only mode")
}

// TestRunWatch_ProcessBatch_BigDelete verifies that the rolling delete
// counter in watch mode holds delete actions when the threshold is exceeded,
// records them as actionable issues, and prevents dispatch.
func TestRunWatch_ProcessBatch_BigDelete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a large baseline so that a batch of deletes triggers big-delete.
	seedOutcomes := make([]Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build a batch that would delete all 20 files.
	var batch []PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, PathChanges{
			Path: o.Path,
			RemoteEvents: []ChangeEvent{{
				Source:    SourceRemote,
				Type:      ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	ready := setupWatchEngine(t, eng)

	// Install a rolling delete counter with threshold=10 on the engine.
	// The planner-level check is disabled (forceSafetyMax) — the counter
	// handles protection in watch mode.
	eng.deleteCounter = newDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &SafetyConfig{BigDeleteThreshold: forceSafetyMax}

	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety)

	// Verify no actions were dispatched (all 20 are deletes and counter tripped).
	select {
	case ta := <-ready:
		assert.Fail(t, "unexpected action dispatched", "path: %s", ta.Action.Path)
	default:
		// Good — no actions.
	}

	// Verify counter is now held.
	assert.True(t, eng.deleteCounter.IsHeld(), "counter should be held")

	// Verify held deletes were recorded as actionable issues.
	rows, listErr := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueBigDeleteHeld)
	require.NoError(t, listErr, "ListSyncFailuresByIssueType")
	assert.Equal(t, 20, len(rows), "should have 20 big_delete_held entries")
}

// TestRunWatch_ProcessBatch_BigDelete_NonDeletesFlow verifies that non-delete
// actions are dispatched even when the delete counter is held.
func TestRunWatch_ProcessBatch_BigDelete_NonDeletesFlow(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed baseline with files that will be "deleted" plus one path that
	// will produce a download (new remote file).
	seedOutcomes := make([]Outcome, 15)
	for i := range 15 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build batch: 15 deletes + 1 new remote file (download).
	var batch []PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, PathChanges{
			Path: o.Path,
			RemoteEvents: []ChangeEvent{{
				Source:    SourceRemote,
				Type:      ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	// Add a new remote file that should produce a download.
	batch = append(batch, PathChanges{
		Path: "newfile.txt",
		RemoteEvents: []ChangeEvent{{
			Source:   SourceRemote,
			Type:     ChangeCreate,
			Path:     "newfile.txt",
			ItemID:   "item-new",
			DriveID:  driveID,
			Hash:     "newhash",
			Size:     50,
			ItemType: ItemTypeFile,
		}},
	})

	ready := setupWatchEngine(t, eng)

	// Install counter with threshold=10. 15 deletes > 10 → trips.
	eng.deleteCounter = newDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &SafetyConfig{BigDeleteThreshold: forceSafetyMax}

	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety)

	// Counter should be held.
	assert.True(t, eng.deleteCounter.IsHeld(), "counter should be held")

	// One download action should have been dispatched.
	dispatched := 0
	for range 5 {
		select {
		case <-ready:
			dispatched++
		default:
		}
	}

	assert.Equal(t, 1, dispatched, "one non-delete action should be dispatched")

	// 15 held delete entries should exist.
	rows, listErr := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueBigDeleteHeld)
	require.NoError(t, listErr, "ListSyncFailuresByIssueType")
	assert.Equal(t, 15, len(rows), "should have 15 big_delete_held entries")
}

// TestRunWatch_ProcessBatch_BigDelete_BelowThreshold verifies that the
// rolling counter allows deletes through when below the threshold.
func TestRunWatch_ProcessBatch_BigDelete_BelowThreshold(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed baseline with 5 files.
	seedOutcomes := make([]Outcome, 5)
	for i := range 5 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build batch: 5 deletes — below threshold of 10.
	var batch []PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, PathChanges{
			Path: o.Path,
			RemoteEvents: []ChangeEvent{{
				Source:    SourceRemote,
				Type:      ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	ready := setupWatchEngine(t, eng)

	eng.deleteCounter = newDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &SafetyConfig{BigDeleteThreshold: forceSafetyMax}

	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety)

	// Counter should NOT be held.
	assert.False(t, eng.deleteCounter.IsHeld(), "counter should not trip at 5 < 10")

	// All 5 deletes should have been dispatched.
	dispatched := 0
	for range 10 {
		select {
		case <-ready:
			dispatched++
		default:
		}
	}

	assert.Equal(t, 5, dispatched, "all 5 delete actions should be dispatched")
}

// TestEngine_ExternalDBChanged verifies the PRAGMA data_version detection.
func TestEngine_ExternalDBChanged(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed the initial data_version.
	dv, err := eng.baseline.DataVersion(ctx)
	require.NoError(t, err)
	eng.lastDataVersion = dv

	// No external changes yet — should return false.
	assert.False(t, eng.externalDBChanged(ctx), "no external changes")

	// Engine's own writes don't change data_version, so repeated checks
	// should still return false.
	assert.False(t, eng.externalDBChanged(ctx), "still no external changes")
}

// TestEngine_HandleExternalChanges_BigDeleteClearance verifies that
// handleExternalChanges releases the delete counter when all
// big_delete_held entries have been cleared.
func TestEngine_HandleExternalChanges_BigDeleteClearance(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Install a held delete counter.
	eng.deleteCounter = newDeleteCounter(10, 5*time.Minute, time.Now)
	eng.deleteCounter.Add(15) // trips the counter
	require.True(t, eng.deleteCounter.IsHeld())

	// Record some big_delete_held issues.
	failures := []ActionableFailure{
		{Path: "file1.txt", DriveID: driveID, Direction: "delete", IssueType: IssueBigDeleteHeld, Error: "held"},
		{Path: "file2.txt", DriveID: driveID, Direction: "delete", IssueType: IssueBigDeleteHeld, Error: "held"},
	}
	require.NoError(t, eng.baseline.UpsertActionableFailures(ctx, failures))

	// handleExternalChanges should NOT release — rows still present.
	eng.handleExternalChanges(ctx)
	assert.True(t, eng.deleteCounter.IsHeld(), "should still be held with entries present")

	// Clear all big_delete_held entries (simulates `issues clear --all`).
	require.NoError(t, eng.baseline.ClearResolvedActionableFailures(ctx, IssueBigDeleteHeld, nil))

	// Now handleExternalChanges should release.
	eng.handleExternalChanges(ctx)
	assert.False(t, eng.deleteCounter.IsHeld(), "should be released after entries cleared")
}

// TestEngine_HandleExternalChanges_PartialClear verifies that the counter
// stays held when only some big_delete_held entries are cleared.
func TestEngine_HandleExternalChanges_PartialClear(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	eng.deleteCounter = newDeleteCounter(10, 5*time.Minute, time.Now)
	eng.deleteCounter.Add(15)
	require.True(t, eng.deleteCounter.IsHeld())

	// Record two big_delete_held entries.
	failures := []ActionableFailure{
		{Path: "file1.txt", DriveID: driveID, Direction: "delete", IssueType: IssueBigDeleteHeld, Error: "held"},
		{Path: "file2.txt", DriveID: driveID, Direction: "delete", IssueType: IssueBigDeleteHeld, Error: "held"},
	}
	require.NoError(t, eng.baseline.UpsertActionableFailures(ctx, failures))

	// Clear only file1.txt — one entry remains (file2.txt is the "current" path).
	require.NoError(t, eng.baseline.ClearResolvedActionableFailures(ctx, IssueBigDeleteHeld, []string{"file2.txt"}))

	eng.handleExternalChanges(ctx)
	assert.True(t, eng.deleteCounter.IsHeld(), "should remain held with one entry still present")
}

// TestRunWatch_ProcessBatch_EmptyPlan verifies that an empty plan (all
// changes classify to no-op) is handled gracefully.
func TestRunWatch_ProcessBatch_EmptyPlan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed baseline with a synced file.
	seedOutcomes := []Outcome{{
		Action:     ActionDownload,
		Success:    true,
		Path:       "already-synced.txt",
		DriveID:    driveID,
		ItemID:     "item-as",
		ItemType:   ItemTypeFile,
		RemoteHash: "samehash",
		LocalHash:  "samehash",
		Size:       5,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// A "change" that matches baseline exactly → planner produces empty plan.
	batch := []PathChanges{{
		Path: "already-synced.txt",
		RemoteEvents: []ChangeEvent{{
			Source:  SourceRemote,
			Type:    ChangeModify,
			Path:    "already-synced.txt",
			ItemID:  "item-as",
			DriveID: driveID,
			Hash:    "samehash",
			Size:    5,
		}},
	}}

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	// Should return without error or dispatching actions.
	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety)
}

// TestRunWatch_Deduplication verifies that processBatch cancels in-flight
// actions for paths that appear in a new batch (B-122).
// Validates: R-2.8
func TestRunWatch_Deduplication(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	// First batch: download a file.
	batch1 := []PathChanges{{
		Path: "overlapping.txt",
		RemoteEvents: []ChangeEvent{{
			Source:  SourceRemote,
			Type:    ChangeCreate,
			Path:    "overlapping.txt",
			DriveID: driveID,
			ItemID:  "item-1",
			Hash:    "hash-v1",
			Size:    10,
		}},
	}}

	eng.processBatch(ctx, batch1, bl, SyncBidirectional, safety)

	// Verify the action is in-flight.
	require.True(t, eng.depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after first batch")

	// Second batch: same path, different content. Should cancel the first.
	batch2 := []PathChanges{{
		Path: "overlapping.txt",
		RemoteEvents: []ChangeEvent{{
			Source:  SourceRemote,
			Type:    ChangeModify,
			Path:    "overlapping.txt",
			DriveID: driveID,
			ItemID:  "item-1",
			Hash:    "hash-v2",
			Size:    20,
		}},
	}}

	eng.processBatch(ctx, batch2, bl, SyncBidirectional, safety)

	// The second batch should have replaced the first.
	// We can't easily verify cancellation directly, but we can verify
	// the path is still tracked (new action replaced old one).
	assert.True(t, eng.depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after second batch")
}

// TestRunWatch_DownloadOnly_SkipsLocalObserver verifies that download-only mode
// does not start a local observer (no fsnotify watcher, no local change detection).
func TestRunWatch_DownloadOnly_SkipsLocalObserver(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOpts{
			PollInterval: 1 * time.Hour,
			Debounce:     10 * time.Millisecond,
		})
	}()

	// Wait for watch loop to start.
	time.Sleep(300 * time.Millisecond)

	// Create a local file. If a local observer were running, it would detect
	// this and eventually produce a sync action. In download-only mode, the
	// local observer is skipped, so this file should be invisible to sync.
	writeLocalFile(t, syncRoot, "local-only.txt", "should-be-ignored")

	// Give time for any observer to fire (if incorrectly started).
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}

	// In download-only mode, the remote observer should be set.
	assert.NotNil(t, eng.remoteObs, "remoteObs should be set in download-only mode")
}

// TestRunWatch_AllObserversDead_ReturnsError verifies that RunWatch returns an
// error (not nil) when all observers exit. Uses upload-only mode with a .nosync
// guard file so the local observer fails immediately.
func TestRunWatch_AllObserversDead_ReturnsError(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create .nosync guard file so local observer exits immediately with error.
	writeLocalFile(t, syncRoot, ".nosync", "")

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(t.Context(), SyncUploadOnly, WatchOpts{
			PollInterval: 1 * time.Hour,
			Debounce:     10 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err, "RunWatch returned nil, want error indicating all observers exited")

		if !errors.Is(err, ErrNosyncGuard) {
			// Should be the "all observers exited" wrapper, but the observer error
			// should be logged as a warning. Check it's not a random error.
			assert.Equal(t, "sync: all observers exited", err.Error())
		}
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout (should exit when all observers die)")
	}
}

// TestRunWatch_WatchLimitExhausted_FallsBackToPolling verifies that when the
// local observer returns ErrWatchLimitExhausted, the engine does NOT consider
// the observer dead. Instead it falls back to periodic full scanning and
// RunWatch continues until the context is canceled.
func TestRunWatch_WatchLimitExhausted_FallsBackToPolling(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create a subdirectory so the ENOSPC watcher has something to fail on.
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o755))

	// Inject a watcher factory that returns ENOSPC after the first Add (root).
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return newEnospcWatcher(1), nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOpts{
			PollInterval: 100 * time.Millisecond, // short for fast test
			Debounce:     10 * time.Millisecond,
		})
	}()

	// Wait long enough for the fallback to trigger at least one full scan.
	time.Sleep(500 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		// RunWatch should return nil (clean shutdown), NOT an "all observers exited" error.
		assert.NoError(t, err, "RunWatch should return nil on clean shutdown with fallback polling")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}
}

func TestResolveConflict_KeepLocal_TransferFails(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, fmt.Errorf("upload failed: network error")
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "fail-upload.txt",
		DriveID:      driveID,
		ItemID:       "item-fu",
		ItemType:     ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that would be uploaded.
	writeLocalFile(t, syncRoot, "fail-upload.txt", "local-data")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	resolveErr := eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal)
	require.Error(t, resolveErr, "expected error from failed upload")

	// Conflict should remain unresolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after failed resolve")
	assert.Len(t, remaining, 1, "expected 1 unresolved conflict")
}

// ---------------------------------------------------------------------------
// Regression: B-091 — resolveTransfer success path commits to baseline
// ---------------------------------------------------------------------------

// TestResolveConflict_KeepLocal_CommitsToBaseline verifies that after a
// successful keep_local resolution (upload), the baseline contains an updated
// entry with the new ItemID and hash from the upload response.
func TestResolveConflict_KeepLocal_CommitsToBaseline(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID:   "resolved-item-id",
				Name: name,
				ETag: "etag-resolved",
				// Empty hash = skip server-side verification (consistent with B-153).
				QuickXorHash: "",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "baseline-commit.txt",
		DriveID:      driveID,
		ItemID:       "original-item-id",
		ItemType:     ItemTypeFile,
		LocalHash:    "old-local-h",
		RemoteHash:   "old-remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that will be uploaded.
	writeLocalFile(t, syncRoot, "baseline-commit.txt", "resolved local")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal), "ResolveConflict")

	// Verify the baseline was updated with the new item from the upload.
	bl, loadErr := eng.baseline.Load(ctx)
	require.NoError(t, loadErr, "baseline.Load")

	entry, ok := bl.GetByPath("baseline-commit.txt")
	require.True(t, ok, "baseline entry not found after resolve")

	assert.Equal(t, "resolved-item-id", entry.ItemID)
	assert.Equal(t, "etag-resolved", entry.ETag)
	assert.NotEmpty(t, entry.LocalHash, "baseline LocalHash should be set (computed from local file)")

	// RemoteHash comes from the upload response's QuickXorHash, which is empty
	// in this mock (skip-verification pattern), so it should be empty.
	assert.Empty(t, entry.RemoteHash, "mock returns no hash")

	// "resolved local" is 14 bytes.
	assert.Equal(t, int64(14), entry.Size)
}

// ---------------------------------------------------------------------------
// Regression: B-077 — resolveTransfer with minimal conflict record (no panic)
// ---------------------------------------------------------------------------

// TestResolveConflict_KeepLocal_MinimalRecord_NoPanic verifies that calling
// ResolveConflict with a sparse ConflictRecord (only mandatory fields) does
// not cause a nil-pointer panic. The original bug was a nil-map panic when
// called without prior Execute().
func TestResolveConflict_KeepLocal_MinimalRecord_NoPanic(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID:   "minimal-resolved",
				Name: name,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict with only the mandatory fields — no hashes, no etag.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "minimal-conflict.txt",
		DriveID:      driveID,
		ItemID:       "item-min",
		ItemType:     ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file.
	writeLocalFile(t, syncRoot, "minimal-conflict.txt", "minimal data")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	// This must not panic. The original bug was a nil-map access in resolveTransfer.
	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal), "ResolveConflict")

	// Verify the conflict is resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
}

// ---------------------------------------------------------------------------
// Plan invariant guard tests
// ---------------------------------------------------------------------------

// TestExecutePlan_ActionsDepsLengthMismatch verifies that executePlan returns
// cleanly (no panic) when plan.Actions and plan.Deps have mismatched lengths.
func TestExecutePlan_ActionsDepsLengthMismatch(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)

	// Create a plan with mismatched Actions and Deps.
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionDownload, Path: "file.txt"},
			{Type: ActionDownload, Path: "file2.txt"},
		},
		Deps: [][]int{{1}}, // only 1 dep entry for 2 actions
	}

	report := &SyncReport{}

	// Should return cleanly without panic.
	eng.executePlan(t.Context(), plan, report, nil)

	// Invariant violation should surface in the report.
	assert.Equal(t, len(plan.Actions), report.Failed)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Error(), "invariant violation")
}

// ---------------------------------------------------------------------------
// Close() cleanup and idempotency
// ---------------------------------------------------------------------------

// TestEngine_Close_NilsObserversAndCleansStale verifies that Close() nils out
// observer references and cleans stale session files. Also tests idempotency —
// calling Close() twice must not panic.
func TestEngine_Close_NilsObserversAndCleansStale(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	dataDir := filepath.Join(tmpDir, "data")

	require.NoError(t, os.MkdirAll(syncRoot, 0o755))
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DataDir:   dataDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)

	// Simulate observers being set (as RunWatch does).
	eng.remoteObs = &RemoteObserver{}
	eng.localObs = &LocalObserver{}

	// First Close should succeed and nil out references.
	require.NoError(t, eng.Close())
	assert.Nil(t, eng.remoteObs, "remoteObs should be nil after Close")
	assert.Nil(t, eng.localObs, "localObs should be nil after Close")

	// Second Close must not panic (idempotency). The baseline DB is already
	// closed so the second call returns an error, which is acceptable.
	assert.NotPanics(t, func() {
		_ = eng.Close()
	}, "second Close must not panic")
}

// ---------------------------------------------------------------------------
// changeEventsToObservedItems converter tests
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_RemoteOnly(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "r1", Path: "remote.txt", DriveID: driveid.New(testDriveID)},
		{Source: SourceLocal, Path: "local.txt"},
		{Source: SourceRemote, ItemID: "r2", Path: "remote2.txt", DriveID: driveid.New(testDriveID)},
	}

	items := changeEventsToObservedItems(events)
	assert.Len(t, items, 2, "should only include remote events")
	assert.Equal(t, "r1", items[0].ItemID)
	assert.Equal(t, "r2", items[1].ItemID)
}

func TestChangeEventsToObservedItems_MapsAllFields(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	events := []ChangeEvent{
		{
			Source:    SourceRemote,
			ItemID:    "item1",
			ParentID:  "parent1",
			DriveID:   driveID,
			Path:      "docs/file.txt",
			ItemType:  ItemTypeFile,
			Hash:      "qxh1",
			Size:      1024,
			Mtime:     123456789,
			ETag:      "etag1",
			IsDeleted: false,
		},
		{
			Source:    SourceRemote,
			ItemID:    "item2",
			DriveID:   driveID,
			Path:      "docs/folder",
			ItemType:  ItemTypeFolder,
			IsDeleted: true,
		},
	}

	items := changeEventsToObservedItems(events)
	require.Len(t, items, 2)

	assert.Equal(t, driveID, items[0].DriveID)
	assert.Equal(t, "item1", items[0].ItemID)
	assert.Equal(t, "parent1", items[0].ParentID)
	assert.Equal(t, "docs/file.txt", items[0].Path)
	assert.Equal(t, "file", items[0].ItemType)
	assert.Equal(t, "qxh1", items[0].Hash)
	assert.Equal(t, int64(1024), items[0].Size)
	assert.Equal(t, int64(123456789), items[0].Mtime)
	assert.Equal(t, "etag1", items[0].ETag)
	assert.False(t, items[0].IsDeleted)

	assert.Equal(t, "folder", items[1].ItemType)
	assert.True(t, items[1].IsDeleted)
}

// ---------------------------------------------------------------------------
// Zero-event guard tests (Step 1)
// ---------------------------------------------------------------------------

// Validates: R-6.7.19
func TestObserveAndCommitRemote_ZeroEvents_NoTokenAdvance(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "new-token-should-not-be-saved",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	e, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	defer e.Close()

	ctx := t.Context()

	// Seed a known delta token.
	require.NoError(t, e.baseline.CommitDeltaToken(ctx, "old-token", driveID.String(), "", driveID.String()))

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemote with 0 events (only root, which is skipped).
	events, err := e.observeAndCommitRemote(ctx, bl)
	require.NoError(t, err)
	assert.Empty(t, events, "should return 0 events (root is skipped)")

	// Token should NOT have been advanced.
	savedToken, err := e.baseline.GetDeltaToken(ctx, driveID.String(), "")
	require.NoError(t, err)
	assert.Equal(t, "old-token", savedToken, "token should not advance when 0 events returned")
}

// Validates: R-2.15.1
func TestObserveAndCommitRemote_WithEvents_TokenAdvances(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "hello.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "new-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	e, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	defer e.Close()

	ctx := t.Context()

	// Seed a known delta token.
	require.NoError(t, e.baseline.CommitDeltaToken(ctx, "old-token", driveID.String(), "", driveID.String()))

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemote with actual events.
	events, err := e.observeAndCommitRemote(ctx, bl)
	require.NoError(t, err)
	assert.Len(t, events, 1, "should return 1 event (root is skipped)")

	// Token SHOULD have been advanced.
	savedToken, err := e.baseline.GetDeltaToken(ctx, driveID.String(), "")
	require.NoError(t, err)
	assert.Equal(t, "new-token", savedToken, "token should advance when events > 0")
}

// ---------------------------------------------------------------------------
// Full reconciliation tests (Step 2)
// ---------------------------------------------------------------------------

func TestFindOrphans_DetectsDeletedItems(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
		{Path: "c.txt", DriveID: driveID, ItemID: "id-c", ItemType: ItemTypeFile},
	})

	// Seen set has 2 of 3 items — id-b is missing (orphan).
	seen := map[driveid.ItemKey]struct{}{
		driveid.NewItemKey(driveID, "id-a"): {},
		driveid.NewItemKey(driveID, "id-c"): {},
	}

	orphans := bl.FindOrphans(seen, driveID, "")
	require.Len(t, orphans, 1, "should detect 1 orphan")
	assert.Equal(t, "b.txt", orphans[0].Path)
	assert.Equal(t, "id-b", orphans[0].ItemID)
	assert.Equal(t, ChangeDelete, orphans[0].Type)
	assert.Equal(t, SourceRemote, orphans[0].Source)
	assert.True(t, orphans[0].IsDeleted)
}

func TestFindOrphans_NoOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
	})

	// All baseline items are in the seen set.
	seen := map[driveid.ItemKey]struct{}{
		driveid.NewItemKey(driveID, "id-a"): {},
		driveid.NewItemKey(driveID, "id-b"): {},
	}

	orphans := bl.FindOrphans(seen, driveID, "")
	assert.Empty(t, orphans, "should find no orphans when all items are in seen set")
}

func TestFindOrphans_IgnoresOtherDrives(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	otherDrive := driveid.New("0000000000000002")

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "other.txt", DriveID: otherDrive, ItemID: "id-other", ItemType: ItemTypeFile},
	})

	// Empty seen set — only driveID's items should be orphaned.
	seen := map[driveid.ItemKey]struct{}{}

	orphans := bl.FindOrphans(seen, driveID, "")
	require.Len(t, orphans, 1, "should only detect orphans for the specified drive")
	assert.Equal(t, "a.txt", orphans[0].Path)
}

func TestFindOrphans_WithPathPrefix(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "SharedFolder/a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "SharedFolder/sub/b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
		{Path: "OtherFolder/c.txt", DriveID: driveID, ItemID: "id-c", ItemType: ItemTypeFile},
	})

	// Only id-a is in the seen set. id-b is an orphan under SharedFolder.
	// id-c is outside the prefix — should NOT be detected.
	seen := map[driveid.ItemKey]struct{}{
		driveid.NewItemKey(driveID, "id-a"): {},
	}

	orphans := bl.FindOrphans(seen, driveID, "SharedFolder")
	require.Len(t, orphans, 1, "should detect only orphans under the prefix")
	assert.Equal(t, "SharedFolder/sub/b.txt", orphans[0].Path)
}

func TestObserveRemoteFull_IntegratesOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	// Full delta returns 2 items (root + file1). Baseline has file1 + file2.
	// file2 should be detected as an orphan.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	e, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	defer e.Close()

	ctx := t.Context()

	// Seed baseline with 2 files (file1 + file2).
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})
	bl.Put(&BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: ItemTypeFile})

	events, token, err := e.observeRemoteFull(ctx, bl)
	require.NoError(t, err)
	assert.Equal(t, "full-token", token)

	// Should have 1 modify (file1 exists in baseline) + 1 orphan delete (file2).
	var modifies, deletes int
	for _, ev := range events {
		switch ev.Type {
		case ChangeModify:
			modifies++
		case ChangeDelete:
			deletes++
			assert.Equal(t, "file2.txt", ev.Path, "orphan should be file2.txt")
			assert.Equal(t, "f2", ev.ItemID)
			assert.True(t, ev.IsDeleted)
		case ChangeCreate, ChangeMove, ChangeShortcut:
			// Not expected in this test.
		}
	}

	assert.Equal(t, 1, modifies, "should have 1 modify event (file1 exists in baseline)")
	assert.Equal(t, 1, deletes, "should have 1 orphan delete event")
}

// ---------------------------------------------------------------------------
// changeEventsToObservedItems — empty ItemID guard (Item 4)
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_SkipsEmptyItemID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "valid-1", Path: "a.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "", Path: "bad.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "valid-2", Path: "b.txt", DriveID: driveID},
	}

	items := changeEventsToObservedItems(events)
	require.Len(t, items, 2, "empty ItemID event should be skipped")
	assert.Equal(t, "valid-1", items[0].ItemID)
	assert.Equal(t, "valid-2", items[1].ItemID)
}

// ---------------------------------------------------------------------------
// resolveReconcileInterval tests (Item 5)
// ---------------------------------------------------------------------------

func TestResolveReconcileInterval_Default(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	d := e.resolveReconcileInterval(WatchOpts{})
	assert.Equal(t, 24*time.Hour, d)
}

func TestResolveReconcileInterval_Disabled(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	d := e.resolveReconcileInterval(WatchOpts{ReconcileInterval: -1})
	assert.Equal(t, time.Duration(0), d)
}

func TestResolveReconcileInterval_Custom(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	d := e.resolveReconcileInterval(WatchOpts{ReconcileInterval: 2 * time.Hour})
	assert.Equal(t, 2*time.Hour, d)
}

func TestResolveReconcileInterval_ClampsBelowMinimum(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	d := e.resolveReconcileInterval(WatchOpts{ReconcileInterval: 1 * time.Minute})
	assert.Equal(t, minReconcileInterval, d, "should be clamped to 15 minutes")
}

// ---------------------------------------------------------------------------
// newReconcileTicker tests (Item 6)
// ---------------------------------------------------------------------------

func TestNewReconcileTicker_Zero(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	ticker := e.newReconcileTicker(0)
	assert.Nil(t, ticker, "zero duration should return nil")
}

func TestNewReconcileTicker_Negative(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	ticker := e.newReconcileTicker(-1)
	assert.Nil(t, ticker, "negative duration should return nil")
}

func TestNewReconcileTicker_Positive(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	ticker := e.newReconcileTicker(time.Hour)
	require.NotNil(t, ticker, "positive duration should return non-nil ticker")
	ticker.Stop()
}

// ---------------------------------------------------------------------------
// observeAndCommitRemoteFull tests (Item 6)
// ---------------------------------------------------------------------------

func TestObserveAndCommitRemoteFull(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	e, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	defer e.Close()

	ctx := t.Context()

	// Seed baseline with file1 + file2 (file2 will become an orphan).
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})
	bl.Put(&BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: ItemTypeFile})

	events, err := e.observeAndCommitRemoteFull(ctx, bl)
	require.NoError(t, err)

	// Should have modify (file1) + orphan delete (file2).
	var modifies, deletes int
	for _, ev := range events {
		switch ev.Type {
		case ChangeModify:
			modifies++
		case ChangeDelete:
			deletes++
			assert.Equal(t, "file2.txt", ev.Path)
			assert.True(t, ev.IsDeleted)
		case ChangeCreate, ChangeMove, ChangeShortcut:
			// not expected
		}
	}

	assert.Equal(t, 1, modifies)
	assert.Equal(t, 1, deletes)

	// Delta token should have been saved.
	savedToken, err := e.baseline.GetDeltaToken(ctx, driveID.String(), "")
	require.NoError(t, err)
	assert.Equal(t, "full-token", savedToken)
}

// ---------------------------------------------------------------------------
// runFullReconciliation tests (Item 6)
// ---------------------------------------------------------------------------

// Validates: R-2.1.6
func TestRunFullReconciliation_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	e, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	defer e.Close()

	ctx := t.Context()

	// Seed baseline matching delta exactly — no orphans.
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})

	safety := DefaultSafetyConfig()
	setupWatchEngine(t, e)

	// Should complete without panic; events exist but planner produces no
	// actions because nothing is different from baseline.
	e.runFullReconciliation(ctx, bl, SyncDownloadOnly, safety)
}

// Validates: R-2.1.6
func TestRunFullReconciliation_DeltaError(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, errors.New("delta unavailable")
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	safety := DefaultSafetyConfig()
	setupWatchEngine(t, e)

	// Should not panic — error is logged and function returns.
	e.runFullReconciliation(ctx, bl, SyncDownloadOnly, safety)
}

// ---------------------------------------------------------------------------
// Observation-time validation: path too long is caught by shouldObserve
// ---------------------------------------------------------------------------

func TestRunOnce_PathTooLong_RecordsIssue(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Create a deeply nested file >400 chars total, each component <255 chars.
	// shouldObserve catches this at observation time — the file never enters
	// the planner. recordSkippedItems writes it to sync_failures.
	deepPath := ""
	for range 51 {
		deepPath = filepath.Join(deepPath, "abcdefgh")
	}
	deepPath = filepath.Join(deepPath, "file.txt")
	require.Greater(t, len(deepPath), 400)

	writeLocalFile(t, syncRoot, deepPath, "test content")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	require.NoError(t, err)

	// The upload should NOT have been attempted — caught at observation time.
	assert.Equal(t, 0, report.Uploads, "path-too-long file should not reach planner")

	// The sync_failures table should have an entry from recordSkippedItems.
	issues, issErr := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, issErr)
	require.NotEmpty(t, issues, "sync_failures should have an entry for path too long")

	found := false
	for _, iss := range issues {
		if iss.IssueType == IssuePathTooLong {
			found = true

			break
		}
	}

	assert.True(t, found, "expected IssuePathTooLong issue in sync_failures")
}

// ---------------------------------------------------------------------------
// Issue #10: drainWorkerResults upload failure recording
// ---------------------------------------------------------------------------

// TestDrainWorkerResults_MultipleResults verifies the drain loop processes
// all buffered results before returning when the channel is closed.
func TestDrainWorkerResults_MultipleResults(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Set up depGraph with actions for each result.
	eng.depGraph = NewDepGraph(eng.logger)
	eng.readyCh = make(chan *TrackedAction, 16)
	for _, id := range []int64{1, 2, 3} {
		eng.depGraph.Add(&Action{Path: fmt.Sprintf("action-%d", id), Type: ActionUpload}, id, nil)
	}

	results := make(chan WorkerResult, 3)
	results <- WorkerResult{Path: "a.txt", ActionType: ActionUpload, Success: false, ErrMsg: "fail1", HTTPStatus: 500, ActionID: 1}
	results <- WorkerResult{Path: "b.txt", ActionType: ActionUpload, Success: false, ErrMsg: "fail2", HTTPStatus: 500, ActionID: 2}
	results <- WorkerResult{Path: "c.txt", ActionType: ActionDownload, Success: true, ActionID: 3}
	close(results)

	eng.drainWorkerResults(ctx, results, nil)

	// Both upload failures should produce sync_failures.
	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, issues, 2, "drain loop should process all results")
}

// ---------------------------------------------------------------------------
// processWorkerResult — shared helper tests
// ---------------------------------------------------------------------------

// setupEngineDepGraph creates a DepGraph on the engine and adds a dummy action
// for the given actionID so that processWorkerResult can call Complete without
// panicking on nil depGraph or unknown ID.
func setupEngineDepGraph(t *testing.T, eng *Engine, actionID int64) {
	t.Helper()
	eng.depGraph = NewDepGraph(eng.logger)
	eng.readyCh = make(chan *TrackedAction, 16)
	dummyAction := &Action{Path: "dummy", Type: ActionDownload}
	eng.depGraph.Add(dummyAction, actionID, nil)
}

func TestProcessWorkerResult_UploadFailure_RecordsLocalIssue(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "connection reset",
		HTTPStatus: 503,
	}, nil)

	// Should record upload failure in sync_failures.
	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "docs/report.xlsx", issues[0].Path)
	assert.Equal(t, "upload", issues[0].Direction)
	assert.Equal(t, "connection reset", issues[0].LastError)
	assert.Equal(t, 503, issues[0].HTTPStatus)
}

func TestProcessWorkerResult_403ReadOnly_SkipsRemoteState(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	// Set watchShortcuts so getWatchShortcuts() returns them for handle403.
	eng.watchShortcutsMu.Lock()
	eng.watchShortcuts = shortcuts
	eng.watchShortcutsMu.Unlock()

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "Shared/TeamDocs/file.txt",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "403 Forbidden",
		HTTPStatus: 403,
	}, bl)

	// Permission-denied should be recorded in sync_failures: one for the
	// file itself (from recordFailure) and one for the boundary directory
	// (from handle403). Both carry issue_type "permission_denied".
	permIssues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Len(t, permIssues, 2, "should record permission_denied for file + boundary directory")

	// remote_state should be empty.
	failed, err := eng.baseline.ListActionableRemoteState(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed, "confirmed read-only 403 should not be in remote_state")
}

func TestProcessWorkerResult_Success_NoRecords(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 0)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: ActionDownload,
		Success:    true,
	}, nil)

	// No failures should be recorded.
	failed, err := eng.baseline.ListActionableRemoteState(ctx)
	require.NoError(t, err)
	assert.Empty(t, failed)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

// ---------------------------------------------------------------------------
// classifyResult — pure classification of WorkerResult (R-6.8.15)
// ---------------------------------------------------------------------------

// Validates: R-6.8.15, R-6.7.12, R-6.7.14
func TestClassifyResult(t *testing.T) {
	t.Parallel()

	// outageErr is a GraphError whose Message triggers isOutagePattern.
	outageErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Message:    "ObjectHandle is Invalid for operation",
		Err:        graph.ErrBadRequest,
	}

	// normalBadRequest is a 400 that does NOT match the outage pattern.
	normalBadRequestErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Message:    "invalid payload",
		Err:        graph.ErrBadRequest,
	}

	tests := []struct {
		name      string
		result    WorkerResult
		wantClass resultClass
		wantScope ScopeKey
	}{
		{
			name:      "success",
			result:    WorkerResult{Success: true},
			wantClass: resultSuccess,
		},
		{
			name:      "context_canceled",
			result:    WorkerResult{Err: context.Canceled},
			wantClass: resultShutdown,
		},
		{
			name:      "context_deadline_exceeded",
			result:    WorkerResult{Err: context.DeadlineExceeded},
			wantClass: resultShutdown,
		},
		{
			name:      "wrapped_context_canceled",
			result:    WorkerResult{Err: fmt.Errorf("operation failed: %w", context.Canceled)},
			wantClass: resultShutdown,
		},
		{
			name:      "401_unauthorized",
			result:    WorkerResult{HTTPStatus: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
			wantClass: resultFatal,
		},
		{
			name:      "403_forbidden",
			result:    WorkerResult{HTTPStatus: http.StatusForbidden, Err: graph.ErrForbidden},
			wantClass: resultSkip,
		},
		{
			name:      "404_not_found",
			result:    WorkerResult{HTTPStatus: http.StatusNotFound, Err: graph.ErrNotFound},
			wantClass: resultRequeue,
		},
		{
			name:      "408_request_timeout",
			result:    WorkerResult{HTTPStatus: http.StatusRequestTimeout, Err: errors.New("timeout")},
			wantClass: resultRequeue,
		},
		{
			name:      "412_precondition_failed",
			result:    WorkerResult{HTTPStatus: http.StatusPreconditionFailed, Err: errors.New("etag mismatch")},
			wantClass: resultRequeue,
		},
		{
			name:      "423_locked",
			result:    WorkerResult{HTTPStatus: http.StatusLocked, Err: graph.ErrLocked},
			wantClass: resultRequeue,
		},
		{
			name:      "429_too_many_requests",
			result:    WorkerResult{HTTPStatus: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			wantClass: resultScopeBlock,
			wantScope: SKThrottleAccount,
		},
		{
			name:      "400_outage_pattern",
			result:    WorkerResult{HTTPStatus: http.StatusBadRequest, Err: outageErr},
			wantClass: resultRequeue,
		},
		{
			name:      "400_normal",
			result:    WorkerResult{HTTPStatus: http.StatusBadRequest, Err: normalBadRequestErr},
			wantClass: resultSkip,
		},
		{
			name:      "500_internal_server_error",
			result:    WorkerResult{HTTPStatus: http.StatusInternalServerError, Err: graph.ErrServerError},
			wantClass: resultRequeue,
		},
		{
			name:      "502_bad_gateway",
			result:    WorkerResult{HTTPStatus: http.StatusBadGateway, Err: graph.ErrServerError},
			wantClass: resultRequeue,
		},
		{
			name:      "503_service_unavailable",
			result:    WorkerResult{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			wantClass: resultRequeue,
		},
		{
			name:      "504_gateway_timeout",
			result:    WorkerResult{HTTPStatus: http.StatusGatewayTimeout, Err: graph.ErrServerError},
			wantClass: resultRequeue,
		},
		{
			name:      "509_bandwidth_limit",
			result:    WorkerResult{HTTPStatus: 509, Err: graph.ErrServerError},
			wantClass: resultRequeue,
		},
		{
			name: "507_own_drive",
			result: WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "",
			},
			wantClass: resultScopeBlock,
			wantScope: SKQuotaOwn,
		},
		{
			name: "507_shortcut_drive",
			result: WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "drive1:item1",
			},
			wantClass: resultScopeBlock,
			wantScope: SKQuotaShortcut("drive1:item1"),
		},
		{
			name:      "409_conflict",
			result:    WorkerResult{HTTPStatus: http.StatusConflict, Err: graph.ErrConflict},
			wantClass: resultSkip,
		},
		{
			name:      "other_4xx_falls_to_skip",
			result:    WorkerResult{HTTPStatus: http.StatusMethodNotAllowed, Err: graph.ErrMethodNotAllowed},
			wantClass: resultSkip,
		},
		{
			name:      "os_err_permission",
			result:    WorkerResult{Err: os.ErrPermission},
			wantClass: resultSkip,
		},
		{
			name:      "wrapped_os_err_permission",
			result:    WorkerResult{Err: fmt.Errorf("cannot write: %w", os.ErrPermission)},
			wantClass: resultSkip,
		},
		// Validates: R-2.10.43
		{
			name:      "disk_full",
			result:    WorkerResult{Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull)},
			wantClass: resultScopeBlock,
			wantScope: SKDiskLocal,
		},
		// Validates: R-2.10.44
		{
			name:      "file_too_large_for_space",
			result:    WorkerResult{Err: fmt.Errorf("download failed: %w", driveops.ErrFileTooLargeForSpace)},
			wantClass: resultSkip,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotClass, gotScope := classifyResult(&tt.result)
			assert.Equal(t, tt.wantClass, gotClass, "resultClass mismatch")
			assert.Equal(t, tt.wantScope, gotScope, "scope key mismatch")
		})
	}
}

// computeBackoff tests removed — backoff is now handled by retry.Reconcile
// policy via sync_failures + drain-loop retrier. See internal/retry/named_test.go.

// ---------------------------------------------------------------------------
// processTrialResult (R-2.10.5, R-2.10.6, R-2.10.8, R-2.10.14)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestProcessTrialResultV2_Success_ClearsScope(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	// Set up a scope block in the ScopeGate.
	now := eng.nowFunc()
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	}))

	// Add scope-blocked failures to the DB (these would be unblocked on success).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "first.txt", DriveID: driveid.New("d"), Direction: strUpload,
		Category: strTransient, ErrMsg: "rate limited", ScopeKey: SKThrottleAccount,
	}, nil)) // nil delayFn → scope-blocked (next_retry_at = NULL)

	// Add the trial action to the DepGraph.
	eng.depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	// Simulate successful trial result.
	eng.processTrialResult(ctx, &WorkerResult{
		ActionID:      1,
		IsTrial:       true,
		TrialScopeKey: SKThrottleAccount,
		Success:       true,
	})

	// Scope block should be cleared.
	assert.False(t, eng.scopeGate.IsScopeBlocked(SKThrottleAccount),
		"scope block should be removed after successful trial")

	// Scope-blocked failures should now be retryable (next_retry_at set to ~now).
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "scope-blocked failures should be unblocked after trial success")
}

// Validates: R-2.10.14
func TestProcessTrialResultV2_Failure_DoublesInterval(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKService, &ScopeBlock{
		Key:           SKService,
		IssueType:     IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	}))

	// Add the trial action to the DepGraph.
	eng.depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	eng.processTrialResult(ctx, &WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: SKService,
		Success:       false,
		HTTPStatus:    503,
		ErrMsg:        "service unavailable",
	})

	// Verify block's TrialInterval was doubled.
	got, ok := eng.scopeGate.GetScopeBlock(SKService)
	require.True(t, ok)
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled")
}

// Validates: R-2.10.6, R-2.10.8, R-2.10.14 — unified cap for all scope types.
func TestProcessTrialResultV2_Failure_CapsAt5m(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		scopeKey   ScopeKey
		issueType  string
		httpStatus int
		actionType ActionType
	}{
		{"quota", SKQuotaOwn, IssueQuotaExceeded, 507, ActionUpload},
		{"service", SKService, IssueServiceOutage, 500, ActionDownload},
		{"throttle", SKThrottleAccount, IssueRateLimited, 429, ActionUpload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eng := newPhase4Engine(t)
			ctx := t.Context()

			now := eng.nowFunc()

			// Start with an interval that would exceed 5m when doubled.
			require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, tt.scopeKey, &ScopeBlock{
				Key:           tt.scopeKey,
				IssueType:     tt.issueType,
				BlockedAt:     now,
				TrialInterval: 4 * time.Minute,
				NextTrialAt:   now.Add(4 * time.Minute),
			}))

			eng.depGraph.Add(&Action{Type: tt.actionType, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

			eng.processTrialResult(ctx, &WorkerResult{
				ActionID:      99,
				IsTrial:       true,
				TrialScopeKey: tt.scopeKey,
				Success:       false,
				HTTPStatus:    tt.httpStatus,
				ErrMsg:        "test failure",
			})

			got, ok := eng.scopeGate.GetScopeBlock(tt.scopeKey)
			require.True(t, ok)
			assert.Equal(t, defaultMaxTrialInterval, got.TrialInterval,
				"%s interval should cap at %v", tt.name, defaultMaxTrialInterval)
		})
	}
}

// Validates: Group A — trial failure must NOT trigger scope detection.
func TestProcessTrialResultV2_Failure_NoScopeDetection(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	ss := NewScopeState(eng.nowFn, eng.logger)
	eng.scopeState = ss

	now := eng.nowFunc()
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKService, &ScopeBlock{
		Key:           SKService,
		IssueType:     IssueServiceOutage,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	}))

	eng.depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	eng.processTrialResult(ctx, &WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: SKService,
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
	})

	got, ok := eng.scopeGate.GetScopeBlock(SKService)
	require.True(t, ok, "scope block should still exist")
	assert.Equal(t, 60*time.Second, got.TrialInterval, "interval should be doubled, not reset")
}

// Validates: R-2.10.14 — computeTrialInterval is the single source of truth
// for initial intervals and backoff extensions.
func TestComputeTrialInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		retryAfter      time.Duration
		currentInterval time.Duration
		want            time.Duration
	}{
		// Retry-After: used directly, no cap (R-2.10.7).
		{"retry-after honored", 90 * time.Second, 0, 90 * time.Second},
		{"retry-after exceeds max", 30 * time.Minute, 0, 30 * time.Minute},
		{"retry-after with current", 2 * time.Minute, 30 * time.Second, 2 * time.Minute},

		// No Retry-After, no current: initial interval.
		{"initial interval", 0, 0, defaultInitialTrialInterval},

		// No Retry-After, with current: double + cap.
		{"double interval", 0, 30 * time.Second, 60 * time.Second},
		{"double caps at max", 0, 4 * time.Minute, defaultMaxTrialInterval},
		{"already at max stays", 0, defaultMaxTrialInterval, defaultMaxTrialInterval},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTrialInterval(tt.retryAfter, tt.currentInterval)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.10.7 — Retry-After is used directly with no cap.
func TestExtendTrialInterval_WithRetryAfter(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	now := eng.nowFunc()
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     IssueRateLimited,
		BlockedAt:     now,
		TrialInterval: 30 * time.Second,
		NextTrialAt:   now.Add(30 * time.Second),
	}))

	eng.depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 99, nil)

	// Retry-After of 30 minutes exceeds defaultMaxTrialInterval (5m) — must be
	// honored directly with no cap, because the server is ground truth.
	eng.processTrialResult(ctx, &WorkerResult{
		ActionID:      99,
		IsTrial:       true,
		TrialScopeKey: SKThrottleAccount,
		Success:       false,
		HTTPStatus:    429,
		RetryAfter:    30 * time.Minute,
		ErrMsg:        "too many requests",
	})

	got, ok := eng.scopeGate.GetScopeBlock(SKThrottleAccount)
	require.True(t, ok)
	assert.Equal(t, 30*time.Minute, got.TrialInterval,
		"Retry-After must be used directly with no cap — server is ground truth")
}

// Validates: R-2.10.43 — full disk:local scope-block lifecycle:
// ErrDiskFull → classifyResult → ScopeGate blocks downloads → trial → release.
func TestDiskLocalScopeBlock_FullCycle(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()
	now := eng.nowFunc()

	// 1. classifyResult maps ErrDiskFull to disk:local scope block.
	class, scope := classifyResult(&WorkerResult{
		Err: fmt.Errorf("download failed: %w", driveops.ErrDiskFull),
	})
	require.Equal(t, resultScopeBlock, class)
	require.Equal(t, SKDiskLocal, scope)

	// 2. Establish the scope block via ScopeGate.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKDiskLocal, &ScopeBlock{
		Key:           SKDiskLocal,
		IssueType:     IssueDiskFull,
		BlockedAt:     now,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(10 * time.Second),
	}))

	// 3. ScopeGate.Admit blocks downloads under disk:local, allows uploads.
	dlAction := &TrackedAction{ID: 1, Action: Action{Type: ActionDownload, Path: "big.zip", DriveID: driveid.New("d"), ItemID: "dl1"}}
	ulAction := &TrackedAction{ID: 2, Action: Action{Type: ActionUpload, Path: "small.txt", DriveID: driveid.New("d"), ItemID: "ul1"}}

	assert.False(t, eng.scopeGate.Admit(dlAction).IsZero(), "download should be blocked by disk:local scope")
	assert.True(t, eng.scopeGate.Admit(ulAction).IsZero(), "upload should NOT be blocked by disk:local scope")

	// 4. Clear scope block (simulating trial success / disk space freed).
	eng.onScopeClear(ctx, SKDiskLocal)

	// 5. Download should now be admitted.
	assert.True(t, eng.scopeGate.Admit(dlAction).IsZero(), "download should be admitted after scope clear")
}

// ---------------------------------------------------------------------------
// deriveScopeKey
// ---------------------------------------------------------------------------

// Validates: R-2.10.2
func TestDeriveScopeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		httpStatus  int
		shortcutKey string
		want        ScopeKey
	}{
		{"429_throttle", 429, "", SKThrottleAccount},
		{"503_service", 503, "", SKService},
		{"507_own", 507, "", SKQuotaOwn},
		{"507_shortcut", 507, "drive1:item1", SKQuotaShortcut("drive1:item1")},
		{"500_service", 500, "", SKService},
		{"502_service", 502, "", SKService},
		{"200_empty", 200, "", ScopeKey{}},
		{"404_empty", 404, "", ScopeKey{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &WorkerResult{HTTPStatus: tt.httpStatus, ShortcutKey: tt.shortcutKey}
			assert.Equal(t, tt.want, deriveScopeKey(r))
		})
	}
}

// ---------------------------------------------------------------------------
// applyScopeBlock arms trial timer
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestApplyScopeBlock_ArmsTrialTimer(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	now := eng.nowFunc()

	// applyScopeBlock sets the scope block via ScopeGate and arms the trial timer.
	eng.applyScopeBlock(ScopeUpdateResult{
		Block:      true,
		ScopeKey:   SKThrottleAccount,
		IssueType:  IssueRateLimited,
		RetryAfter: 30 * time.Second,
	})

	// Verify the block has the correct NextTrialAt from the injectable clock.
	earliest, ok := eng.scopeGate.EarliestTrialAt()
	require.True(t, ok, "EarliestTrialAt should find the scope block")
	assert.Equal(t, now.Add(30*time.Second), earliest, "NextTrialAt should be now + trial interval")

	// Trial timer should be armed.
	eng.trialMu.Lock()
	timerSet := eng.trialTimer != nil
	eng.trialMu.Unlock()
	assert.True(t, timerSet, "trial timer should be armed after applyScopeBlock")
}

// ---------------------------------------------------------------------------
// recordFailure populates ScopeKey
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "quota-fail.txt",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "insufficient storage",
		HTTPStatus: 507,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, SKQuotaOwn, issues[0].ScopeKey, "507 own-drive should populate scope key")
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_429(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "throttled.txt",
		ActionType: ActionDownload,
		Success:    false,
		ErrMsg:     "too many requests",
		HTTPStatus: 429,
		ActionID:   1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, SKThrottleAccount, issues[0].ScopeKey)
}

// Validates: R-2.10.11
func TestRecordFailure_PopulatesScopeKey_507Shortcut(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineDepGraph(t, eng, 1)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:        "shared/file.txt",
		ActionType:  ActionUpload,
		Success:     false,
		ErrMsg:      "quota exceeded",
		HTTPStatus:  507,
		ShortcutKey: "driveA:item42",
		ActionID:    1,
	}, nil)

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, SKQuotaShortcut("driveA:item42"), issues[0].ScopeKey)
}

// ---------------------------------------------------------------------------
// Drain loop integration tests (Phase 4 architecture: DepGraph + ScopeGate)
// ---------------------------------------------------------------------------

// startDrainLoop creates a real engine with DepGraph, ScopeGate, readyCh,
// scopeState, buf, and retryTimerCh — the full Phase 4 pipeline — and starts
// drainWorkerResults. Returns:
//   - results chan: test sends WorkerResults here (simulating workers)
//   - ready chan: test reads dispatched actions from readyCh
//   - cancel func: stops the drain goroutine
//   - eng: the engine (for state inspection)
//   - buf: the Buffer (for verifying retrier re-injection)
func startDrainLoop(t *testing.T) (chan WorkerResult, <-chan *TrackedAction, context.CancelFunc, *Engine, *Buffer) {
	t.Helper()

	eng := newPhase4Engine(t)
	eng.scopeState = NewScopeState(eng.nowFunc, eng.logger)

	buf := NewBuffer(eng.logger)
	eng.buf = buf

	results := make(chan WorkerResult, 16)
	ready := eng.readyCh

	ctx, cancel := context.WithCancel(t.Context())
	go eng.drainWorkerResults(ctx, results, nil)

	return results, ready, cancel, eng, buf
}

// readReady reads one TrackedAction from the ready channel with a 1s timeout.
func readReady(t *testing.T, ready <-chan *TrackedAction) {
	t.Helper()

	select {
	case <-ready:
	case <-time.After(time.Second):
		require.Fail(t, "timed out waiting for action on ready channel")
	}
}

// Validates: R-2.10.5 — drain loop processes results and routes dependents.
func TestE2E_DrainLoop_ProcessesAndRoutes(t *testing.T) {
	t.Parallel()

	results, ready, cancel, eng, _ := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()

	// Add parent action to DepGraph, send to readyCh.
	ta := eng.depGraph.Add(&Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 0, nil)
	require.NotNil(t, ta)
	eng.readyCh <- ta
	readReady(t, ready)

	// Send 429 result — scope detection creates block + records failure.
	results <- WorkerResult{
		ActionID:   0,
		Path:       "a.txt",
		ActionType: ActionUpload,
		DriveID:    driveid.New(engineTestDriveID),
		Success:    false,
		HTTPStatus: 429,
		RetryAfter: 5 * time.Millisecond,
		ErrMsg:     "rate limited",
		Err:        fmt.Errorf("rate limited"),
	}

	// Verify scope block created and failure recorded.
	require.Eventually(t, func() bool {
		return eng.scopeGate.IsScopeBlocked(SKThrottleAccount)
	}, time.Second, time.Millisecond, "scope block should be created")

	issues, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, issues, "failure should be recorded")
}

// TestE2E_DrainLoop_TrialResultSuccess verifies that trial success clears the
// scope block and unblocks failures via processTrialResult in the drain loop.
func TestE2E_DrainLoop_TrialResultSuccess(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng, _ := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()

	// Set up scope block and a scope-blocked failure.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     IssueRateLimited,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	}))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "blocked.txt", DriveID: driveid.New(engineTestDriveID), Direction: strUpload,
		Category: strTransient, ErrMsg: "rate limited", ScopeKey: SKThrottleAccount,
	}, nil))

	// Add trial action to depGraph.
	ta := eng.depGraph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 1, nil)
	require.NotNil(t, ta)

	// Send trial success via results channel.
	results <- WorkerResult{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: SKThrottleAccount,
	}

	// Scope block should be cleared.
	require.Eventually(t, func() bool {
		return !eng.scopeGate.IsScopeBlocked(SKThrottleAccount)
	}, time.Second, time.Millisecond, "scope block should be cleared after trial success")
}

// TestE2E_DrainLoop_TrialResultFailure verifies trial failure doubles the interval.
func TestE2E_DrainLoop_TrialResultFailure(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng, _ := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()

	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKService, &ScopeBlock{
		Key:           SKService,
		IssueType:     IssueServiceOutage,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	}))

	ta := eng.depGraph.Add(&Action{Type: ActionDownload, Path: "trial.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "i1"}, 99, nil)
	require.NotNil(t, ta)

	results <- WorkerResult{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    ActionDownload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       false,
		HTTPStatus:    500,
		ErrMsg:        "internal server error",
		Err:           fmt.Errorf("internal server error"),
		IsTrial:       true,
		TrialScopeKey: SKService,
	}

	// Interval should be doubled from 10ms to 20ms.
	require.Eventually(t, func() bool {
		block, ok := eng.scopeGate.GetScopeBlock(SKService)
		return ok && block.TrialInterval == 20*time.Millisecond
	}, time.Second, time.Millisecond, "trial failure should double interval")
}

func TestE2E_DrainExit_StopsTimer(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng, _ := startDrainLoop(t)
	defer cancel()

	// Create scope block → arms trial timer.
	eng.applyScopeBlock(ScopeUpdateResult{
		Block:      true,
		ScopeKey:   SKService,
		IssueType:  IssueServiceOutage,
		RetryAfter: time.Hour, // long interval so it doesn't fire during test
	})

	// Verify timer is armed.
	require.Eventually(t, func() bool {
		eng.trialMu.Lock()
		defer eng.trialMu.Unlock()
		return eng.trialTimer != nil
	}, time.Second, time.Millisecond)

	// Close results channel → drainWorkerResults returns → defer stopTrialTimer.
	close(results)

	// Timer should be cleared.
	require.Eventually(t, func() bool {
		eng.trialMu.Lock()
		defer eng.trialMu.Unlock()
		return eng.trialTimer == nil
	}, time.Second, time.Millisecond)
}

// ---------------------------------------------------------------------------
// Unit tests — trial timing initial intervals and caps (R-2.10.6, R-2.10.7, R-2.10.8)
// ---------------------------------------------------------------------------

// Validates: R-2.10.6
func TestTrialTimer_QuotaStartsAt5s(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Feed 3 unique paths with 507 within 10s → triggers quota:own block.
	for i := range 3 {
		r := &WorkerResult{
			Path:       fmt.Sprintf("/file-%d.txt", i),
			HTTPStatus: 507,
		}
		sr := ss.UpdateScope(r)
		if i < 2 {
			assert.False(t, sr.Block, "should not trigger before threshold")
		} else {
			require.True(t, sr.Block, "should trigger at threshold")
			assert.Equal(t, SKQuotaOwn, sr.ScopeKey)
			assert.Equal(t, "quota_exceeded", sr.IssueType)
			assert.Zero(t, sr.RetryAfter,
				"sliding window trigger should have zero RetryAfter")
		}
	}
}

// TestTrialTimer_BackoffCapsAt5m is covered by
// TestProcessTrialResultV2_Failure_CapsAt5m which uses ScopeGate.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.7
func TestTrialTimer_RateLimited_StartsAtRetryAfter(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	r := &WorkerResult{
		Path:       "/file.txt",
		HTTPStatus: 429,
		RetryAfter: 90 * time.Second,
	}

	sr := ss.UpdateScope(r)
	require.True(t, sr.Block, "429 should immediately trigger block")
	assert.Equal(t, SKThrottleAccount, sr.ScopeKey)
	assert.Equal(t, "rate_limited", sr.IssueType)
	assert.Equal(t, 90*time.Second, sr.RetryAfter,
		"rate_limited RetryAfter should pass through server value")
}

// TestTrialTimer_RateLimited_BlocksAllActionTypes is covered by
// TestScopeGate_Admit_Blocked and related tests in scope_gate_test.go.
// Removed: old test used held-queue mechanism.

// Validates: R-2.10.8
func TestTrialTimer_Service_StartsAt5s(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	// Feed 5 unique paths with 500 within 30s → triggers service block.
	for i := range 5 {
		r := &WorkerResult{
			Path:       fmt.Sprintf("/file-%d.txt", i),
			HTTPStatus: 500,
		}
		sr := ss.UpdateScope(r)
		if i < 4 {
			assert.False(t, sr.Block, "should not trigger before threshold")
		} else {
			require.True(t, sr.Block, "should trigger at threshold")
			assert.Equal(t, SKService, sr.ScopeKey)
			assert.Equal(t, "service_outage", sr.IssueType)
			assert.Zero(t, sr.RetryAfter,
				"sliding window trigger should have zero RetryAfter")
		}
	}
}

// Validates: R-2.10.8
func TestTrialTimer_Service_503RetryAfterOverride(t *testing.T) {
	t.Parallel()

	clock, _ := controllableClock()
	ss := NewScopeState(clock, discardLogger())

	r := &WorkerResult{
		Path:       "/file.txt",
		HTTPStatus: 503,
		RetryAfter: 120 * time.Second,
	}

	sr := ss.UpdateScope(r)
	require.True(t, sr.Block, "503 with Retry-After should immediately trigger block")
	assert.Equal(t, SKService, sr.ScopeKey)
	assert.Equal(t, "service_outage", sr.IssueType)
	assert.Equal(t, 120*time.Second, sr.RetryAfter,
		"503+Retry-After should pass through server value")
}

// clearResolvedSkippedItems
// ---------------------------------------------------------------------------

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_AllThreeIssueTypes(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record failures for each scanner-detectable issue type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad\x01name.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueInvalidFilename, Category: "actionable", ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "still-bad\x02.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueInvalidFilename, Category: "actionable", ErrMsg: "invalid character",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "very/long/path.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssuePathTooLong, Category: "actionable", ErrMsg: "path exceeds limit",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "huge-file.bin", DriveID: driveID, Direction: "upload",
		IssueType: IssueFileTooLarge, Category: "actionable", ErrMsg: "file too large",
	}, nil))

	// Verify all 4 failures exist.
	all, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 4)

	// Simulate a new scan where only "still-bad\x02.txt" still exists as skipped.
	// "bad\x01name.txt" was renamed, "very/long/path.txt" was shortened,
	// "huge-file.bin" was deleted.
	currentSkipped := []SkippedItem{
		{Path: "still-bad\x02.txt", Reason: IssueInvalidFilename},
	}

	eng.clearResolvedSkippedItems(ctx, currentSkipped)

	// Only the still-existing invalid filename should remain.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "still-bad\x02.txt", remaining[0].Path)
	assert.Equal(t, IssueInvalidFilename, remaining[0].IssueType)
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_EmptySkipped_ClearsAll(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record one failure per type.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueInvalidFilename, Category: "actionable", ErrMsg: "invalid",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "long.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssuePathTooLong, Category: "actionable", ErrMsg: "too long",
	}, nil))

	// Empty scan — all problematic files were resolved.
	eng.clearResolvedSkippedItems(ctx, nil)

	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "all scanner-detectable failures should be cleared when no skipped items remain")
}

// Validates: R-2.10.2
func TestClearResolvedSkippedItems_DoesNotAffectRuntimeIssues(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record a scanner-detectable failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "bad.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueInvalidFilename, Category: "actionable", ErrMsg: "invalid",
	}, nil))

	// Record a runtime failure (permission denied — not scanner-detectable).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "Shared/folder", DriveID: driveID, Direction: "upload",
		IssueType: IssuePermissionDenied, Category: "actionable", ErrMsg: "read-only",
		HTTPStatus: 403,
	}, nil))

	// Clear all scanner-detectable items (empty = all resolved).
	eng.clearResolvedSkippedItems(ctx, nil)

	// Runtime failure should survive — clearResolvedSkippedItems only
	// clears invalid_filename, path_too_long, file_too_large.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, IssuePermissionDenied, remaining[0].IssueType)
}

// Validates: R-2.12.2
func TestClearResolvedSkippedItems_CaseCollision(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record case collision failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "File.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueCaseCollision, Category: "actionable",
		ErrMsg: "conflicts with file.txt",
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "file.txt", DriveID: driveID, Direction: "upload",
		IssueType: IssueCaseCollision, Category: "actionable",
		ErrMsg: "conflicts with File.txt",
	}, nil))

	// Verify both exist.
	all, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	// Simulate user renaming one collider — next scan finds zero case collisions.
	eng.clearResolvedSkippedItems(ctx, nil)

	// Both case collision entries should be cleared.
	remaining, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "case collision failures should be auto-cleared when resolved")
}

// ---------------------------------------------------------------------------
// feedScopeDetection guard: local errors must not feed scope windows
// ---------------------------------------------------------------------------

// Validates: R-6.7.27
func TestFeedScopeDetection_LocalErrorIgnored(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	eng.scopeState = NewScopeState(time.Now, eng.logger)

	// Feed several local errors (HTTPStatus=0) — should not trigger a scope block.
	for i := range 10 {
		eng.feedScopeDetection(&WorkerResult{
			Path:       fmt.Sprintf("file-%d.txt", i),
			ActionType: ActionDownload,
			HTTPStatus: 0, // local error — no HTTP status
			Err:        os.ErrPermission,
			ErrMsg:     "permission denied",
		})
	}

	// No scope block should have been created.
	assert.False(t, eng.scopeGate.IsScopeBlocked(SKService),
		"local errors with HTTPStatus=0 must not trigger service scope")
	assert.False(t, eng.scopeGate.IsScopeBlocked(SKThrottleAccount),
		"local errors with HTTPStatus=0 must not trigger throttle scope")
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_Throttled(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	// Initially not suppressed.
	assert.False(t, eng.isObservationSuppressed())

	// After throttle block, should be suppressed.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKThrottleAccount, &ScopeBlock{
		Key:           SKThrottleAccount,
		IssueType:     IssueRateLimited,
		TrialInterval: 30 * time.Second,
	}))
	assert.True(t, eng.isObservationSuppressed())
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_ServiceOutage(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	// Service outage should also suppress.
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKService, &ScopeBlock{
		Key:           SKService,
		IssueType:     IssueServiceOutage,
		TrialInterval: 60 * time.Second,
	}))
	assert.True(t, eng.isObservationSuppressed())
}

// Validates: R-2.10.30
func TestIsObservationSuppressed_NilScopeGate(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.scopeGate = nil

	// With nil scopeGate, should not panic and should return false.
	assert.False(t, eng.isObservationSuppressed())
}

// Validates: R-2.10.30, R-2.10.31
func TestIsObservationSuppressed_QuotaDoesNotSuppress(t *testing.T) {
	t.Parallel()

	eng := newPhase4Engine(t)
	ctx := t.Context()

	// Quota scope block should NOT suppress observation (R-2.10.31).
	require.NoError(t, eng.scopeGate.SetScopeBlock(ctx, SKQuotaOwn, &ScopeBlock{
		Key:           SKQuotaOwn,
		IssueType:     IssueQuotaExceeded,
		TrialInterval: 5 * time.Minute,
	}))
	assert.False(t, eng.isObservationSuppressed())
}

// ---------------------------------------------------------------------------
// issueTypeForHTTPStatus — maps HTTP status to issue type (R-6.6.10)
// ---------------------------------------------------------------------------

// Validates: R-6.6.10
func TestIssueTypeForHTTPStatus(t *testing.T) {
	t.Parallel()

	outageErr := &graph.GraphError{
		StatusCode: http.StatusBadRequest,
		Message:    "ObjectHandle is Invalid for operation",
		Err:        graph.ErrBadRequest,
	}

	tests := []struct {
		name       string
		httpStatus int
		err        error
		want       string
	}{
		{"429_rate_limited", http.StatusTooManyRequests, nil, IssueRateLimited},
		{"507_quota_exceeded", http.StatusInsufficientStorage, nil, IssueQuotaExceeded},
		{"403_permission_denied", http.StatusForbidden, nil, IssuePermissionDenied},
		{"400_outage_pattern", http.StatusBadRequest, outageErr, IssueServiceOutage},
		{"400_normal", http.StatusBadRequest, errors.New("bad request"), ""},
		{"500_service_outage", http.StatusInternalServerError, nil, IssueServiceOutage},
		{"503_service_outage", http.StatusServiceUnavailable, nil, IssueServiceOutage},
		{"408_request_timeout", http.StatusRequestTimeout, nil, "request_timeout"},
		{"412_transient_conflict", http.StatusPreconditionFailed, nil, "transient_conflict"},
		{"404_transient_not_found", http.StatusNotFound, nil, "transient_not_found"},
		{"423_resource_locked", http.StatusLocked, nil, "resource_locked"},
		{"permission_error", 0, os.ErrPermission, IssueLocalPermissionDenied},
		// Validates: R-2.10.43
		{"disk_full", 0, driveops.ErrDiskFull, IssueDiskFull},
		{"wrapped_disk_full", 0, fmt.Errorf("download: %w", driveops.ErrDiskFull), IssueDiskFull},
		// Validates: R-2.10.44
		{"file_too_large_for_space", 0, driveops.ErrFileTooLargeForSpace, IssueFileTooLargeForSpace},
		{"unknown_status", 418, nil, ""},
		{"zero_status_no_error", 0, nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := issueTypeForHTTPStatus(tt.httpStatus, tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// logFailureSummary — aggregated failure logging (R-6.6.12)
// ---------------------------------------------------------------------------

// Validates: R-6.6.12
func TestLogFailureSummary_AggregatesAboveThreshold(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})

	// Add 15 errors with the same message prefix — should aggregate.
	eng.syncErrorsMu.Lock()
	for i := range 15 {
		eng.syncErrors = append(eng.syncErrors, fmt.Errorf("quota_exceeded: upload failed for file %d", i))
	}
	eng.syncErrorsMu.Unlock()

	// Should not panic; clears syncErrors after logging.
	eng.logFailureSummary()

	eng.syncErrorsMu.Lock()
	assert.Empty(t, eng.syncErrors, "syncErrors should be cleared after summary")
	eng.syncErrorsMu.Unlock()
}

// Validates: R-6.6.12
func TestLogFailureSummary_NoErrors(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})

	// Should be a no-op with no errors.
	eng.logFailureSummary()

	eng.syncErrorsMu.Lock()
	assert.Empty(t, eng.syncErrors)
	eng.syncErrorsMu.Unlock()
}

// ---------------------------------------------------------------------------
// Retrier pipeline integration test (Phase 4 architecture)
//
// Exercises the drain-loop integrated retrier: action → failure → sync_failures
// → retry timer fires → runRetrierSweep → createEventFromDB → Buffer.
// ---------------------------------------------------------------------------

// Validates: R-6.8.10, R-6.8.11, R-6.8.7
func TestRetryPipeline_TransientFailure_DrainLoopRetrier(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng, buf := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	testPath := "docs/report.pdf"

	// Seed remote_state so createEventFromDB can build a full event when
	// the retrier sweep processes this failure.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     testPath,
			ItemType: strFile,
			Hash:     "report-hash",
			Size:     4096,
		},
	}, "", driveID))

	// Add action to depGraph, send to readyCh, drain it.
	ta := eng.depGraph.Add(&Action{
		Type: ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-abc",
	}, 0, nil)
	require.NotNil(t, ta)
	eng.readyCh <- ta
	readReady(t, eng.readyCh)

	// Use a nowFn that's 1 hour in the future so retrier sees rows as due.
	futureTime := time.Now().Add(time.Hour)
	eng.nowFn = func() time.Time { return futureTime }

	// Send a 503 result — classifies as resultRequeue (transient).
	results <- WorkerResult{
		ActionID:   0,
		Path:       testPath,
		ActionType: ActionDownload,
		DriveID:    driveID,
		Success:    false,
		HTTPStatus: http.StatusServiceUnavailable,
		ErrMsg:     "service unavailable",
		Err:        fmt.Errorf("service unavailable"),
	}

	// Verify: sync_failures row created.
	require.Eventually(t, func() bool {
		rows, err := eng.baseline.ListSyncFailures(ctx)
		return err == nil && len(rows) == 1
	}, time.Second, time.Millisecond, "sync_failures row should be created")

	// Trigger retrier sweep manually (in production this fires from retry timer).
	eng.runRetrierSweep(ctx)

	// Verify: retrier injected event into buffer.
	assert.Greater(t, buf.Len(), 0, "retrier should inject event into buffer")

	flushed := buf.FlushImmediate()
	require.Len(t, flushed, 1, "should have exactly one path in buffer")
	assert.Equal(t, testPath, flushed[0].Path, "buffered path")
}

// Validates: R-6.8.10
func TestDrainLoop_Success_ClearsSyncFailure(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng, _ := startDrainLoop(t)
	defer cancel()

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	testPath := "docs/stale-failure.txt"

	// Seed a sync_failures row — simulates a previous transient failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: testPath, DriveID: driveID, Direction: "download",
		Category: "transient", ErrMsg: "previous failure",
		HTTPStatus: http.StatusServiceUnavailable,
	}, func(int) time.Duration { return time.Hour }))

	rows, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "seeded failure should exist")

	// Add action, send to readyCh, drain it.
	ta := eng.depGraph.Add(&Action{
		Type: ActionDownload, Path: testPath, DriveID: driveID, ItemID: "item-ok",
	}, 0, nil)
	require.NotNil(t, ta)
	eng.readyCh <- ta
	readReady(t, eng.readyCh)

	// Send a success result — defensive clear removes the row.
	results <- WorkerResult{
		ActionID: 0, Path: testPath, ActionType: ActionDownload,
		DriveID: driveID, Success: true,
	}

	// Verify: sync_failures row cleared.
	require.Eventually(t, func() bool {
		rows, err := eng.baseline.ListSyncFailures(ctx)
		return err == nil && len(rows) == 0
	}, time.Second, time.Millisecond, "sync_failures row should be cleared on success")

	assert.Equal(t, int32(1), eng.succeeded.Load(), "succeeded counter")
}
