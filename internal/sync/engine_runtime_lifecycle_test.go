package sync

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The line said when a block scope of each condition starts. The groups must
// name every declared member, so a new condition is worded on purpose, and
// its level is pinned: one that clears without the user (throttling, an
// outage) is Info, and a default that warned for it would read as something
// to act on.
// Validates: R-6.6.30
func TestBlockScopeActivationLine_EveryMemberIsDecided(t *testing.T) {
	t.Parallel()

	type scopeLine struct{ clearsAutomatically, generic bool }
	const retryIn = "1m"
	key := syncdomain.SKPermLocalWrite("Docs")
	lineFor := func(conditionType syncdomain.IssueType) scopeLine {
		line, clearsAutomatically := blockScopeActivationLine(key, conditionType, retryIn)
		assert.Contains(t, line, retryIn, "%q names when it is checked again", conditionType)

		return scopeLine{
			clearsAutomatically: clearsAutomatically,
			generic:             strings.Contains(line, "("+strings.ReplaceAll(string(conditionType), "_", " ")+")"),
		}
	}

	syncfixture.RequireEveryIssueTypeDecided(t, syncfixture.IssueTypeVerdicts(t,
		syncfixture.IssueTypeVerdictGroup[scopeLine]{
			Verdict: scopeLine{clearsAutomatically: true},
			Members: []syncdomain.IssueType{syncdomain.IssueRateLimited, syncdomain.IssueServiceOutage},
		},
		syncfixture.IssueTypeVerdictGroup[scopeLine]{
			// Worded for what the user can do about it.
			Verdict: scopeLine{},
			Members: []syncdomain.IssueType{syncdomain.IssueQuotaExceeded, syncdomain.IssueDiskFull, syncdomain.IssueLocalReadOnly},
		},
		syncfixture.IssueTypeVerdictGroup[scopeLine]{
			Verdict: scopeLine{generic: true},
			Members: []syncdomain.IssueType{
				syncdomain.IssueInvalidFilename, syncdomain.IssuePathTooLong, syncdomain.IssueFileTooLarge, syncdomain.IssueHashError,
				syncdomain.IssueUnauthorized, syncdomain.IssueRemoteWriteDenied, syncdomain.IssueRemoteReadDenied, syncdomain.IssueRemoteRootNotFound, syncdomain.IssueLocalReadDenied,
				syncdomain.IssueLocalWriteDenied, syncdomain.IssueCaseCollision, syncdomain.IssueRemoteNameTwin, syncdomain.IssueFileTooLargeForSpace,
				syncdomain.IssueLocalNameTooLong, syncdomain.IssueDeletesHeld, syncdomain.IssueLocalUnsyncableReplacement, syncdomain.IssueLocalMountBoundary,
				syncdomain.IssueLocalMountMissing, syncdomain.IssueRemoteFolderUnverifiable, syncdomain.IssueLocalInterruptedRemoval, syncdomain.IssueRemovedFolderKept, syncdomain.IssueLocalFileTooLarge, syncdomain.IssueLocalBusy,
				syncdomain.IssueLocalCrossDevice, syncdomain.IssueUnsupportedOperation, syncdomain.IssueMalwareDetected, syncdomain.IssueLocalDatabaseOpen,
				syncdomain.IssueUploadVerification, syncdomain.IssueDownloadIntegrity, syncdomain.IssueRequestTimeout, syncdomain.IssueTransientConflict,
				syncdomain.IssueTransientNotFound, syncdomain.IssueResourceLocked, syncdomain.IssueTransferURLRejected,
			},
		},
	), lineFor)

	assert.Equal(t, scopeLine{generic: true}, lineFor(syncfixture.IssueTypeOutsideTheEnum), "a value outside the enum")
}

// The block-scope line's level is the family's verdict, the one the watch
// condition summary reads: an issue type whose family clears automatically
// is said at Info, every other at Warn.
// Validates: R-6.6.30
func TestBlockScopeActivationLine_AgreesWithTheFamilyVerdict(t *testing.T) {
	t.Parallel()

	const retryIn = "1m"
	for _, member := range syncfixture.IssueTypeConstantsInSource(t) {
		_, clearsAutomatically := blockScopeActivationLine(syncdomain.SKService(), member, retryIn)
		key, hasFamily := syncdomain.ConditionKeyForIssueType(member)
		assert.Equal(t, hasFamily && syncdomain.ConditionClearsAutomatically(key), clearsAutomatically, "%q", member)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_RecordBlockedRetryWork_PersistsOnlyExactBlockedRoot(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := syncdomain.SKQuotaOwn()
	flow := testEngineFlow(t, eng)

	root := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionFolderCreate,
		Path:    "dir",
		DriveID: driveid.New("drive1"),
	}, 1, nil)
	require.NotNil(t, root)

	child := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveid.New("drive1"),
	}, 2, []int64{1})
	assert.Nil(t, child)

	seedBlockScopeRowForTest(t, eng, scopeKey)
	require.NoError(t, flow.recordBlockedRetryWork(t.Context(), &root.Action, scopeKey))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "dir", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
	assert.Equal(t, 2, rt.sched.InFlight(), "root and dependent both remain in the graph until the exact blocked root is released")
}

