package sync

import (
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
		Path:       "blocked.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		ScopeKey:   scopeKey,
		LastError:  "blocked retry",
		Blocked:    true,
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
		Key:          scopeKey,
		IssueType:    IssueQuotaExceeded,
		TimingSource: ScopeTimingNone,
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
		Path:       "trial.txt",
		ActionType: ActionDownload,
		IssueType:  scopeKey.IssueType(),
		ScopeKey:   scopeKey,
		LastError:  "stale blocked retry",
		Blocked:    true,
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
		Path:       "trial.txt",
		ActionType: ActionUpload,
		IssueType:  scopeKey.IssueType(),
		ScopeKey:   scopeKey,
		LastError:  "blocked retry",
		Blocked:    true,
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
func TestRemoteBlockedRetryRelevant_MatchesPathAndBoundaryChanges(t *testing.T) {
	t.Parallel()

	row := &RetryWorkRow{
		Path:       "Shared/Docs/report.txt",
		ActionType: ActionUpload,
		ScopeKey:   SKPermRemote("Shared/Docs"),
		Blocked:    true,
	}

	assert.True(t, remoteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs": true}))
	assert.True(t, remoteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs/subdir": true}))
	assert.True(t, remoteBlockedRetryRelevant(row, map[string]bool{"Shared/Docs/report.txt": true}))
	assert.False(t, remoteBlockedRetryRelevant(row, map[string]bool{"Elsewhere": true}))
	assert.False(t, remoteBlockedRetryRelevant(&RetryWorkRow{
		Path:       row.Path,
		ActionType: row.ActionType,
		ScopeKey:   SKService(),
		Blocked:    true,
	}, map[string]bool{"Shared/Docs": true}))

	assert.True(t, pathMatchesPrefix("Shared/Docs/report.txt", "Shared/Docs"))
	assert.False(t, pathMatchesPrefix("Shared/Other", "Shared/Docs"))
}

// Validates: R-2.10.5
func TestScopeController_ClearResolvedRemoteBlockedRetryWork_ReleasesResolvedRemoteScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()
	resolvedScope := SKPermRemote("Shared/Resolved")
	retainedScope := SKPermRemote("Shared/Retained")

	setTestBlockScope(t, eng, &BlockScope{
		Key:           resolvedScope,
		IssueType:     IssueRemoteWriteDenied,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(time.Minute),
		TrialInterval: time.Minute,
	})
	setTestBlockScope(t, eng, &BlockScope{
		Key:           retainedScope,
		IssueType:     IssueRemoteWriteDenied,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(time.Minute),
		TrialInterval: time.Minute,
	})

	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:       "Shared/Resolved/file.txt",
		ActionType: ActionRemoteDelete,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   resolvedScope,
		LastError:  "blocked resolved retry",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:       "Shared/Retained/file.txt",
		ActionType: ActionRemoteDelete,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   retainedScope,
		LastError:  "blocked retained retry",
		Blocked:    true,
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

	controller.clearResolvedRemoteBlockedRetryWork(t.Context(), rt, map[string]bool{"Shared": true})

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/Retained/file.txt", retryRows[0].Path)
	assert.False(t, isTestBlockScopeed(eng, resolvedScope))
	assert.True(t, isTestBlockScopeed(eng, retainedScope))
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
			IssueType:     IssueDiskFull,
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
			Key:          SKDiskLocal(),
			IssueType:    IssueDiskFull,
			TimingSource: ScopeTimingBackoff,
			BlockedAt:    eng.nowFn().Add(-time.Minute),
		}

		require.NoError(t, controller.normalizeDiskScope(t.Context(), block))

		blocks, err := eng.baseline.ListBlockScopes(t.Context())
		require.NoError(t, err)
		require.Len(t, blocks, 1)
		assert.Equal(t, SKDiskLocal(), blocks[0].Key)
		assert.Equal(t, scopeStartupRevalidateDisk, scopeStartupPolicyFor(SKDiskLocal()))
	})
}
