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
	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

	eng, err := NewEngine(t.Context(), &synctypes.EngineConfig{
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

	report, err := eng.RunOnce(t.Context(), synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, err)
	assert.False(t, deltaCalled, "drive-root delta must not be used for shared configured roots")
	assert.Equal(t, 1, folderDeltaCalls)
	assert.GreaterOrEqual(t, report.Downloads, 1)

	token, err := eng.baseline.GetDeltaToken(t.Context(), driveID.String(), "shared-root")
	require.NoError(t, err)
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

	eng, err := NewEngine(t.Context(), &synctypes.EngineConfig{
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

	report, err := eng.RunOnce(t.Context(), synctypes.SyncDownloadOnly, synctypes.RunOpts{DryRun: true})
	require.NoError(t, err)
	assert.True(t, report.DryRun)
	assert.Equal(t, 1, folderDeltaCalls)
	assert.False(t, executorCalled)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, bl.Len())

	token, err := eng.baseline.GetDeltaToken(t.Context(), driveID.String(), "shared-root")
	require.NoError(t, err)
	assert.Empty(t, token)
}

func TestRunOnce_BigDelete_HoldsDeletesDurably(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Upload-only mode with no local files → local observer sees all baseline
	// entries as deleted → EF6 → synctypes.ActionRemoteDelete. With threshold=10,
	// 20 remote deletes > 10, so the engine records durable held-delete
	// intent and executes no destructive deletes until the user approves.
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

	report, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)
	require.NotNil(t, report)
	assert.Equal(t, 0, report.RemoteDeletes, "held deletes must not execute before approval")

	held, err := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	require.Len(t, held, 20)
	for i := range held {
		assert.Equal(t, synctypes.ActionRemoteDelete, held[i].ActionType)
		assert.Equal(t, synctypes.HeldDeleteStateHeld, held[i].State)
		assert.NotEmpty(t, held[i].LastError)
	}
}

func TestRunOnce_BigDelete_ApprovedDeletesBypassHold(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Same scenario as the held-delete test, but the user has already approved
	// the durable held-delete rows. The next normal sync pass should execute
	// those deletes without requiring any CLI force flag.
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

	held := make([]synctypes.HeldDeleteRecord, 0, len(seedOutcomes))
	for i := range seedOutcomes {
		held = append(held, synctypes.HeldDeleteRecord{
			DriveID:    seedOutcomes[i].DriveID,
			ItemID:     seedOutcomes[i].ItemID,
			Path:       seedOutcomes[i].Path,
			ActionType: synctypes.ActionRemoteDelete,
			State:      synctypes.HeldDeleteStateHeld,
		})
	}
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, held))
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	report, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err, "RunOnce with approved held deletes")
	assert.GreaterOrEqual(t, report.RemoteDeletes, 1, "approved deletes should bypass big-delete hold")

	remaining, err := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	assert.Empty(t, remaining, "successful approved deletes should consume their approval rows")
}

// Validates: R-6.4.1
func TestRunOnce_BigDelete_StaleApprovalWithDifferentItemIDDoesNotBypassHold(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.bigDeleteThreshold = 10
	ctx := t.Context()

	seedOutcomes := make([]synctypes.Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("current-item-%02d", i),
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

	staleApprovals := make([]synctypes.HeldDeleteRecord, 0, len(seedOutcomes))
	for i := range seedOutcomes {
		staleApprovals = append(staleApprovals, synctypes.HeldDeleteRecord{
			DriveID:    seedOutcomes[i].DriveID,
			ItemID:     fmt.Sprintf("stale-item-%02d", i),
			Path:       seedOutcomes[i].Path,
			ActionType: synctypes.ActionRemoteDelete,
			State:      synctypes.HeldDeleteStateHeld,
		})
	}
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, staleApprovals))
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	report, err := eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)
	assert.Equal(t, 0, report.RemoteDeletes, "stale path-only approval must not authorize reused-path delete")

	approved, err := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 20, "stale approvals remain visible instead of being consumed")

	held, err := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	require.Len(t, held, 20, "current deletes are held under their current item IDs")
	for i := range held {
		assert.Contains(t, held[i].ItemID, "current-item-")
	}
}

type sharedFolderRecoveryRunOnceFixture struct {
	eng            *testEngine
	baseline       *synctypes.Baseline
	syncRoot       string
	checker        *mockPermChecker
	shortcuts      []synctypes.Shortcut
	remoteDriveID  string
	blockedPath    string
	sharedFolderID string
}