// Validates: R-2.10.5
func TestEngineFlow_PersistBlockedRetryWork_CanonicalizesRowsAcrossRuntimePaths(t *testing.T) {
	t.Parallel()

	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	type persistCase struct {
		name    string
		persist func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime)
	}

	normalize := func(row syncdomain.RetryWorkRow) syncdomain.RetryWorkRow {
		row.AttemptCount = 0
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
			name:    "permission evidence",
			persist: persistBlockedRetryViaPermissionEvidence(scopeKey),
		},
	}

	want := normalize(syncdomain.RetryWorkRow{
		Path:       "Shared/Docs/file.txt",
		OldPath:    "Shared/Docs/old.txt",
		ActionType: syncdomain.ActionUpload,
		ScopeKey:   scopeKey,
		Blocked:    true,
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := newSingleOwnerEngine(t)
			rt := testWatchRuntime(t, eng)
			flow := testEngineFlow(t, eng)

			tc.persist(t, eng, flow, rt)

			retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
			require.Len(t, retryRows, 1)
			got := normalize(retryRows[0])
			assert.Equal(t, scopeKey, got.ScopeKey)
			assert.True(t, got.Blocked)
			assert.Equal(t, int64(0), got.NextRetryAt)
			assert.Equal(t, want, got)
		})
	}
}

func persistBlockedRetryViaAdmission(scopeKey syncdomain.ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		seedBlockScopeRowForTest(t, eng, scopeKey)
		action := &syncdomain.Action{
			Type:    syncdomain.ActionUpload,
			Path:    "Shared/Docs/file.txt",
			OldPath: "Shared/Docs/old.txt",
			DriveID: driveid.New("drive1"),
		}
		require.NoError(t, flow.recordBlockedRetryWork(t.Context(), action, scopeKey))
	}
}

func persistBlockedRetryViaHeldScope(scopeKey syncdomain.ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		seedBlockScopeRowForTest(t, eng, scopeKey)
		current := rt.sched.AddForTest(&syncdomain.Action{
			Type:    syncdomain.ActionUpload,
			Path:    "Shared/Docs/file.txt",
			OldPath: "Shared/Docs/old.txt",
			DriveID: driveid.New("drive1"),
		}, 1, nil)
		require.NotNil(t, current)
		require.NoError(t, flow.holdActionUnderScope(t.Context(), current, &syncdomain.ActionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: syncdomain.ActionUpload,
			HTTPStatus: 403,
		}, scopeKey))
	}
}

func persistBlockedRetryViaTrialRehome(scopeKey syncdomain.ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		seedBlockScopeRowForTest(t, eng, scopeKey)
		require.NoError(t, flow.rehomeBlockedRetryWork(t.Context(), &syncdomain.ActionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: syncdomain.ActionUpload,
			HTTPStatus: 403,
		}, scopeKey))
	}
}

