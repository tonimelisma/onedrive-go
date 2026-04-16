package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewEngine_ZeroDriveID_ReturnsError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	mock := &engineMockClient{}
	logger := testLogger(t)

	_, err := newEngine(t.Context(), &engineInputs{
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

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")

	assert.Equal(t, SyncBidirectional, report.Mode)

	total := report.Downloads + report.Uploads + report.LocalDeletes +
		report.RemoteDeletes + report.FolderCreates + report.Moves +
		report.Conflicts + report.SyncedUpdates + report.Cleanups
	assert.Equal(t, 0, total, "expected zero actions")
	assert.Equal(t, 0, report.Succeeded, "succeeded")
	assert.Equal(t, 0, report.Failed, "failed")
}

// Validates: R-2.11.3, R-2.11.5
func TestRunOnce_SharePointRootFormsRecordsActionableFailure(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	eng.localRules = LocalObservationRules{RejectSharePointRootForms: true}
	writeLocalFile(t, syncRoot, "forms", "reserved root name")

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 0, report.Uploads, "reserved SharePoint root names must not produce upload actions")

	failures, err := eng.baseline.ListActionableFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "forms", failures[0].Path)
	assert.Equal(t, IssueInvalidFilename, failures[0].IssueType)
	assert.Equal(t, CategoryActionable, failures[0].Category)
}

// Validates: R-2.1.3
func TestRunOnce_DownloadOnly_ObservesLocalScanButSuppressesUploads(t *testing.T) {
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

	report, err := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
	require.NoError(t, err, "RunOnce")

	assert.Equal(t, 0, report.Uploads, "download-only mode should suppress uploads even though local scan still runs")
	assert.Equal(t, 1, report.DeferredByMode.Uploads, "download-only mode should report the deferred upload")
}

// Validates: R-2.1.4
func TestRunOnce_UploadOnly_StillObservesRemoteDelta(t *testing.T) {
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

	_, err := eng.RunOnce(ctx, SyncUploadOnly, RunOptions{})
	require.NoError(t, err, "RunOnce")
	assert.True(t, deltaCalled, "upload-only mode should still observe remote delta")
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

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
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

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{DryRun: true})
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
	savedToken := readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String())
	assert.Empty(t, savedToken, "dry-run should not save delta token")
}

// Validates: R-2.1.1, R-3.3.12
func TestRunOnce_SharedConfiguredRootUsesScopedDeltaAndToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	deltaCalled := false
	folderDeltaCalls := 0

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalled = true
			return deltaPageWithItems(nil, "wrong-token"), nil
		},
		folderDeltaFn: func(_ context.Context, gotDriveID driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			folderDeltaCalls++
			assert.Equal(t, driveID, gotDriveID)
			assert.Equal(t, "shared-root", folderID)
			assert.Empty(t, token)

			return []graph.Item{
				{
					ID: "remote-file-1", Name: "inside.txt", ParentID: "shared-root",
					DriveID: driveID, Size: 4, QuickXorHash: "hash1",
				},
			}, "scoped-token-1", nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:          dbPath,
		SyncRoot:        syncRoot,
		DriveID:         driveID,
		RootItemID:      "shared-root",
		Fetcher:         mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          testLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	report, err := eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	assert.False(t, deltaCalled, "drive-root delta must not be used for shared configured roots")
	assert.Equal(t, 1, folderDeltaCalls)
	assert.GreaterOrEqual(t, report.Downloads, 1)

	token := readObservationCursorForTest(t, eng.baseline, t.Context(), driveID.String())
	assert.Equal(t, "scoped-token-1", token)
}

// Validates: R-2.1.5
func TestRunOnce_DryRun_SharedConfiguredRootDoesNotSaveScopedDeltaToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	executorCalled := false
	folderDeltaCalls := 0

	mock := &engineMockClient{
		folderDeltaFn: func(_ context.Context, gotDriveID driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			folderDeltaCalls++
			assert.Equal(t, driveID, gotDriveID)
			assert.Equal(t, "shared-root", folderID)
			assert.Empty(t, token)

			return []graph.Item{{
				ID:           "remote-file-1",
				Name:         "inside.txt",
				ParentID:     "shared-root",
				DriveID:      driveID,
				Size:         4,
				QuickXorHash: "hash1",
			}}, "scoped-token-dry-run", nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			executorCalled = true
			return 0, nil
		},
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:          dbPath,
		SyncRoot:        syncRoot,
		DriveID:         driveID,
		RootItemID:      "shared-root",
		Fetcher:         mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          testLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	report, err := eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.True(t, report.DryRun)
	assert.Equal(t, 1, folderDeltaCalls)
	assert.False(t, executorCalled)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, bl.Len())

	token := readObservationCursorForTest(t, eng.baseline, t.Context(), driveID.String())
	assert.Empty(t, token)
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

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
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

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.Error(t, err, "expected error from canceled context")
}

