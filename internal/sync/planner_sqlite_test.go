package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.1, R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_BuildsActionsFromSQLiteReconciliation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-folder', 'folder', 'folder', '', '', NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:            "upload.txt",
			ItemType:        ItemTypeFile,
			Hash:            "new-local",
			Size:            2,
			Mtime:           2,
			ContentIdentity: "new-local",
			ObservedAt:      1,
		},
		{
			Path:       "new-folder",
			ItemType:   ItemTypeFolder,
			ObservedAt: 1,
		},
	}))

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-upload",
			Path:     "upload.txt",
			ItemType: ItemTypeFile,
			Hash:     "old",
			Size:     1,
			Mtime:    1,
			ETag:     "etag-old",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-folder",
			Path:     "folder",
			ItemType: ItemTypeFolder,
		},
	}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		nil,
		bl,
		SyncBidirectional,
		&SafetyConfig{},
	)
	require.NoError(t, err)

	require.Len(t, plan.Actions, 3)
	byPath := make(map[string]Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}

	assert.Equal(t, ActionFolderCreate, byPath["folder"].Type)
	assert.Equal(t, CreateLocal, byPath["folder"].CreateSide)
	assert.Equal(t, ActionFolderCreate, byPath["new-folder"].Type)
	assert.Equal(t, CreateRemote, byPath["new-folder"].CreateSide)
	assert.Equal(t, ActionUpload, byPath["upload.txt"].Type)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:            "conflict.txt",
		ItemType:        ItemTypeFile,
		Hash:            "local-new",
		Size:            2,
		Mtime:           2,
		ContentIdentity: "local-new",
		ObservedAt:      1,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		nil,
		bl,
		SyncBidirectional,
		&SafetyConfig{},
	)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 2)

	assert.Equal(t, ActionConflictCopy, plan.Actions[0].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[0].Path)
	require.NotNil(t, plan.Actions[0].ConflictInfo)
	assert.Equal(t, ConflictEditEdit, plan.Actions[0].ConflictInfo.ConflictType)

	assert.Equal(t, ActionDownload, plan.Actions[1].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[1].Path)
	require.NotNil(t, plan.Actions[1].ConflictInfo)
	assert.Equal(t, ConflictEditEdit, plan.Actions[1].ConflictInfo.ConflictType)

	require.Len(t, plan.Deps, 2)
	assert.Equal(t, []int{0}, plan.Deps[1], "download should wait for the conflict copy")
}

// Validates: R-2.1.1, R-2.1.3
func TestPlannerPlanCurrentState_UsesRemoteRowDriveOwnershipForDownloadActions(t *testing.T) {
	t.Parallel()

	sharedDriveID := driveid.New("shared-drive-id")

	plan := planCurrentStateForInputs(
		t,
		[]SQLiteComparisonRow{{
			Path:           "Shared/report.txt",
			RemotePresent:  true,
			ComparisonKind: "remote_only_create",
		}},
		[]SQLiteReconciliationRow{{
			Path:               "Shared/report.txt",
			ComparisonKind:     "remote_only_create",
			ReconciliationKind: strDownload,
		}},
		nil,
		[]RemoteStateRow{{
			Path:     "Shared/report.txt",
			ItemID:   "remote-report",
			DriveID:  sharedDriveID,
			ItemType: ItemTypeFile,
			Hash:     "remote-hash",
			Size:     12,
			Mtime:    123,
		}},
		nil,
		nil,
		NewBaselineForTest(nil),
	)

	require.Len(t, plan.Actions, 1)
	assert.Equal(t, ActionDownload, plan.Actions[0].Type)
	assert.Equal(t, sharedDriveID, plan.Actions[0].DriveID)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_UploadOnlyDefersRemoteConflictResolutionWithoutConflictCopy(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:            "conflict.txt",
		ItemType:        ItemTypeFile,
		Hash:            "local-new",
		Size:            2,
		Mtime:           2,
		ContentIdentity: "local-new",
		ObservedAt:      1,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		nil,
		bl,
		SyncUploadOnly,
		&SafetyConfig{},
	)
	require.NoError(t, err)

	assert.Empty(t, plan.Actions)
	assert.Equal(t, 1, plan.DeferredByMode.Downloads)
	assert.Equal(t, 0, plan.DeferredByMode.Uploads)
}

func planCurrentStateForStore(t *testing.T, store *SyncStore) *ActionPlan {
	t.Helper()

	ctx := t.Context()
	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)
	observationIssues, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	blockScopes, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		observationIssues,
		blockScopes,
		bl,
		SyncBidirectional,
		&SafetyConfig{},
	)
	require.NoError(t, err)

	return plan
}

