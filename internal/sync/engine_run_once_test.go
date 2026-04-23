package sync

import (
	"context"
	"database/sql"
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

func newRunOnceBidirectionalMock(driveID driveid.ID) *engineMockClient {
	return &engineMockClient{
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
		uploadFn: func(
			_ context.Context,
			_ driveid.ID,
			_ string,
			name string,
			_ io.ReaderAt,
			_ int64,
			_ time.Time,
			_ graph.ProgressFunc,
		) (*graph.Item, error) {
			return &graph.Item{
				ID: "uploaded-id", Name: name, Size: 13, QuickXorHash: "localhash1",
			}, nil
		},
	}
}

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

	failures := actionableObservationIssuesForTest(t, eng.baseline, t.Context())
	require.Len(t, failures, 1)
	assert.Equal(t, "forms", failures[0].Path)
	assert.Equal(t, IssueInvalidFilename, failures[0].IssueType)
	assert.True(t, failures[0].ScopeKey.IsZero())
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

// Validates: R-2.1.3
func TestRunOnce_DownloadOnly_PersistsStatusForDeferredOnlyPass(t *testing.T) {
	t.Parallel()

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
	assert.Equal(t, 1, report.DeferredByMode.Uploads)

	status, err := eng.baseline.ReadSyncStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Zero(t, status.LastSyncedAt, "directional passes must not persist sync status")
	assert.Zero(t, status.LastSyncDurationMs)
	assert.Zero(t, status.LastSucceededCount)
	assert.Zero(t, status.LastFailedCount)
}

// Validates: R-2.1.3
func TestRunOnce_DownloadOnly_DefersEditDeleteAutoResolveUpload(t *testing.T) {
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

	writeLocalFile(t, syncRoot, "edit-delete.txt", "baseline content")
	baselineHash := hashContentQuickXor(t, "baseline content")
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "edit-delete.txt",
		DriveID:         driveID,
		ItemID:          "item-edit-delete",
		ItemType:        ItemTypeFile,
		LocalHash:       baselineHash,
		RemoteHash:      baselineHash,
		LocalSize:       int64(len("baseline content")),
		LocalSizeKnown:  true,
		RemoteSize:      int64(len("baseline content")),
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
		ETag:            "etag-edit-delete",
	}}, "")

	writeLocalFile(t, syncRoot, "edit-delete.txt", "locally modified content")
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-current"))
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, driveID, time.Now()))

	report, err := eng.RunOnce(ctx, SyncDownloadOnly, RunOptions{})
	require.NoError(t, err, "RunOnce")

	assert.Zero(t, report.Uploads, "download-only should defer the local-wins edit-delete upload")
	assert.Equal(t, 1, report.DeferredByMode.Uploads, "download-only should report the deferred edit-delete upload")

	data, err := localpath.ReadFile(filepath.Join(syncRoot, "edit-delete.txt"))
	require.NoError(t, err)
	assert.Equal(t, "locally modified content", string(data))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	entry, ok := bl.GetByPath("edit-delete.txt")
	require.True(t, ok)
	assert.Equal(t, "item-edit-delete", entry.ItemID)
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
	mock := newRunOnceBidirectionalMock(driveID)

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

// Validates: R-2.1.3
func TestLoadCurrentInputsStageTx_ReadsSnapshotWritesFromProvidedTransaction(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	tx, err := beginPerfTx(ctx, eng.baseline.db)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()

	require.NoError(t, replaceLocalStateTx(ctx, tx, []LocalStateRow{{
		Path:            "tx-only.txt",
		ItemType:        ItemTypeFile,
		Hash:            "hash",
		Size:            4,
		Mtime:           5,
		ContentIdentity: "hash",
	}}))

	inputs, err := flow.loadCurrentInputsStageTx(ctx, eng.baseline, tx, eng.driveID)
	require.NoError(t, err)
	require.Len(t, inputs.localRows, 1)
	assert.Equal(t, "tx-only.txt", inputs.localRows[0].Path)
}

// Validates: R-2.10.33
func TestReconcileRuntimeState_PrunesRetryAndScopeState(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "keep.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		FirstSeenAt:  1,
		LastSeenAt:   2,
	}))
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "drop.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		FirstSeenAt:  3,
		LastSeenAt:   4,
	}))
	require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKService(),
		BlockedAt:     time.Unix(100, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(160, 0),
	}))
	require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKThrottleDrive(driveid.New("drive1")),
		BlockedAt:     time.Unix(200, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(260, 0),
	}))
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &RetryWorkRow{
		Path:         "blocked.txt",
		ActionType:   ActionRemoteDelete,
		ScopeKey:     SKService(),
		Blocked:      true,
		AttemptCount: 1,
		FirstSeenAt:  5,
		LastSeenAt:   6,
	}))

	err := flow.reconcileRuntimeState(ctx, &ActionPlan{
		Actions: []Action{{
			Type: ActionUpload,
			Path: "keep.txt",
		}},
	})
	require.NoError(t, err)

	retries, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, retries, 1)
	assert.Equal(t, "keep.txt", retries[0].Path)

	blocks, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.1.3, R-2.1.4
