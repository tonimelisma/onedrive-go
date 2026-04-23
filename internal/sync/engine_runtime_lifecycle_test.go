package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-2.10.5
func TestEngineFlow_RecordBlockedRetryWork_PersistsOnlyExactBlockedRoot(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKQuotaOwn()
	flow := testEngineFlow(t, eng)

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

	require.NoError(t, flow.recordBlockedRetryWork(t.Context(), &root.Action, scopeKey))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "dir", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
	assert.Equal(t, 2, rt.depGraph.InFlightCount(), "root and dependent both remain in the graph until the exact blocked root is released")
}

// Validates: R-2.10.5
func TestEngineFlow_PersistBlockedRetryWork_CanonicalizesRowsAcrossRuntimePaths(t *testing.T) {
	t.Parallel()

	scopeKey := SKPermRemoteWrite("Shared/Docs")
	wantMessage := blockedRetryWorkMessage(scopeKey)

	type persistCase struct {
		name    string
		persist func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime)
	}

	normalize := func(row RetryWorkRow) RetryWorkRow {
		row.AttemptCount = 0
		row.FirstSeenAt = 0
		row.LastSeenAt = 0
		row.HTTPStatus = 0
		return row
	}

	cases := []persistCase{
		{
			name:    "admission",
			persist: persistBlockedRetryViaAdmission(scopeKey),
		},
		{
			name:    "hold under scope",
			persist: persistBlockedRetryViaHeldScope(scopeKey),
		},
		{
			name:    "trial rehome",
			persist: persistBlockedRetryViaTrialRehome(scopeKey),
		},
		{
			name:    "permission outcome",
			persist: persistBlockedRetryViaPermissionOutcome(scopeKey),
		},
	}

	var want RetryWorkRow
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newSingleOwnerEngine(t)
			rt := testWatchRuntime(t, eng)
			flow := testEngineFlow(t, eng)

			tc.persist(t, eng, flow, rt)

			retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
			require.Len(t, retryRows, 1)
			got := normalize(retryRows[0])
			assert.Equal(t, scopeKey.ConditionType(), got.ConditionType)
			assert.Equal(t, scopeKey, got.ScopeKey)
			assert.True(t, got.Blocked)
			assert.Equal(t, wantMessage, got.LastError)
			assert.Equal(t, int64(0), got.NextRetryAt)

			if i == 0 {
				want = got
				return
			}
			assert.Equal(t, want, got)
		})
	}
}

func persistBlockedRetryViaAdmission(scopeKey ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		action := &Action{
			Type:    ActionUpload,
			Path:    "Shared/Docs/file.txt",
			OldPath: "Shared/Docs/old.txt",
			DriveID: driveid.New("drive1"),
		}
		require.NoError(t, flow.recordBlockedRetryWork(t.Context(), action, scopeKey))
	}
}

func persistBlockedRetryViaHeldScope(scopeKey ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		current := rt.depGraph.Add(&Action{
			Type:    ActionUpload,
			Path:    "Shared/Docs/file.txt",
			OldPath: "Shared/Docs/old.txt",
			DriveID: driveid.New("drive1"),
		}, 1, nil)
		require.NotNil(t, current)
		require.NoError(t, flow.holdActionUnderScope(t.Context(), nil, current, &ActionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: ActionUpload,
			HTTPStatus: 403,
		}, scopeKey))
	}
}

func persistBlockedRetryViaTrialRehome(scopeKey ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		require.NoError(t, flow.rehomeBlockedRetryWork(t.Context(), &ActionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: ActionUpload,
			HTTPStatus: 403,
		}, scopeKey))
	}
}