func planCurrentStateForInputs(
	t *testing.T,
	comparisons []SQLiteComparisonRow,
	reconciliations []SQLiteReconciliationRow,
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
	observationIssues []ObservationIssueRow,
	blockScopes []*BlockScope,
	baseline *Baseline,
) *ActionPlan {
	t.Helper()

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		observationIssues,
		blockScopes,
		baseline,
		SyncBidirectional,
		&SafetyConfig{},
	)
	require.NoError(t, err)

	return plan
}

func planForUnavailableLocalReadBoundaryDescendant(t *testing.T) *ActionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/sub/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Private",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		IssueType:  IssueLocalReadDenied,
		Error:      "directory not accessible",
		ScopeKey:   SKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryDescendant(t *testing.T) *ActionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:            "Shared/sub/file.txt",
		ItemType:        ItemTypeFile,
		Hash:            "hash",
		Size:            10,
		Mtime:           1,
		ContentIdentity: "hash",
		ObservedAt:      1,
	}}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Shared",
		DriveID:    driveID,
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote subtree unreadable",
		ScopeKey:   SKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalReadBoundaryCleanupCandidate(t *testing.T) *ActionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private-dir', 'Private/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-private-file', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Private",
		DriveID:    driveid.New(engineTestDriveID),
		ActionType: ActionFolderCreate,
		IssueType:  IssueLocalReadDenied,
		Error:      "directory not accessible",
		ScopeKey:   SKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryCleanupCandidate(t *testing.T) *ActionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared-dir', 'Shared/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-shared-file', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:       "Shared/sub",
			ItemType:   ItemTypeFolder,
			ObservedAt: 1,
		},
		{
			Path:            "Shared/sub/file.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash",
			Size:            10,
			Mtime:           1,
			ContentIdentity: "hash",
			ObservedAt:      1,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Shared",
		DriveID:    driveID,
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote subtree unreadable",
		ScopeKey:   SKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalMoveSource(t *testing.T) *ActionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]SQLiteComparisonRow{{
			Path:            "docs/source.txt",
			BaselinePresent: true,
			LocalPresent:    true,
			RemotePresent:   true,
			LocalChanged:    true,
			RemoteChanged:   false,
			ComparisonKind:  "local_move_source",
		}},
		[]SQLiteReconciliationRow{{
			Path:               "docs/source.txt",
			ComparisonKind:     "local_move_source",
			ReconciliationKind: strLocalMove,
			LocalMoveTarget:    "docs/dest.txt",
		}},
		[]LocalStateRow{{
			Path:       "docs/source.txt",
			ItemType:   ItemTypeFile,
			Hash:       "local",
			ObservedAt: 1,
		}},
		[]RemoteStateRow{{
			Path:    "docs/source.txt",
			ItemID:  "remote-source",
			DriveID: driveid.New(engineTestDriveID),
		}},
		[]ObservationIssueRow{{
			Path:      "docs/source.txt",
			IssueType: IssueLocalReadDenied,
		}},
		nil,
		NewBaselineForTest([]*BaselineEntry{{
			Path:     "docs/source.txt",
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "baseline-source",
			ItemType: ItemTypeFile,
		}}),
	)
}

func planForUnavailableRemoteMoveDestination(t *testing.T) *ActionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]SQLiteComparisonRow{{
			Path:            "Shared/dest.txt",
			BaselinePresent: true,
			LocalPresent:    true,
			RemotePresent:   true,
			LocalChanged:    false,
			RemoteChanged:   true,
			ComparisonKind:  "remote_move_dest",
		}},
		[]SQLiteReconciliationRow{{
			Path:               "Shared/dest.txt",
			ComparisonKind:     "remote_move_dest",
			ReconciliationKind: strRemoteMove,
			RemoteMoveSource:   "Shared/source.txt",
		}},
		[]LocalStateRow{{
			Path:       "Shared/dest.txt",
			ItemType:   ItemTypeFile,
			Hash:       "local",
			ObservedAt: 1,
		}},
		[]RemoteStateRow{{
			Path:    "Shared/dest.txt",
			ItemID:  "remote-dest",
			DriveID: driveid.New(engineTestDriveID),
		}},
		[]ObservationIssueRow{{
			Path:      "Shared",
			IssueType: IssueRemoteReadDenied,
			ScopeKey:  SKPermRemoteRead("Shared"),
		}},
		nil,
		NewBaselineForTest([]*BaselineEntry{{
			Path:     "Shared/dest.txt",
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "baseline-dest",
			ItemType: ItemTypeFile,
		}}),
	)
}