func TestRunOnce_PersistsLocalSnapshotAndConvergedSQLiteReconciliation(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := newRunOnceBidirectionalMock(driveID)

	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local.txt", "local-content")

	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	localRows, err := eng.baseline.ListLocalState(t.Context())
	require.NoError(t, err)
	require.Len(t, localRows, 1)
	assert.Equal(t, "local.txt", localRows[0].Path)
	assert.NotEmpty(t, localRows[0].Hash)
	assert.Equal(t, localRows[0].Hash, localRows[0].ContentIdentity)

	reconciliationRows, err := eng.baseline.QueryReconciliationState(t.Context())
	require.NoError(t, err)
	require.Len(t, reconciliationRows, 2)

	byPath := make(map[string]SQLiteReconciliationRow, len(reconciliationRows))
	for _, row := range reconciliationRows {
		byPath[row.Path] = row
	}

	assert.Contains(t, byPath, "local.txt")
	assert.Contains(t, byPath, "remote.txt")
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

// Validates: R-2.1.5
func TestBuildDryRunCurrentActionPlan_UsesScratchCommittedSnapshots(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "remote-preview", Name: "remote-preview.txt", ParentID: "root",
					DriveID: driveID, Size: 15, QuickXorHash: "remote-preview-hash",
				},
			}, "token-dry-run-preview"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:            "stale-local.txt",
		ItemType:        ItemTypeFile,
		Hash:            "stale-local-hash",
		Size:            5,
		Mtime:           11,
		ContentIdentity: "stale-local-hash",
	}}))
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES ('stale-remote', 'stale-remote.txt', 'file', 'stale-remote-hash', 6, ?, 'etag-stale')`,
		time.Now().UnixNano(),
	)
	require.NoError(t, err)
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-live-before-dry-run"))

	writeLocalFile(t, syncRoot, "fresh-local.txt", "fresh-local")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	result, err := flow.loadDryRunCurrentInputs(ctx, bl, false)
	require.NoError(t, err)

	localPaths := make([]string, 0, len(result.localRows))
	for i := range result.localRows {
		localPaths = append(localPaths, result.localRows[i].Path)
	}
	assert.Contains(t, localPaths, "fresh-local.txt")
	assert.NotContains(t, localPaths, "stale-local.txt")

	remotePaths := make([]string, 0, len(result.remoteRows))
	for i := range result.remoteRows {
		remotePaths = append(remotePaths, result.remoteRows[i].Path)
	}
	assert.Contains(t, remotePaths, "remote-preview.txt")

	liveLocalRows, err := eng.baseline.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, liveLocalRows, 1)
	assert.Equal(t, "stale-local.txt", liveLocalRows[0].Path)

	liveRemoteRows, err := eng.baseline.ListRemoteState(ctx)
	require.NoError(t, err)
	require.Len(t, liveRemoteRows, 1)
	assert.Equal(t, "stale-remote.txt", liveRemoteRows[0].Path)
	assert.NotContains(t, []string{liveRemoteRows[0].Path}, "remote-preview.txt")

	liveObservationIssues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, liveObservationIssues, "dry-run observation findings must stay out of the durable store")

	liveBlockScopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, liveBlockScopes, "dry-run read boundaries must stay out of block_scopes")

	savedToken := readObservationCursorForTest(t, eng.baseline, ctx, driveID.String())
	assert.Equal(t, "token-live-before-dry-run", savedToken)
}

// Validates: R-2.1.5
func TestLoadDryRunCurrentInputs_ObservationFindingsStayScratchOnly(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-dry-run-clear"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "/",
		DriveID:    driveID,
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote unreadable before dry-run",
		ScopeKey:   SKPermRemoteRead(""),
	})
	require.NoError(t, eng.baseline.UpsertBlockScope(ctx, &BlockScope{
		Key:           SKPermRemoteWrite(""),
		BlockedAt:     time.Unix(100, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(160, 0),
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	_, err = flow.loadDryRunCurrentInputs(ctx, bl, false)
	require.NoError(t, err)

	liveObservationIssues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, liveObservationIssues, 1)
	assert.Equal(t, IssueRemoteReadDenied, liveObservationIssues[0].IssueType)
	assert.Equal(t, "/", liveObservationIssues[0].Path)

	liveBlockScopes, err := eng.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, liveBlockScopes, 1)
	assert.Equal(t, SKPermRemoteWrite(""), liveBlockScopes[0].Key)
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

// Validates: R-2.1.2
func TestRunOnce_SharedConfiguredRootEnumerateStillPersistsFullReconcileCadenceWithoutToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	listCalls := 0
	mock := &engineMockClient{
		listChildrenRecursiveFn: func(_ context.Context, gotDriveID driveid.ID, folderID string) ([]graph.Item, error) {
			listCalls++
			assert.Equal(t, driveID, gotDriveID)
			assert.Equal(t, "shared-root", folderID)
			return nil, nil
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
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          testLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, eng.Close(t.Context()))
	})

	_, err = eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, listCalls)

	state, err := eng.baseline.ReadObservationState(t.Context())
	require.NoError(t, err)
	assert.Empty(t, state.Cursor)
	assert.NotZero(t, state.LastFullRemoteRefreshAt)
	assert.NotZero(t, state.NextFullRemoteRefreshAt)
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

// Validates: R-2.5.5, R-2.5.6
func TestNewEngine_RequiresResetForNonSQLiteStateDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600))

	resetErr := requireIncompatibleStoreEngineError(t, dbPath)
	assert.Equal(t, StateStoreIncompatibleReasonOpenFailed, resetErr.Reason)
}

// Validates: R-2.5.5, R-2.5.6
func TestNewEngine_RequiresResetForIncompatibleSchemaStateDB(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		CREATE TABLE baseline (
			item_id TEXT NOT NULL PRIMARY KEY,
			path TEXT NOT NULL UNIQUE
		);
		CREATE TABLE legacy_shadow (
			path TEXT NOT NULL PRIMARY KEY
		);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	resetErr := requireIncompatibleStoreEngineError(t, dbPath)
	assert.Equal(t, StateStoreIncompatibleReasonIncompatibleSchema, resetErr.Reason)
}

// Validates: R-2.5.5, R-2.5.6
func TestNewEngine_RequiresResetForUnsupportedStoreGeneration(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	createUnsupportedGenerationStateDB(t, dbPath)

	resetErr := requireIncompatibleStoreEngineError(t, dbPath)
	assert.Equal(t, StateStoreIncompatibleReasonIncompatibleSchema, resetErr.Reason)
}

func createUnsupportedGenerationStateDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)

	_, err = db.ExecContext(t.Context(), canonicalSchemaSQL)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `DROP TABLE store_metadata`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func requireIncompatibleStoreEngineError(t *testing.T, dbPath string) *StateStoreIncompatibleError {
	t.Helper()

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  t.TempDir(),
		DriveID:   driveid.New(engineTestDriveID),
		Fetcher:   &engineMockClient{},
		Items:     &engineMockClient{},
		Downloads: &engineMockClient{},
		Uploads:   &engineMockClient{},
		Logger:    testLogger(t),
	})
	require.Error(t, err)
	require.Nil(t, eng)

	var resetErr *StateStoreIncompatibleError
	require.ErrorAs(t, err, &resetErr)

	return resetErr
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
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, driveID, time.Now()))

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
// deadlock. Regression test for: empty plan left the runtime without any
// dispatchable work but the execution loop failed to recognize quiescence.
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
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-dl', 'retry-download.txt', 'file', NULL, 18, ?)`,
		now,
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
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES ('item-edit', 'remote-edit.txt', 'file', 'remote-hash-new', 19, ?, 'etag-edit-new')`,
		now,
	)
	require.NoError(t, err, "seed remote mirror edit row")
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-current"))
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, driveID, time.Now()))

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
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES ('item-dl', 'retry-download.txt', 'file', ?, 18, ?, 'etag-dl')`,
		downloadHash, now,
	)
	require.NoError(t, err, "seed remote mirror row")
	require.NoError(t, eng.baseline.CommitObservationCursor(ctx, driveID, "token-current"))
	require.NoError(t, eng.baseline.MarkFullRemoteRefresh(ctx, driveID, time.Now()))

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

func TestSyncStatusFromUpdate_RequiresEngineOwnedTimestamp(t *testing.T) {
	t.Parallel()

	status := syncStatusFromUpdate(&SyncStatusUpdate{
		Duration:  2 * time.Second,
		Succeeded: 3,
		Failed:    1,
	})
	assert.Nil(t, status)

	status = syncStatusFromUpdate(&SyncStatusUpdate{
		SyncedAt:  time.Unix(123, 456).UTC(),
		Duration:  2 * time.Second,
		Succeeded: 3,
		Failed:    1,
	})
	require.NotNil(t, status)
	assert.Equal(t, time.Unix(123, 456).UTC().UnixNano(), status.LastSyncedAt)
}