func newSharedFolderRecoveryRunOnceFixture(t *testing.T) *sharedFolderRecoveryRunOnceFixture {
	t.Helper()

	const (
		blockedPath    = "Shared/TeamDocs/sub/file.txt"
		boundaryPath   = "Shared/TeamDocs/sub"
		sharedFolderID = "folder-id"
	)

	remoteDriveID := permissionsRemoteDriveID
	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":" + sharedFolderID: {{
				ID:    "perm-1",
				Roles: []string{"read"},
			}},
			driveid.New(remoteDriveID).String() + ":root-id": {{
				ID:    "perm-2",
				Roles: []string{"write"},
			}},
		},
	}

	shortcuts := []synctypes.Shortcut{{
		ItemID:       "shortcut-1",
		RemoteDrive:  remoteDriveID,
		RemoteItem:   "root-id",
		LocalPath:    "Shared/TeamDocs",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}}
	baselineEntries := []synctypes.Outcome{
		{
			Action:   synctypes.ActionDownload,
			Success:  true,
			Path:     "Shared",
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "shared-parent-id",
			ParentID: "root",
			ItemType: synctypes.ItemTypeFolder,
		},
		{
			Action:   synctypes.ActionDownload,
			Success:  true,
			Path:     "Shared/TeamDocs",
			DriveID:  driveid.New(remoteDriveID),
			ItemID:   "root-id",
			ParentID: "root",
			ItemType: synctypes.ItemTypeFolder,
		},
		{
			Action:   synctypes.ActionDownload,
			Success:  true,
			Path:     boundaryPath,
			DriveID:  driveid.New(remoteDriveID),
			ItemID:   sharedFolderID,
			ParentID: "root-id",
			ItemType: synctypes.ItemTypeFolder,
		},
	}

	eng, bl, syncRoot := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)

	return &sharedFolderRecoveryRunOnceFixture{
		eng:            eng,
		baseline:       bl,
		syncRoot:       syncRoot,
		checker:        checker,
		shortcuts:      shortcuts,
		remoteDriveID:  remoteDriveID,
		blockedPath:    blockedPath,
		sharedFolderID: sharedFolderID,
	}
}

// Validates: R-2.10.9, R-2.10.11, R-2.14.4
func TestRunOnce_SharedFolderPermissionRecovery_AutoUploadsPreviouslyBlockedFile(t *testing.T) {
	t.Parallel()

	fixture := newSharedFolderRecoveryRunOnceFixture(t)
	ctx := t.Context()

	var (
		uploadCalls    int
		uploadDriveID  driveid.ID
		uploadParentID string
		uploadName     string
	)
	uploadMock := &engineMockClient{
		uploadFn: func(
			_ context.Context,
			driveID driveid.ID,
			parentID,
			name string,
			_ io.ReaderAt,
			size int64,
			_ time.Time,
			_ graph.ProgressFunc,
		) (*graph.Item, error) {
			uploadCalls++
			uploadDriveID = driveID
			uploadParentID = parentID
			uploadName = name

			return &graph.Item{
				ID:           "uploaded-id",
				Name:         name,
				Size:         size,
				QuickXorHash: "uploaded-hash",
			}, nil
		},
	}
	fixture.eng.execCfg.SetUploads(uploadMock)
	fixture.eng.execCfg.SetTransferMgr(driveops.NewTransferManager(
		fixture.eng.execCfg.Downloads(), fixture.eng.execCfg.Uploads(), nil, fixture.eng.logger,
	))

	writeLocalFile(t, fixture.syncRoot, fixture.blockedPath, "updated shared content")

	newTestWatchState(t, fixture.eng)
	decision := applyRemote403Decision(t, fixture.eng, ctx, fixture.baseline, fixture.blockedPath, fixture.shortcuts)
	require.True(t, decision.Matched)
	require.Equal(t, permissionCheckActivateDerivedScope, decision.Kind)
	require.Len(t, listRemoteBlockedFailures(t, fixture.eng, ctx), 1)
	clearTestWatchRuntime(fixture.eng)

	fixture.checker.perms[driveid.New(fixture.remoteDriveID).String()+":"+fixture.sharedFolderID] = []graph.Permission{{
		ID:    "perm-1",
		Roles: []string{"write"},
	}}

	report, err := fixture.eng.RunOnce(ctx, synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)

	assert.Equal(t, 1, uploadCalls, "a recovered shared-folder upload should execute exactly once during the sync pass")
	assert.Equal(t, driveid.New(fixture.remoteDriveID), uploadDriveID, "the recovered upload must target the shortcut's remote drive")
	assert.Equal(t, fixture.sharedFolderID, uploadParentID, "the recovered upload must resolve the shared folder parent from baseline")
	assert.Equal(t, "file.txt", uploadName)
	assert.Equal(t, 1, report.Uploads)
	assert.GreaterOrEqual(t, report.Succeeded, 1)
	assert.Zero(t, report.Failed)
	assert.Empty(t, report.Errors)

	remainingRemoteBlocked := listRemoteBlockedFailures(t, fixture.eng, ctx)
	assert.Empty(t, remainingRemoteBlocked, "successful automatic recovery should clear the shared-folder blocked issue")
	assert.Empty(t, fixture.eng.permHandler.DeniedPrefixes(ctx), "released shared-folder boundaries should no longer suppress writes")

	failures, listErr := fixture.eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, failures, "successful recovery upload should leave no sync_failures rows behind")
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

	eng, ctx := newRunOnceFailingDownloadEngine(t)

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

