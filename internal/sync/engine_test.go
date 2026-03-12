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
	// entries as deleted → EF6 → ActionRemoteDelete. 20 remote deletes on a
	// 20-entry baseline = 100% > 50% threshold → ErrBigDeleteTriggered.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
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
// deadlock. Regression test for: empty plan caused NewDepTracker with total=0,
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

	eng := &Engine{}
	cfg := eng.resolveSafetyConfig(RunOpts{})

	def := DefaultSafetyConfig()
	assert.Equal(t, def.BigDeleteMaxCount, cfg.BigDeleteMaxCount)
	assert.Equal(t, def.BigDeleteMaxPercent, cfg.BigDeleteMaxPercent)
}

func TestResolveSafetyConfig_Force(t *testing.T) {
	t.Parallel()

	eng := &Engine{}
	cfg := eng.resolveSafetyConfig(RunOpts{Force: true})

	assert.Equal(t, forceSafetyMax, cfg.BigDeleteMaxCount)
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

// TestRunWatch_ProcessBatch_BigDelete verifies that big-delete protection
// triggers are handled gracefully (batch is skipped, loop continues).
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

	// Build a batch that would delete all 20 files (100% > threshold).
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

	tracker := NewPersistentDepTracker(testLogger(t))
	safety := DefaultSafetyConfig()

	// processBatch should log a warning and return without panicking.
	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety, tracker)

	// Verify no actions were dispatched (big-delete skipped the batch).
	select {
	case ta := <-tracker.Ready():
		assert.Fail(t, "unexpected action dispatched", "path: %s", ta.Action.Path)
	default:
		// Good — no actions.
	}
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

	tracker := NewPersistentDepTracker(testLogger(t))
	safety := DefaultSafetyConfig()

	// Should return without error or dispatching actions.
	eng.processBatch(ctx, batch, bl, SyncBidirectional, safety, tracker)
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

	tracker := NewPersistentDepTracker(testLogger(t))
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

	eng.processBatch(ctx, batch1, bl, SyncBidirectional, safety, tracker)

	// Verify the action is in-flight.
	require.True(t, tracker.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after first batch")

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

	eng.processBatch(ctx, batch2, bl, SyncBidirectional, safety, tracker)

	// The second batch should have replaced the first.
	// We can't easily verify cancellation directly, but we can verify
	// the path is still tracked (new action replaced old one).
	assert.True(t, tracker.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after second batch")
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
	tracker := NewDepTracker(10, testLogger(t))

	// Should complete without panic; events exist but planner produces no
	// actions because nothing is different from baseline.
	e.runFullReconciliation(ctx, bl, SyncDownloadOnly, safety, tracker)
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
	tracker := NewDepTracker(10, testLogger(t))

	// Should not panic — error is logged and function returns.
	e.runFullReconciliation(ctx, bl, SyncDownloadOnly, safety, tracker)
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

	// Set up tracker with actions for each result.
	tracker := NewDepTracker(16, eng.logger)
	for _, id := range []int64{1, 2, 3} {
		tracker.Add(&Action{Path: fmt.Sprintf("action-%d", id), Type: ActionUpload}, id, nil)
	}
	eng.tracker = tracker

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

// setupEngineTracker creates a one-shot DepTracker on the engine and adds a
// dummy action for the given actionID so that processWorkerResult can call
// Complete without panicking on nil tracker or unknown ID.
func setupEngineTracker(t *testing.T, eng *Engine, actionID int64) {
	t.Helper()
	tracker := NewDepTracker(16, eng.logger)
	dummyAction := &Action{Path: "dummy", Type: ActionDownload}
	tracker.Add(dummyAction, actionID, nil)
	eng.tracker = tracker
}

func TestProcessWorkerResult_UploadFailure_RecordsLocalIssue(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	setupEngineTracker(t, eng, 0)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "connection reset",
		HTTPStatus: 503,
	}, nil, nil)

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
	setupEngineTracker(t, eng, 0)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "Shared/TeamDocs/file.txt",
		ActionType: ActionUpload,
		Success:    false,
		ErrMsg:     "403 Forbidden",
		HTTPStatus: 403,
	}, bl, shortcuts)

	// Permission-denied should be recorded in sync_failures.
	permIssues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Len(t, permIssues, 1, "should record permission_denied issue")

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
	setupEngineTracker(t, eng, 0)

	eng.processWorkerResult(ctx, &WorkerResult{
		Path:       "docs/report.xlsx",
		ActionType: ActionDownload,
		Success:    true,
	}, nil, nil)

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

