package sync

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-2.10.5
func TestEngine_CascadeRecordAndComplete_RecordsBlockedRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKQuotaOwn()

	root := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: driveid.New("drive1"),
	}, 1, nil)
	require.NotNil(t, root)

	testEngineFlow(t, eng).scopeController().cascadeRecordAndComplete(t.Context(), root, scopeKey)

	assert.Equal(t, 0, rt.depGraph.InFlightCount())
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "blocked.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestEngine_CascadeRecordAndComplete_CascadesToDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKQuotaOwn()

	root := rt.depGraph.Add(&Action{
		Type:    ActionFolderCreate,
		Path:    "dir",
		DriveID: driveid.New("drive1"),
	}, 1, nil)
	require.NotNil(t, root)

	child := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveid.New("drive1"),
	}, 2, []int64{1})
	assert.Nil(t, child)

	testEngineFlow(t, eng).scopeController().cascadeRecordAndComplete(t.Context(), root, scopeKey)

	assert.Equal(t, 0, rt.depGraph.InFlightCount())
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 2)
	assert.ElementsMatch(t, []string{"dir", "dir/file.txt"}, []string{retryRows[0].Path, retryRows[1].Path})
}

// Validates: R-2.10.5
func TestScopeController_RecordCascadeRetryWork_RetryableTransientScopeEvidenceStaysUnblockedUntilScopeActivates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()

	controller.recordCascadeRetryWork(t.Context(), rt, &Action{
		Type:    ActionUpload,
		Path:    "child.txt",
		DriveID: eng.driveID,
	}, &ActionCompletion{
		Path:       "parent.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		ErrMsg:     "temporary outage",
	})

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "child.txt", retryRows[0].Path)
	assert.Equal(t, SKService(), retryRows[0].ScopeKey)
	assert.False(t, retryRows[0].Blocked)
	assert.NotZero(t, retryRows[0].NextRetryAt)
}