func persistBlockedRetryViaPermissionOutcome(scopeKey ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		matched, err := flow.applyPermissionOutcome(t.Context(), nil, permissionFlowRemote403, &PermissionOutcome{
			Matched:      true,
			Kind:         permissionOutcomeActivateDerivedScope,
			ScopeKey:     scopeKey,
			BoundaryPath: "Shared/Docs",
			TriggerPath:  "Shared/Docs/file.txt",
			RetryWorkFailure: &RetryWorkFailure{
				Path:          "Shared/Docs/file.txt",
				OldPath:       "Shared/Docs/old.txt",
				ActionType:    ActionUpload,
				ConditionType: IssueRemoteWriteDenied,
				ScopeKey:      scopeKey,
				LastError:     "folder is read-only",
				HTTPStatus:    403,
				Blocked:       true,
			},
		})
		require.NoError(t, err)
		require.True(t, matched)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_ApplyTrialReclassification_RehomesDiskScopeRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	handled, err := flow.applyTrialReclassification(t.Context(), rt, &ResultDecision{
		Class:    errclass.ClassBlockScopeingTransient,
		ScopeKey: SKDiskLocal(),
	}, &ActionCompletion{
		Path:       "disk.txt",
		ActionType: ActionUpload,
		ErrMsg:     "disk full",
	}, nil)
	require.NoError(t, err)
	assert.True(t, handled)

	assert.True(t, isTestBlockScopeed(eng, SKDiskLocal()))
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "disk.txt", retryRows[0].Path)
	assert.Equal(t, SKDiskLocal(), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5, R-2.10.33, R-2.14.1
func TestEngineFlow_ApplyTrialReclassification_LocalFilePermissionReusesPermissionOutcomePath(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))
	seedObservationIssueForTest(t, eng.baseline, "keep.txt", IssueInvalidFilename, ScopeKey{})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:          "accessible/file.txt",
		ActionType:    ActionDownload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		Blocked:       true,
		AttemptCount:  1,
		LastError:     "blocked",
		FirstSeenAt:   1,
		LastSeenAt:    2,
	}))

	handled, err := flow.applyTrialReclassification(t.Context(), rt, &ResultDecision{
		PermissionFlow: permissionFlowLocalPermission,
	}, &ActionCompletion{
		Path:          "accessible/file.txt",
		ActionType:    ActionDownload,
		Err:           os.ErrPermission,
		ErrMsg:        "permission denied",
		TrialScopeKey: scopeKey,
	}, nil)
	require.NoError(t, err)
	assert.True(t, handled)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "accessible/file.txt", retryRows[0].Path)
	assert.True(t, retryRows[0].ScopeKey.IsZero())
	assert.False(t, retryRows[0].Blocked)
	assert.NotZero(t, retryRows[0].NextRetryAt)

	observationIssues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, observationIssues, 1)
	assert.Equal(t, "keep.txt", observationIssues[0].Path)
	assert.Equal(t, IssueInvalidFilename, observationIssues[0].IssueType)
}

// Validates: R-2.10.5
func TestEngineFlow_NormalizePersistedScopes_DiscardsEmptyScopeWithoutBlockedWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           SKDiskLocal(),
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFn().Add(time.Minute),
	}))

	require.NoError(t, flow.normalizePersistedScopes(t.Context(), nil))

	assert.False(t, isTestBlockScopeed(eng, SKDiskLocal()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_LoadActiveScopes_PopulatesRuntimeLifecycleWorkingSet(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := rt.engineFlow
	scopeKey := SKQuotaOwn()
	now := eng.nowFn()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))

	require.NoError(t, flow.loadActiveScopes(t.Context(), rt))

	assert.True(t, rt.hasActiveScope(scopeKey))
	activeScopes := rt.snapshotActiveScopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, scopeKey, activeScopes[0].Key)
}

// Validates: R-2.10.33
func TestEngineFlow_NormalizePersistedScopes_RemovesStaleScopeAndPreservesReadyRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKDiskLocal()
	now := eng.nowFn()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		BlockedAt:     now.Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:          "retry-now.txt",
		ActionType:    ActionDownload,
		ConditionType: IssueDiskFull,
		ScopeKey:      scopeKey,
		Blocked:       false,
		AttemptCount:  1,
		NextRetryAt:   now.UnixNano(),
		LastError:     "ready retry should survive stale-scope cleanup",
		FirstSeenAt:   now.UnixNano(),
		LastSeenAt:    now.UnixNano(),
	}))

	require.NoError(t, flow.normalizePersistedScopes(t.Context(), nil))

	assert.False(t, isTestBlockScopeed(eng, scopeKey))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry-now.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.False(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestEngineFlow_ClearBlockedRetryWorkForScope_RemovesScopedRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
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

	require.NoError(t, flow.clearBlockedRetryWorkForScope(t.Context(), RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: ActionUpload,
	}, scopeKey))

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_AdmitReady_BlocksNormalActionUnderActiveScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKQuotaOwn()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFn().Add(time.Minute),
	})

	ready := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	dispatched, err := flow.admitReady(t.Context(), rt, []*TrackedAction{ready})
	require.NoError(t, err)

	assert.Empty(t, dispatched)
	assert.Equal(t, 1, rt.depGraph.InFlightCount())

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "blocked.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestEngineFlow_AdmitReady_TrialCandidateClearsStaleBlockedRetryWhenScopeNoLongerMatches(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
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

	dispatched, err := flow.admitReady(t.Context(), rt, []*TrackedAction{ready})
	require.NoError(t, err)

	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(1), dispatched[0].ID)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_AdmitReady_TrialCandidateStillMatchingScopeDispatchesWithoutClearingRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
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

	dispatched, err := flow.admitReady(t.Context(), rt, []*TrackedAction{ready})
	require.NoError(t, err)

	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(1), dispatched[0].ID)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "trial.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-6.8
func TestEngineFlow_AdmitReady_FailsClosedWhenBlockedRetryWorkPersistenceFails(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKQuotaOwn()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFn().Add(time.Minute),
	})

	ready := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	require.NoError(t, eng.baseline.Close(t.Context()))

	dispatched, err := flow.admitReady(t.Context(), rt, []*TrackedAction{ready})
	require.Error(t, err)
	require.ErrorContains(t, err, "blocked retry_work")
	assert.Empty(t, dispatched)
	assert.Empty(t, rt.heldByKey, "admission must not create in-memory held work when the blocked retry row was not durably recorded")
	assert.Equal(t, 1, rt.depGraph.InFlightCount())
}