// Validates: R-6.8.15
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
		wantScope string
	}{
		{
			name:      "success",
			result:    WorkerResult{Success: true},
			wantClass: resultSuccess,
			wantScope: "",
		},
		{
			name:      "context_canceled",
			result:    WorkerResult{Err: context.Canceled},
			wantClass: resultShutdown,
			wantScope: "",
		},
		{
			name:      "context_deadline_exceeded",
			result:    WorkerResult{Err: context.DeadlineExceeded},
			wantClass: resultShutdown,
			wantScope: "",
		},
		{
			name:      "wrapped_context_canceled",
			result:    WorkerResult{Err: fmt.Errorf("operation failed: %w", context.Canceled)},
			wantClass: resultShutdown,
			wantScope: "",
		},
		{
			name:      "401_unauthorized",
			result:    WorkerResult{HTTPStatus: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
			wantClass: resultFatal,
			wantScope: "",
		},
		{
			name:      "403_forbidden",
			result:    WorkerResult{HTTPStatus: http.StatusForbidden, Err: graph.ErrForbidden},
			wantClass: resultSkip,
			wantScope: "",
		},
		{
			name:      "404_not_found",
			result:    WorkerResult{HTTPStatus: http.StatusNotFound, Err: graph.ErrNotFound},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "408_request_timeout",
			result:    WorkerResult{HTTPStatus: http.StatusRequestTimeout, Err: errors.New("timeout")},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "412_precondition_failed",
			result:    WorkerResult{HTTPStatus: http.StatusPreconditionFailed, Err: errors.New("etag mismatch")},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "423_locked",
			result:    WorkerResult{HTTPStatus: http.StatusLocked, Err: graph.ErrLocked},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "429_too_many_requests",
			result:    WorkerResult{HTTPStatus: http.StatusTooManyRequests, Err: graph.ErrThrottled},
			wantClass: resultScopeBlock,
			wantScope: "throttle:account",
		},
		{
			name:      "400_outage_pattern",
			result:    WorkerResult{HTTPStatus: http.StatusBadRequest, Err: outageErr},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "400_normal",
			result:    WorkerResult{HTTPStatus: http.StatusBadRequest, Err: normalBadRequestErr},
			wantClass: resultSkip,
			wantScope: "",
		},
		{
			name:      "500_internal_server_error",
			result:    WorkerResult{HTTPStatus: http.StatusInternalServerError, Err: graph.ErrServerError},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "502_bad_gateway",
			result:    WorkerResult{HTTPStatus: http.StatusBadGateway, Err: graph.ErrServerError},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "503_service_unavailable",
			result:    WorkerResult{HTTPStatus: http.StatusServiceUnavailable, Err: graph.ErrServerError},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "504_gateway_timeout",
			result:    WorkerResult{HTTPStatus: http.StatusGatewayTimeout, Err: graph.ErrServerError},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name:      "509_bandwidth_limit",
			result:    WorkerResult{HTTPStatus: 509, Err: graph.ErrServerError},
			wantClass: resultRequeue,
			wantScope: "",
		},
		{
			name: "507_own_drive",
			result: WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "",
			},
			wantClass: resultScopeBlock,
			wantScope: "quota:own",
		},
		{
			name: "507_shortcut_drive",
			result: WorkerResult{
				HTTPStatus:  http.StatusInsufficientStorage,
				Err:         errors.New("insufficient storage"),
				ShortcutKey: "drive1:item1",
			},
			wantClass: resultScopeBlock,
			wantScope: "quota:shortcut:drive1:item1",
		},
		{
			name:      "409_conflict",
			result:    WorkerResult{HTTPStatus: http.StatusConflict, Err: graph.ErrConflict},
			wantClass: resultSkip,
			wantScope: "",
		},
		{
			name:      "other_4xx_falls_to_skip",
			result:    WorkerResult{HTTPStatus: http.StatusMethodNotAllowed, Err: graph.ErrMethodNotAllowed},
			wantClass: resultSkip,
			wantScope: "",
		},
		{
			name:      "os_err_permission",
			result:    WorkerResult{Err: os.ErrPermission},
			wantClass: resultSkip,
			wantScope: "",
		},
		{
			name:      "wrapped_os_err_permission",
			result:    WorkerResult{Err: fmt.Errorf("cannot write: %w", os.ErrPermission)},
			wantClass: resultSkip,
			wantScope: "",
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
// policy via sync_failures + FailureRetrier. See internal/retry/named_test.go.