// Validates: R-2.10.5
func TestScopeController_ApplyTrialPreserveEffects_RehomesDiskScopeRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()

	controller.applyTrialPreserveEffects(t.Context(), rt, &ResultDecision{
		Class:    errclass.ClassBlockScopeingTransient,
		ScopeKey: SKDiskLocal(),
	}, &ActionCompletion{
		Path:       "disk.txt",
		ActionType: ActionUpload,
		ErrMsg:     "disk full",
	}, nil)

	assert.True(t, isTestBlockScopeed(eng, SKDiskLocal()))
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "disk.txt", retryRows[0].Path)
	assert.Equal(t, SKDiskLocal(), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestScopeController_ClearBlockedRetryWorkForScope_RemovesScopedRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKService()

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "blocked.txt",
		ActionType:    ActionUpload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		LastError:     "blocked retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	controller.clearBlockedRetryWorkForScope(t.Context(), RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, scopeKey)

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestScopeController_AdmitReady_BlocksNormalActionUnderActiveScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKQuotaOwn()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		ConditionType: IssueQuotaExceeded,
		TimingSource:  ScopeTimingNone,
	})

	ready := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	dispatched := controller.admitReady(t.Context(), rt, []*TrackedAction{ready})

	assert.Empty(t, dispatched)
	assert.Zero(t, rt.depGraph.InFlightCount())

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "blocked.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestScopeController_AdmitReady_TrialCandidateClearsStaleBlockedRetryWhenScopeNoLongerMatches(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKQuotaOwn()

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "trial.txt",
		ActionType:    ActionDownload,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "stale blocked retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	ready := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched := controller.admitReady(t.Context(), rt, []*TrackedAction{ready})

	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(1), dispatched[0].ID)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestScopeController_AdmitReady_TrialCandidateStillMatchingScopeDispatchesWithoutClearingRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKQuotaOwn()

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		ConditionType: scopeKey.ConditionType(),
		ScopeKey:      scopeKey,
		LastError:     "blocked retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	ready := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched := controller.admitReady(t.Context(), rt, []*TrackedAction{ready})

	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(1), dispatched[0].ID)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "trial.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestRemoteWriteBlockedRetryRelevant_MatchesPathAndBoundaryChanges(t *testing.T) {
	t.Parallel()

	row := &RetryWorkRow{
		Path:       "Shared/Docs/report.txt",
		ActionType: ActionUpload,
		ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
		Blocked:    true,
	}

	assert.True(t, remoteWriteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs": true}))
	assert.True(t, remoteWriteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs/subdir": true}))
	assert.True(t, remoteWriteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs/report.txt": true}))
	assert.False(t, remoteWriteBlockedRetryRelevant(row, map[string]bool{"Elsewhere": true}))
	assert.False(t, remoteWriteBlockedRetryRelevant(&RetryWorkRow{
		Path:       row.Path,
		ActionType: row.ActionType,
		ScopeKey:   SKService(),
		Blocked:    true,
	}, map[string]bool{"Shared/Docs": true}))

	assert.True(t, scopePathMatches("Shared/Docs/report.txt", "Shared/Docs"))
	assert.False(t, scopePathMatches("Shared/Other", "Shared/Docs"))
}

// Validates: R-2.10.5
func TestScopeController_RunPermissionMaintenance_LocalObservationKeepsRemotePermissionScopeUntilProbeRelease(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	resolvedScope := SKPermRemoteWrite("Shared/Resolved")
	retainedScope := SKPermRemoteWrite("Shared/Retained")

	setTestBlockScope(t, eng, &BlockScope{
		Key:           resolvedScope,
		ConditionType: IssueRemoteWriteDenied,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(time.Minute),
		TrialInterval: time.Minute,
	})
	setTestBlockScope(t, eng, &BlockScope{
		Key:           retainedScope,
		ConditionType: IssueRemoteWriteDenied,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(time.Minute),
		TrialInterval: time.Minute,
	})

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "Shared/Resolved/file.txt",
		ActionType:    ActionRemoteDelete,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      resolvedScope,
		LastError:     "blocked resolved retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "Shared/Retained/file.txt",
		ActionType:    ActionRemoteDelete,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      retainedScope,
		LastError:     "blocked retained retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, eng.baseline.CommitMutation(t.Context(), &BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "Shared/Retained/file.txt",
		DriveID:         eng.driveID,
		ItemID:          "item-retained",
		ItemType:        ItemTypeFile,
		LocalHash:       "hash",
		RemoteHash:      "hash",
		LocalSize:       12,
		LocalSizeKnown:  true,
		RemoteSize:      12,
		RemoteSizeKnown: true,
		LocalMtime:      1700000000,
		RemoteMtime:     1700000000,
	}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	controller.runPermissionMaintenance(t.Context(), nil, bl, permissionMaintenanceRequest{
		Reason:       permissionMaintenanceLocalObservation,
		ChangedPaths: map[string]bool{"Shared": true},
	})

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/Retained/file.txt", retryRows[0].Path)
	assert.True(t, isTestBlockScopeed(eng, resolvedScope))
	assert.True(t, isTestBlockScopeed(eng, retainedScope))
}

// Validates: R-2.10.5
func TestScopeController_RunPermissionMaintenance_StartupClearsResolvedRemoteWriteBlockedRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKPermRemoteWrite("Shared/Resolved")

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		ConditionType: IssueRemoteWriteDenied,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(time.Minute),
		TrialInterval: time.Minute,
	})

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "Shared/Resolved/file.txt",
		ActionType:    ActionRemoteDelete,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      scopeKey,
		LastError:     "blocked resolved retry",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	ph := &PermissionHandler{
		store:  eng.baseline,
		nowFn:  eng.nowFn,
		logger: newTestLogger(t),
	}
	eng.permHandler = ph

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	controller.runPermissionMaintenance(t.Context(), nil, bl, permissionMaintenanceRequest{
		Reason: permissionMaintenanceStartup,
	})

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.True(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestScopeController_NormalizePersistedScopes_LeavesPermissionScopesForPermissionMaintenance(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKPermLocalWrite("restored")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		ConditionType: IssueLocalWriteDenied,
		TimingSource:  ScopeTimingNone,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
	}))

	require.NoError(t, controller.normalizePersistedScopes(t.Context(), nil))
	assert.True(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestScopeController_NormalizeDiskScope_UsesCurrentDiskState(t *testing.T) {
	t.Parallel()

	t.Run("disabled minimum space releases stale scope", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		controller := testEngineFlow(t, eng).scopeController()
		eng.minFreeSpace = 0
		block := &BlockScope{
			Key:           SKDiskLocal(),
			ConditionType: IssueDiskFull,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     eng.nowFn().Add(-time.Minute),
			TrialInterval: time.Minute,
			NextTrialAt:   eng.nowFn().Add(time.Minute),
		}

		require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), block))
		require.NoError(t, controller.normalizeDiskScope(t.Context(), block))

		assert.False(t, isTestBlockScopeed(eng, SKDiskLocal()))
	})

	t.Run("insufficient space refreshes persisted interval", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		controller := testEngineFlow(t, eng).scopeController()
		eng.minFreeSpace = 100
		eng.diskAvailableFn = func(string) (uint64, error) { return 10, nil }
		block := &BlockScope{
			Key:           SKDiskLocal(),
			ConditionType: IssueDiskFull,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     eng.nowFn().Add(-time.Minute),
		}

		require.NoError(t, controller.normalizeDiskScope(t.Context(), block))

		blocks, err := eng.baseline.ListBlockScopes(t.Context())
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		assert.Equal(t, SKDiskLocal(), blocks[0].Key)
		assert.Equal(t, scopeStartupRevalidateDisk, scopeStartupPolicyFor(SKDiskLocal()))
	})
}