func persistBlockedRetryViaPermissionEvidence(scopeKey syncdomain.ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		err := flow.applyPermissionFailureEvidence(t.Context(), nil, &syncdomain.ActionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: syncdomain.ActionUpload,
		}, permissionEvidence{
			Kind:         permissionEvidenceBoundaryDenied,
			BoundaryPath: "Shared/Docs",
			TriggerPath:  "Shared/Docs/file.txt",
			IssueType:    syncdomain.IssueRemoteWriteDenied,
		}, true)
		require.NoError(t, err)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_ApplyTrialReclassification_RehomesDiskScopeRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	handled, err := flow.applyTrialReclassification(t.Context(), &syncdomain.ResultDecision{
		Class:    errclass.ClassScopeBlockingTransient,
		ScopeKey: syncdomain.SKDiskLocal(),
	}, &syncdomain.ActionCompletion{
		Path:       "disk.txt",
		ActionType: syncdomain.ActionUpload,
		ErrMsg:     "disk full",
	}, nil)
	require.NoError(t, err)
	assert.True(t, handled)

	assert.True(t, isTestBlockScopeed(eng, syncdomain.SKDiskLocal()))
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "disk.txt", retryRows[0].Path)
	assert.Equal(t, syncdomain.SKDiskLocal(), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5, R-2.10.33, R-2.14.1
func TestEngineFlow_ApplyTrialReclassification_LocalFilePermissionReusesPermissionEvidencePath(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKService()

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))
	syncstoretest.SeedObservationIssueForTest(t, eng.baseline, "keep.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})
	seedBlockScopeRowForTest(t, eng, scopeKey)
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "accessible/file.txt",
		ActionType:   syncdomain.ActionDownload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))

	handled, err := flow.applyTrialReclassification(t.Context(), &syncdomain.ResultDecision{}, &syncdomain.ActionCompletion{
		Path:          "accessible/file.txt",
		ActionType:    syncdomain.ActionDownload,
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
	assert.Equal(t, syncdomain.IssueInvalidFilename, observationIssues[0].IssueType)
}

// Validates: R-2.10.5
func TestEngineFlow_NormalizePersistedScopes_DiscardsEmptyScopeWithoutBlockedWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           syncdomain.SKDiskLocal(),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))

	require.NoError(t, flow.normalizePersistedScopes(t.Context()))

	assert.False(t, isTestBlockScopeed(eng, syncdomain.SKDiskLocal()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_LoadActiveScopes_PopulatesRuntimeLifecycleWorkingSet(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := syncdomain.SKQuotaOwn()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))

	require.NoError(t, rt.loadActiveScopes(t.Context()))

	assert.True(t, rt.sched.HasScope(scopeKey))
	activeScopes := rt.sched.Scopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, scopeKey, activeScopes[0].Key)
}

// Validates: R-2.10.33
func TestEngineFlow_NormalizePersistedScopes_RemovesStaleScopeAndPreservesReadyRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKDiskLocal()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "retry-now.txt",
		ActionType:   syncdomain.ActionDownload,
		ScopeKey:     scopeKey,
		Blocked:      false,
		AttemptCount: 1,
		NextRetryAt:  now.UnixNano(),
	}))

	require.NoError(t, flow.normalizePersistedScopes(t.Context()))

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
	scopeKey := syncdomain.SKService()
	seedBlockScopeRowForTest(t, eng, scopeKey)

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)

	require.NoError(t, flow.clearBlockedRetryWorkForScope(t.Context(), syncdomain.RetryWorkKey{
		Path:       "blocked.txt",
		ActionType: syncdomain.ActionUpload,
	}, scopeKey))

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_AdmitReady_BlocksNormalActionUnderActiveScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKQuotaOwn()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	})

	ready := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	dispatched, err := flow.admitReady(t.Context(), []*syncdomain.TrackedAction{ready})
	require.NoError(t, err)

	assert.Empty(t, dispatched)
	assert.Equal(t, 1, rt.sched.InFlight())

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "blocked.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
//
// A scope's trial that a higher-priority scope also blocks is held under that
// scope instead, and its work moves there; the scope it left, with no blocked
// work remaining, is discarded rather than left as dead state that fails the
// persisted invariants (found by the chaos Graph layer, D-092).
func TestEngineFlow_AdmitReady_RehomedWorkDoesNotStrandItsOldScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: syncdomain.SKService()})
	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: testThrottleScope()})
	movedKey := syncfixture.TestRetryWorkKey("moved.txt", "", syncdomain.ActionDownload)
	moved, err := eng.baseline.RecordBlockedRetryWork(t.Context(), movedKey, syncdomain.SKService(), nil)
	require.NoError(t, err)
	flow.retryWork.rows[movedKey] = *moved
	_, err = eng.baseline.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("other.txt", "", syncdomain.ActionDownload), testThrottleScope(), nil)
	require.NoError(t, err)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))

	ready := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "moved.txt",
		DriveID: eng.driveID,
		View:    &syncdomain.PathView{Path: "moved.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = syncdomain.SKService()
	require.Equal(t, testThrottleScope(), rt.sched.BlockingScope(ready), "the throttle scope outranks the service scope")

	dispatched, err := flow.admitReady(t.Context(), []*syncdomain.TrackedAction{ready})
	require.NoError(t, err)

	assert.Empty(t, dispatched)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
	assert.False(t, rt.sched.HasScope(syncdomain.SKService()), "the emptied service scope is discarded")
	for _, row := range listRetryWorkForTest(t, eng.baseline, t.Context()) {
		assert.Equal(t, testThrottleScope(), row.ScopeKey, "%s", row.Path)
		assert.True(t, row.Blocked, "%s", row.Path)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_AdmitReady_TrialCandidateClearsStaleBlockedRetryWhenScopeNoLongerMatches(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKQuotaOwn()
	seedBlockScopeRowForTest(t, eng, scopeKey)

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("trial.txt", "", syncdomain.ActionDownload), scopeKey, nil)
	require.NoError(t, err)

	ready := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		View:    &syncdomain.PathView{Path: "trial.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched, err := flow.admitReady(t.Context(), []*syncdomain.TrackedAction{ready})
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
	scopeKey := syncdomain.SKQuotaOwn()
	seedBlockScopeRowForTest(t, eng, scopeKey)

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("trial.txt", "", syncdomain.ActionUpload), scopeKey, nil)
	require.NoError(t, err)

	ready := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		View:    &syncdomain.PathView{Path: "trial.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched, err := flow.admitReady(t.Context(), []*syncdomain.TrackedAction{ready})
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
	scopeKey := syncdomain.SKQuotaOwn()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	})

	ready := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	require.NoError(t, eng.baseline.Close(t.Context()))

	dispatched, err := flow.admitReady(t.Context(), []*syncdomain.TrackedAction{ready})
	require.Error(t, err)
	require.ErrorContains(t, err, "blocked retry_work")
	assert.Empty(t, dispatched)
	assert.Zero(t, rt.sched.HeldCount(), "admission must not create in-memory held work when the blocked retry row was not durably recorded")
	assert.Equal(t, 1, rt.sched.InFlight())
}