func TestRunOnce_DeltaTokenPersisted(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := newDownloadDeltaMock(
		driveID,
		&graph.Item{
			ID: "f1", Name: "file.txt", ParentID: "root",
			DriveID: driveID, Size: 5, QuickXorHash: "hash1",
		},
		"new-delta-token",
		[]byte("data"),
	)

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")

	// Verify delta token was saved.
	token := readObservationCursorForTest(t, eng.baseline, ctx, engineTestDriveID)
	assert.Equal(t, "new-delta-token", token)
}

func TestRunOnce_BaselineUpdatedAfterRun(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := newDownloadDeltaMock(
		driveID,
		&graph.Item{
			ID: "item-a", Name: "alpha.txt", ParentID: "root",
			DriveID: driveID, Size: 7, QuickXorHash: "alphahash",
		},
		"token-v2",
		[]byte("alpha!!"),
	)

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
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

	_, err := newEngine(t.Context(), &engineInputs{
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
	seedOutcomes := []ActionOutcome{{
		Action:  ActionDownload,
		Success: true,
		Path:    "seed.txt",
		DriveID: driveID,
		ItemID:  "seed-1",
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "stale-token")
	require.NoError(t, eng.baseline.MarkFullRemoteReconcile(ctx, driveID, time.Now()))

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
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
	seedOutcomes := []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "unchanged.txt",
		DriveID:         driveID,
		ItemID:          "f1",
		ItemType:        ItemTypeFile,
		RemoteHash:      "matchhash",
		LocalHash:       "matchhash",
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	// Write a matching local file so the local observer also sees no change.
	writeLocalFile(t, syncRoot, "unchanged.txt", "hello")

	// This should complete without deadlock — use a timeout to detect hangs.
	done := make(chan struct{})
	var report *Report
	var runErr error

	go func() {
		report, runErr = eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
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

	eng, ctx := newRunOnceFailingDownloadEngine(t)

	// Seed a known delta token.
	seedBaseline(t, eng.baseline, ctx, nil, "old-token")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")
	require.GreaterOrEqual(t, report.Failed, 1, "should have failures")

	// Delta token IS advanced — committed atomically with observations.
	// Failed items are tracked in remote_state, not by rolling back the token.
	token := readObservationCursorForTest(t, eng.baseline, ctx, engineTestDriveID)
	assert.Equal(t, "new-token-after-observation", token,
		"delta token should advance with observations even when actions fail")
}

func TestRunOnce_FailedActionsRemainInReportErrorsAfterSummaryLogging(t *testing.T) {
	t.Parallel()

	eng, ctx := newRunOnceFailingDownloadEngine(t)

	seedBaseline(t, eng.baseline, ctx, nil, "old-token")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")
	require.GreaterOrEqual(t, report.Failed, 1, "should have failures")
	require.NotEmpty(t, report.Errors, "report should keep raw errors after summary logging")
	assert.Contains(t, report.Errors[0].Error(), "simulated network error")
}

func newRunOnceFailingDownloadEngine(t *testing.T) (*testEngine, context.Context) {
	t.Helper()

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
	return eng, t.Context()
}

// Validates: R-2.5.1
func TestRunOnce_ReconcilesRemoteMirrorDownloadDriftWithoutFreshDelta(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("recovered-download"))
			return int64(n), err
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	now := time.Now().UnixNano()
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, observed_at)
		VALUES ('item-dl', 'retry-download.txt', 'file', NULL, 18, ?, ?)`,
		now, now,
	)
	require.NoError(t, err, "seed remote mirror row")

	report, runErr := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 1, report.Downloads, "remote mirror drift should be reconciled without a fresh delta event")

	// #nosec G304 -- test reads a fixed file name rooted in t.TempDir().
	data, err := os.ReadFile(filepath.Join(syncRoot, "retry-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "recovered-download", string(data))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	entry, ok := bl.GetByPath("retry-download.txt")
	require.True(t, ok)
	assert.Equal(t, "item-dl", entry.ItemID)
}

// Validates: R-2.1.4, R-2.5.1
func TestRunOnce_UploadOnly_ReportsDeferredRemoteMirrorDriftWithoutFreshDelta(t *testing.T) {
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

	writeLocalFile(t, syncRoot, "remote-edit.txt", "original remote edit")
	writeLocalFile(t, syncRoot, "remote-delete.txt", "original remote delete")

	editHash := hashContentQuickXor(t, "original remote edit")
	deleteHash := hashContentQuickXor(t, "original remote delete")
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{
		{
			Action:          ActionDownload,
			Success:         true,
			Path:            "remote-edit.txt",
			DriveID:         driveID,
			ItemID:          "item-edit",
			ItemType:        ItemTypeFile,
			LocalHash:       editHash,
			RemoteHash:      editHash,
			LocalSize:       int64(len("original remote edit")),
			LocalSizeKnown:  true,
			RemoteSize:      int64(len("original remote edit")),
			RemoteSizeKnown: true,
			LocalMtime:      time.Now().UnixNano(),
			RemoteMtime:     time.Now().UnixNano(),
			ETag:            "etag-edit-old",
		},
		{
			Action:          ActionDownload,
			Success:         true,
			Path:            "remote-delete.txt",
			DriveID:         driveID,
			ItemID:          "item-delete",
			ItemType:        ItemTypeFile,
			LocalHash:       deleteHash,
			RemoteHash:      deleteHash,
			LocalSize:       int64(len("original remote delete")),
			LocalSizeKnown:  true,
			RemoteSize:      int64(len("original remote delete")),
			RemoteSizeKnown: true,
			LocalMtime:      time.Now().UnixNano(),
			RemoteMtime:     time.Now().UnixNano(),
			ETag:            "etag-delete-old",
		},
	}, "")

	now := time.Now().UnixNano()
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, observed_at, etag)
		VALUES ('item-edit', 'remote-edit.txt', 'file', 'remote-hash-new', 19, ?, ?, 'etag-edit-new')`,
		now, now,
	)
	require.NoError(t, err, "seed remote mirror edit row")
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-current"))
	require.NoError(t, eng.baseline.MarkFullRemoteReconcile(ctx, driveID, time.Now()))

	report, runErr := eng.RunOnce(ctx, SyncUploadOnly, RunOptions{})
	require.NoError(t, runErr, "RunOnce")

	assert.Zero(t, report.Downloads, "upload-only should not execute remote-to-local repair")
	assert.Zero(t, report.LocalDeletes, "upload-only should not execute remote delete repair")
	assert.Equal(t, 1, report.DeferredByMode.Downloads, "upload-only should report the deferred remote edit download")
	assert.Equal(t, 1, report.DeferredByMode.LocalDeletes, "upload-only should report the deferred remote delete")

	editData, err := localpath.ReadFile(filepath.Join(syncRoot, "remote-edit.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original remote edit", string(editData))

	deleteData, err := localpath.ReadFile(filepath.Join(syncRoot, "remote-delete.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original remote delete", string(deleteData))
}

// Validates: R-2.5.1
func TestRunOnce_DownloadOnly_DoesNotOverrideLocalDeleteWhenRemoteAlsoChanged(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("recovered-download"))
			return int64(n), err
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	writeLocalFile(t, syncRoot, "retry-download.txt", "recovered-download")
	downloadHash := hashContentQuickXor(t, "recovered-download")
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "retry-download.txt",
		DriveID:         driveID,
		ItemID:          "item-dl",
		ItemType:        ItemTypeFile,
		LocalHash:       downloadHash,
		RemoteHash:      downloadHash,
		LocalSize:       18,
		LocalSizeKnown:  true,
		RemoteSize:      18,
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
		ETag:            "etag-dl",
	}}, "")
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "retry-download.txt")))

	now := time.Now().UnixNano()
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, observed_at, etag)
		VALUES ('item-dl', 'retry-download.txt', 'file', ?, 18, ?, ?, 'etag-dl')`,
		downloadHash, now, now,
	)
	require.NoError(t, err, "seed remote mirror row")
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-current"))
	require.NoError(t, eng.baseline.MarkFullRemoteReconcile(ctx, driveID, time.Now()))

	report, runErr := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 0, report.Downloads, "download-only should not auto-resolve two-sided drift")

	_, err = os.Stat(filepath.Join(syncRoot, "retry-download.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	entry, ok := bl.GetByPath("retry-download.txt")
	require.True(t, ok)
	assert.Equal(t, "etag-dl", entry.ETag)
}

// Validates: R-2.5.1
func TestRunOnce_ReconcilesRemoteDeleteDriftWithoutFreshDelta(t *testing.T) {
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

	writeLocalFile(t, syncRoot, "retry-delete.txt", "delete me")
	deleteHash := hashContentQuickXor(t, "delete me")
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "retry-delete.txt",
		DriveID:         driveID,
		ItemID:          "item-del",
		ItemType:        ItemTypeFile,
		LocalHash:       deleteHash,
		RemoteHash:      deleteHash,
		LocalSize:       9,
		LocalSizeKnown:  true,
		RemoteSize:      9,
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
		ETag:            "etag-del",
	}}, "")

	report, runErr := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 1, report.LocalDeletes, "remote delete drift should be reconciled without a fresh delta event")

	_, err := os.Stat(filepath.Join(syncRoot, "retry-delete.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, found := bl.GetByPath("retry-delete.txt")
	assert.False(t, found)
}
