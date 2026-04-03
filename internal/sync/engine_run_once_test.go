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
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

	_, err := NewEngine(t.Context(), &synctypes.EngineConfig{
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

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, err, "RunOnce")

	assert.Equal(t, synctypes.SyncBidirectional, report.Mode)

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
	eng.localRules = synctypes.LocalObservationRules{RejectSharePointRootForms: true}
	writeLocalFile(t, syncRoot, "forms", "reserved root name")

	report, err := eng.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, err, "RunOnce")
	assert.Equal(t, 0, report.Uploads, "reserved SharePoint root names must not produce upload actions")

	failures, err := eng.baseline.ListActionableFailures(t.Context())
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "forms", failures[0].Path)
	assert.Equal(t, synctypes.IssueInvalidFilename, failures[0].IssueType)
	assert.Equal(t, synctypes.CategoryActionable, failures[0].Category)
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

	report, err := eng.RunOnce(ctx, synctypes.SyncDownloadOnly, synctypes.RunOpts{})
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

	_, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
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

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{DryRun: true})
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
	// entries as deleted → EF6 → synctypes.ActionRemoteDelete. With threshold=10,
	// 20 remote deletes > 10 → synctypes.ErrBigDeleteTriggered.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.bigDeleteThreshold = 10 // low threshold for test
	ctx := t.Context()

	seedOutcomes := make([]synctypes.Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("item-%02d", i),
			ItemType:        synctypes.ItemTypeFile,
			RemoteHash:      fmt.Sprintf("hash%02d", i),
			LocalHash:       fmt.Sprintf("hash%02d", i),
			LocalSize:       100,
			LocalSizeKnown:  true,
			RemoteSize:      100,
			RemoteSizeKnown: true,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	_, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
	assert.ErrorIs(t, err, synctypes.ErrBigDeleteTriggered)
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

	seedOutcomes := make([]synctypes.Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("item-%02d", i),
			ItemType:        synctypes.ItemTypeFile,
			RemoteHash:      fmt.Sprintf("hash%02d", i),
			LocalHash:       fmt.Sprintf("hash%02d", i),
			LocalSize:       100,
			LocalSizeKnown:  true,
			RemoteSize:      100,
			RemoteSizeKnown: true,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	report, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{Force: true})
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

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	_, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	_, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Verify delta token was saved.
	token, err := eng.baseline.GetDeltaToken(ctx, engineTestDriveID, "")
	require.NoError(t, err, "GetDeltaToken")
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

	_, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	_, err := NewEngine(t.Context(), &synctypes.EngineConfig{
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
	seedOutcomes := []synctypes.Outcome{{
		Action:  synctypes.ActionDownload,
		Success: true,
		Path:    "seed.txt",
		DriveID: driveID,
		ItemID:  "seed-1",
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "stale-token")

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, err, "RunOnce")

	// Delta should have been called twice (expired + retry).
	assert.Equal(t, 2, callCount, "delta call count")

	// Report should reflect no content changes (only root in delta).
	total := report.Downloads + report.Uploads
	assert.Equal(t, 0, total, "downloads+uploads")
}

// TestRunOnce_EmptyPlan_NoPanic verifies that when changes exist but all
// classify to no-op actions (producing an empty plan), the engine does not
// deadlock. Regression test for: empty plan caused syncdispatch.NewDepGraph with total=0,
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
	seedOutcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "unchanged.txt",
		DriveID:         driveID,
		ItemID:          "f1",
		ItemType:        synctypes.ItemTypeFile,
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
	var report *synctypes.SyncReport
	var runErr error

	go func() {
		report, runErr = eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, 'item-dl', '/downloading.txt', 'file', 'downloading', ?),
		       (?, 'item-del', '/deleting.txt', 'file', 'deleting', ?)`,
		engineTestDriveID, now, engineTestDriveID, now)
	require.NoError(t, err, "seed in-progress rows")

	// RunOnce should reset these at startup.
	_, runErr := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, runErr, "RunOnce")

	// Verify the states were reset.
	var dlStatus, delStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-dl'`).Scan(&dlStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDownload, dlStatus, "downloading should be reset")

	// deleting → deleted because the file doesn't exist on disk (crash
	// recovery checks filesystem to determine target state).
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-del'`).Scan(&delStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, delStatus, "deleting with no local file should be marked deleted")
}

// Validates: R-2.10.41
func TestRunOnce_CrashRecovery_MixedDeletingCandidates(t *testing.T) {
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

	writeLocalFile(t, syncRoot, "exists.txt", "still here")

	now := time.Now().Unix()
	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, sync_status, observed_at)
		VALUES (?, 'gone', '/gone.txt', 'file', 'deleting', ?),
		       (?, 'exists', '/exists.txt', 'file', 'deleting', ?),
		       (?, 'bad', '/../bad.txt', 'file', 'deleting', ?)`,
		engineTestDriveID, now,
		engineTestDriveID, now,
		engineTestDriveID, now,
	)
	require.NoError(t, err, "seed crash-recovery rows")

	_, runErr := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.NoError(t, runErr, "RunOnce")

	var goneStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'gone'`).Scan(&goneStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, goneStatus)

	var existsStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'exists'`).Scan(&existsStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, existsStatus)

	var badStatus synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'bad'`).Scan(&badStatus)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusPendingDelete, badStatus)
}

func TestResolveSafetyConfig_Default(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: synctypes.DefaultBigDeleteThreshold}
	cfg := eng.resolveSafetyConfig(false, false)

	assert.Equal(t, synctypes.DefaultBigDeleteThreshold, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_Force(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: synctypes.DefaultBigDeleteThreshold}
	cfg := eng.resolveSafetyConfig(true, false)

	assert.Equal(t, forceSafetyMax, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_UsesConfiguredThreshold(t *testing.T) {
	t.Parallel()

	// Verify the config bug is fixed: engine uses the configured threshold,
	// not a hardcoded default.
	eng := &Engine{bigDeleteThreshold: 500}
	cfg := eng.resolveSafetyConfig(false, false)

	assert.Equal(t, 500, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_WatchMode_DisablesThreshold(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: 500}
	cfg := eng.resolveSafetyConfig(false, true)

	assert.Equal(t, forceSafetyMax, cfg.BigDeleteThreshold,
		"watch mode should disable planner-level big-delete threshold")
}