// A stored retry time at either end of the stored range is a time: the one
// before 1678 is due and arms the retry timer at once, the one past 2262
// holds the work in the far future. Only zero means no time.
// Validates: R-2.10.33
func TestEngineFlow_HoldFromPersistedRetryState_ReadsBothEndsOfTheStoredRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stored  int64
		wantDue bool
	}{
		{name: "before the range", stored: math.MinInt64, wantDue: true},
		{name: "past the range", stored: math.MaxInt64, wantDue: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng := newSingleOwnerEngine(t)
			rt := testWatchRuntime(t, eng)
			flow := testEngineFlow(t, eng)
			held := rt.sched.AddForTest(&syncdomain.Action{Type: syncdomain.ActionUpload, Path: "retry.txt", DriveID: eng.driveID}, 1, nil)
			require.NotNil(t, held)
			work := syncdomain.RetryWorkKeyForAction(&held.Action)
			flow.retryWork.rows[work] = syncdomain.RetryWorkRow{
				Path:         work.Path,
				ActionType:   work.ActionType,
				AttemptCount: 1,
				NextRetryAt:  tc.stored,
			}

			require.NoError(t, flow.holdActionFromPersistedRetryState(held, work))

			nextRetry, timed := flow.sched.EarliestHeldRetryAt()
			require.True(t, timed, "the held retry has a time the retry timer arms for")
			assert.Equal(t, time.Unix(0, tc.stored), nextRetry)
			assert.Equal(t, tc.wantDue, flow.sched.HasDueHeldWork(eng.nowFunc()))
		})
	}
}

// Validates: R-2.10.33
func TestEngineFlow_ScopeHasBlockedRetryWork_AnswersForOneScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	seedBlockScopeRowForTest(t, eng, syncdomain.SKService())
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey("held.txt", "", syncdomain.ActionUpload), syncdomain.SKService(), nil)
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(ctx,
		&syncdomain.RetryWorkFailure{Work: syncfixture.TestRetryWorkKey("timed.txt", "", syncdomain.ActionUpload), ScopeKey: syncdomain.SKQuotaOwn()},
		func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	blocked, err := flow.scopeHasBlockedRetryWork(ctx, syncdomain.SKService())
	require.NoError(t, err)
	assert.True(t, blocked, "a blocked row under the scope")

	blocked, err = flow.scopeHasBlockedRetryWork(ctx, syncdomain.SKQuotaOwn())
	require.NoError(t, err)
	assert.False(t, blocked, "a delayed row carrying the scope as evidence is not blocked under it")

	blocked, err = flow.scopeHasBlockedRetryWork(ctx, testThrottleScope())
	require.NoError(t, err)
	assert.False(t, blocked, "no row under the scope")
}