func TestRunOnce_FailedActionsRemainInReportErrorsAfterSummaryLogging(t *testing.T) {
	t.Parallel()

	eng, ctx := newRunOnceFailingDownloadEngine(t)

	seedBaseline(t, eng.baseline, ctx, nil, "old-token")

	report, err := eng.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
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

	// prepareRunOnceState should reset these before one-shot planning begins.
	runner := newOneShotRunner(eng.Engine)
	runErr := runner.prepareRunOnceState(ctx)
	require.NoError(t, runErr, "prepareRunOnceState")

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

	runner := newOneShotRunner(eng.Engine)
	runErr := runner.prepareRunOnceState(ctx)
	require.NoError(t, runErr, "prepareRunOnceState")

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

// Validates: R-2.5.1, R-2.5.4, R-6.8.15
func TestRunOnce_CrashRecovery_ReplaysDownloadingStateWithoutFreshDelta(t *testing.T) {
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
		INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, size, mtime, sync_status, observed_at)
		VALUES (?, 'item-dl', 'retry-download.txt', 'file', NULL, 18, ?, 'downloading', ?)`,
		engineTestDriveID, now, now,
	)
	require.NoError(t, err, "seed downloading row")

	report, runErr := eng.RunOnce(ctx, synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 1, report.Downloads, "due crash-recovery download should be replanned in one-shot mode")

	//nolint:gosec // test-controlled tempdir path
	data, err := os.ReadFile(filepath.Join(syncRoot, "retry-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "recovered-download", string(data))

	var status synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-dl'`,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusSynced, status)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "successful one-shot retry replay should clear crash-recovery bridge rows")
}

// Validates: R-2.5.1, R-2.5.4, R-6.8.15
func TestRunOnce_CrashRecovery_ReplaysDownloadingStateWithBaselineAndMissingLocalFile(t *testing.T) {
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
	seedBaseline(t, eng.baseline, ctx, []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "retry-download.txt",
		DriveID:         driveID,
		ItemID:          "item-dl",
		ItemType:        synctypes.ItemTypeFile,
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
		INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, size, mtime, sync_status, observed_at, etag)
		VALUES (?, 'item-dl', 'retry-download.txt', 'file', ?, 18, ?, 'downloading', ?, 'etag-dl')`,
		engineTestDriveID, downloadHash, now, now,
	)
	require.NoError(t, err, "seed downloading row")

	report, runErr := eng.RunOnce(ctx, synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 1, report.Downloads, "missing local file should force crash-recovery download replay")

	//nolint:gosec // test-controlled tempdir path
	data, err := os.ReadFile(filepath.Join(syncRoot, "retry-download.txt"))
	require.NoError(t, err)
	assert.Equal(t, "recovered-download", string(data))

	var status synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-dl'`,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusSynced, status)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "successful one-shot retry replay should clear crash-recovery bridge rows")
}

// Validates: R-2.5.1, R-2.5.4, R-6.8.15
func TestRunOnce_CrashRecovery_ReplaysDeletingStateWithoutFreshDelta(t *testing.T) {
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
	seedBaseline(t, eng.baseline, ctx, []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "retry-delete.txt",
		DriveID:         driveID,
		ItemID:          "item-del",
		ItemType:        synctypes.ItemTypeFile,
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
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, ok := bl.GetByPath("retry-delete.txt")
	require.True(t, ok, "seeded baseline entry should exist for delete replay")

	now := time.Now().UnixNano()
	_, err = eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, hash, size, mtime, sync_status, observed_at)
		VALUES (?, 'item-del', 'retry-delete.txt', 'file', ?, 9, ?, 'deleting', ?)`,
		engineTestDriveID, deleteHash, now, now,
	)
	require.NoError(t, err, "seed deleting row")

	report, runErr := eng.RunOnce(ctx, synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, runErr, "RunOnce")
	assert.Equal(t, 1, report.LocalDeletes, "due crash-recovery delete should be replanned in one-shot mode")

	_, err = os.Stat(filepath.Join(syncRoot, "retry-delete.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)

	var status synctypes.SyncStatus
	err = eng.baseline.DB().QueryRowContext(ctx,
		`SELECT sync_status FROM remote_state WHERE item_id = 'item-del'`,
	).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, synctypes.SyncStatusDeleted, status)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "successful one-shot retry replay should clear crash-recovery delete bridge rows")
}

func TestResolveSafetyConfig_Default(t *testing.T) {
	t.Parallel()

	eng := &Engine{bigDeleteThreshold: synctypes.DefaultBigDeleteThreshold}
	cfg := eng.resolveSafetyConfig()

	assert.Equal(t, plannerSafetyMax, cfg.BigDeleteThreshold)
}

func TestResolveSafetyConfig_UsesConfiguredThreshold(t *testing.T) {
	t.Parallel()

	// Planner-level protection is disabled because the engine now owns the
	// durable held-delete workflow after planning.
	eng := &Engine{bigDeleteThreshold: 500}
	cfg := eng.resolveSafetyConfig()

	assert.Equal(t, plannerSafetyMax, cfg.BigDeleteThreshold)
}