func assertNoActionForPath(t *testing.T, plan *ActionPlan, path string, actionType ActionType) {
	t.Helper()

	for i := range plan.Actions {
		assert.Falsef(
			t,
			plan.Actions[i].Path == path && plan.Actions[i].Type == actionType,
			"unexpected %s for %s",
			actionType,
			path,
		)
	}
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadDeniedDoesNotDeleteRemoteData(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-danger', 'danger.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-danger')`)
	require.NoError(t, err)

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-danger",
		Path:     "danger.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-danger",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "danger.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "file not accessible",
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "danger.txt", ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryDoesNotDeleteLocalData(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:            "Shared/a.txt",
		ItemType:        ItemTypeFile,
		Hash:            "hash",
		Size:            10,
		Mtime:           1,
		ContentIdentity: "hash",
		ObservedAt:      1,
	}}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Shared",
		DriveID:    driveid.New(engineTestDriveID),
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote subtree unreadable",
		ScopeKey:   SKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared/a.txt", ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundaryBlocksRemoteDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/a.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Private",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		IssueType:  IssueLocalReadDenied,
		Error:      "directory not accessible",
		ScopeKey:   SKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private/a.txt", ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryBlocksLocalDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-team-a', 'Team/a.txt', 'file', 'hash-a', 'hash-a', 10, 10, 1, 1, 'etag-team-a'),
			('item-team-b', 'Team/sub/b.txt', 'file', 'hash-b', 'hash-b', 11, 11, 2, 2, 'etag-team-b')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:            "Team/a.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-a",
			Size:            10,
			Mtime:           1,
			ContentIdentity: "hash-a",
			ObservedAt:      1,
		},
		{
			Path:            "Team/sub/b.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-b",
			Size:            11,
			Mtime:           2,
			ContentIdentity: "hash-b",
			ObservedAt:      2,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Team",
		DriveID:    driveid.New(engineTestDriveID),
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote subtree unreadable",
		ScopeKey:   SKPermRemoteRead("Team"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Team/a.txt", ActionLocalDelete)
	assertNoActionForPath(t, plan, "Team/sub/b.txt", ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundarySuppressesRemoteOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-private-dir",
			Path:     "Private",
			ItemType: ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-subdir",
			Path:     "Private/sub",
			ItemType: ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-file",
			Path:     "Private/sub/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-private",
			Size:     10,
			Mtime:    1,
			ETag:     "etag-private",
		},
	}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Private",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		IssueType:  IssueLocalReadDenied,
		Error:      "directory not accessible",
		ScopeKey:   SKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionDownload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundarySuppressesLocalOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:       "Shared",
			ItemType:   ItemTypeFolder,
			ObservedAt: 1,
		},
		{
			Path:       "Shared/sub",
			ItemType:   ItemTypeFolder,
			ObservedAt: 1,
		},
		{
			Path:            "Shared/sub/file.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-shared",
			Size:            10,
			Mtime:           1,
			ContentIdentity: "hash-shared",
			ObservedAt:      1,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "Shared",
		DriveID:    driveid.New(engineTestDriveID),
		ActionType: ActionDownload,
		IssueType:  IssueRemoteReadDenied,
		Error:      "remote subtree unreadable",
		ScopeKey:   SKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionUpload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_NewUnreadableLocalPathProducesNoActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "blocked/new.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "file not accessible",
	})

	plan := planCurrentStateForStore(t, store)
	assert.Empty(t, plan.Actions)

	observationIssues, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, observationIssues, 1)
	assert.Equal(t, "blocked/new.txt", observationIssues[0].Path)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalMoveSourceWithUnavailableTruthProducesNoActions(t *testing.T) {
	t.Parallel()

	plan := planForUnavailableLocalMoveSource(t)

	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteMoveDestinationWithUnavailableTruthProducesNoActions(t *testing.T) {
	t.Parallel()

	plan := planForUnavailableRemoteMoveDestination(t)

	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_UnavailableTruthNeverDeletesData(t *testing.T) {
	t.Parallel()

	t.Run("local read boundary keeps remote descendants from looking deleted", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalReadBoundaryDescendant(t)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionRemoteDelete)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionDownload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote read boundary keeps local descendants from looking deleted", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryDescendant(t)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionLocalDelete)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionUpload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("local unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Private/sub", ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionRemoteDelete)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Shared/sub", ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionLocalDelete)
		assert.Empty(t, plan.Actions)
	})

	t.Run("unavailable move source produces no remote move", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalMoveSource(t)

		assert.Empty(t, plan.Actions)
	})

	t.Run("unavailable remote move destination produces no local move", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteMoveDestination(t)

		assert.Empty(t, plan.Actions)
	})
}
