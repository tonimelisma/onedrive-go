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
	key := SKPermLocalWrite("Docs")
	lineFor := func(conditionType IssueType) scopeLine {
		line, clearsAutomatically := blockScopeActivationLine(key, conditionType, retryIn)
		assert.Contains(t, line, retryIn, "%q names when it is checked again", conditionType)

		return scopeLine{
			clearsAutomatically: clearsAutomatically,
			generic:             strings.Contains(line, "("+strings.ReplaceAll(string(conditionType), "_", " ")+")"),
		}
	}

	requireEveryIssueTypeDecided(t, issueTypeVerdicts(t,
		issueTypeVerdictGroup[scopeLine]{
			verdict: scopeLine{clearsAutomatically: true},
			members: []IssueType{issueRateLimited, issueServiceOutage},
		},
		issueTypeVerdictGroup[scopeLine]{
			// Worded for what the user can do about it.
			verdict: scopeLine{},
			members: []IssueType{IssueQuotaExceeded, issueDiskFull, issueLocalReadOnly},
		},
		issueTypeVerdictGroup[scopeLine]{
			verdict: scopeLine{generic: true},
			members: []IssueType{
				IssueInvalidFilename, issuePathTooLong, issueFileTooLarge, issueHashPanic,
				IssueUnauthorized, IssueRemoteWriteDenied, issueRemoteReadDenied, issueLocalReadDenied,
				issueLocalWriteDenied, issueCaseCollision, issueRemoteNameTwin, issueFileTooLargeForSpace,
				issueLocalNameTooLong, issueDeletesHeld, issueLocalUnsyncableReplacement, issueLocalMountBoundary,
				issueRemoteFolderUnverifiable, issueLocalInterruptedRemoval, issueRemovedFolderKept, issueLocalFileTooLarge, issueLocalBusy,
				issueLocalCrossDevice, issueUnsupportedOperation, issueMalwareDetected, issueLocalDatabaseOpen,
				issueUploadVerification, issueDownloadIntegrity, issueRequestTimeout, issueTransientConflict,
				issueTransientNotFound, issueResourceLocked, issueTransferURLRejected,
			},
		},
	), lineFor)

	assert.Equal(t, scopeLine{generic: true}, lineFor(issueTypeOutsideTheEnum), "a value outside the enum")
}

// The block-scope line's level is the family's verdict, the one the watch
// condition summary reads: an issue type whose family clears automatically
// is said at Info, every other at Warn.
// Validates: R-6.6.30
func TestBlockScopeActivationLine_AgreesWithTheFamilyVerdict(t *testing.T) {
	t.Parallel()

	const retryIn = "1m"
	for _, member := range issueTypeConstantsInSource(t) {
		_, clearsAutomatically := blockScopeActivationLine(SKService(), member, retryIn)
		key, hasFamily := conditionKeyForIssueType(member)
		assert.Equal(t, hasFamily && conditionClearsAutomatically(key), clearsAutomatically, "%q", member)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_RecordBlockedRetryWork_PersistsOnlyExactBlockedRoot(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKQuotaOwn()
	flow := testEngineFlow(t, eng)

	root := rt.sched.graph.Add(&Action{
		Type:    ActionFolderCreate,
		Path:    "dir",
		DriveID: driveid.New("drive1"),
	}, 1, nil)
	require.NotNil(t, root)

	child := rt.sched.graph.Add(&Action{
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
	assert.Equal(t, 2, rt.sched.graph.InFlightCount(), "root and dependent both remain in the graph until the exact blocked root is released")
}

// Validates: R-2.10.5
func TestEngineFlow_PersistBlockedRetryWork_CanonicalizesRowsAcrossRuntimePaths(t *testing.T) {
	t.Parallel()

	scopeKey := SKPermRemoteWrite("Shared/Docs")

	type persistCase struct {
		name    string
		persist func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime)
	}

	normalize := func(row RetryWorkRow) RetryWorkRow {
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

	want := normalize(RetryWorkRow{
		Path:       "Shared/Docs/file.txt",
		OldPath:    "Shared/Docs/old.txt",
		ActionType: ActionUpload,
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
		current := rt.sched.graph.Add(&Action{
			Type:    ActionUpload,
			Path:    "Shared/Docs/file.txt",
			OldPath: "Shared/Docs/old.txt",
			DriveID: driveid.New("drive1"),
		}, 1, nil)
		require.NotNil(t, current)
		require.NoError(t, flow.holdActionUnderScope(t.Context(), current, &actionCompletion{
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
		require.NoError(t, flow.rehomeBlockedRetryWork(t.Context(), &actionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: ActionUpload,
			HTTPStatus: 403,
		}, scopeKey))
	}
}

func persistBlockedRetryViaPermissionEvidence(scopeKey ScopeKey) func(*testing.T, *testEngine, *engineFlow, *watchRuntime) {
	return func(t *testing.T, eng *testEngine, flow *engineFlow, rt *watchRuntime) {
		t.Helper()
		err := flow.applyPermissionFailureEvidence(t.Context(), nil, &actionCompletion{
			Path:       "Shared/Docs/file.txt",
			OldPath:    "Shared/Docs/old.txt",
			ActionType: ActionUpload,
		}, permissionEvidence{
			Kind:         permissionEvidenceBoundaryDenied,
			BoundaryPath: "Shared/Docs",
			TriggerPath:  "Shared/Docs/file.txt",
			IssueType:    IssueRemoteWriteDenied,
		}, true)
		require.NoError(t, err)
	}
}

// Validates: R-2.10.5
func TestEngineFlow_ApplyTrialReclassification_RehomesDiskScopeRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	handled, err := flow.applyTrialReclassification(t.Context(), &resultDecision{
		Class:    errclass.ClassScopeBlockingTransient,
		ScopeKey: SKDiskLocal(),
	}, &actionCompletion{
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
func TestEngineFlow_ApplyTrialReclassification_LocalFilePermissionReusesPermissionEvidencePath(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))
	seedObservationIssueForTest(t, eng.baseline, "keep.txt", IssueInvalidFilename, ScopeKey{})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "accessible/file.txt",
		ActionType:   ActionDownload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))

	handled, err := flow.applyTrialReclassification(t.Context(), &resultDecision{}, &actionCompletion{
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
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))

	require.NoError(t, flow.normalizePersistedScopes(t.Context()))

	assert.False(t, isTestBlockScopeed(eng, SKDiskLocal()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_LoadActiveScopes_PopulatesRuntimeLifecycleWorkingSet(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKQuotaOwn()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))

	require.NoError(t, rt.loadActiveScopes(t.Context()))

	assert.True(t, rt.scopes.hasActiveScope(scopeKey))
	activeScopes := rt.scopes.snapshotActiveScopes()
	require.Len(t, activeScopes, 1)
	assert.Equal(t, scopeKey, activeScopes[0].Key)
}

// Validates: R-2.10.33
func TestEngineFlow_NormalizePersistedScopes_RemovesStaleScopeAndPreservesReadyRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKDiskLocal()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   now.Add(time.Minute),
	}))
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "retry-now.txt",
		ActionType:   ActionDownload,
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
	scopeKey := SKService()

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), testRetryWorkKey("blocked.txt", "", ActionUpload), scopeKey)
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
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	})

	ready := rt.sched.graph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	dispatched, err := flow.admitReady(t.Context(), []*trackedAction{ready})
	require.NoError(t, err)

	assert.Empty(t, dispatched)
	assert.Equal(t, 1, rt.sched.graph.InFlightCount())

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

	setTestBlockScope(t, eng, &BlockScope{Key: SKService()})
	setTestBlockScope(t, eng, &BlockScope{Key: testThrottleScope()})
	movedKey := testRetryWorkKey("moved.txt", "", ActionDownload)
	moved, err := eng.baseline.RecordBlockedRetryWork(t.Context(), movedKey, SKService())
	require.NoError(t, err)
	flow.retries.rowsByKey[movedKey] = *moved
	_, err = eng.baseline.RecordBlockedRetryWork(t.Context(), testRetryWorkKey("other.txt", "", ActionDownload), testThrottleScope())
	require.NoError(t, err)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))

	ready := rt.sched.graph.Add(&Action{
		Type:    ActionDownload,
		Path:    "moved.txt",
		DriveID: eng.driveID,
		View:    &pathView{Path: "moved.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = SKService()
	require.Equal(t, testThrottleScope(), rt.scopes.findBlockingScope(ready), "the throttle scope outranks the service scope")

	dispatched, err := flow.admitReady(t.Context(), []*trackedAction{ready})
	require.NoError(t, err)

	assert.Empty(t, dispatched)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
	assert.False(t, rt.scopes.hasActiveScope(SKService()), "the emptied service scope is discarded")
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
	scopeKey := SKQuotaOwn()

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), testRetryWorkKey("trial.txt", "", ActionDownload), scopeKey)
	require.NoError(t, err)

	ready := rt.sched.graph.Add(&Action{
		Type:    ActionDownload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		View:    &pathView{Path: "trial.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched, err := flow.admitReady(t.Context(), []*trackedAction{ready})
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

	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), testRetryWorkKey("trial.txt", "", ActionUpload), scopeKey)
	require.NoError(t, err)

	ready := rt.sched.graph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		View:    &pathView{Path: "trial.txt"},
	}, 1, nil)
	require.NotNil(t, ready)
	ready.IsTrial = true
	ready.TrialScopeKey = scopeKey

	dispatched, err := flow.admitReady(t.Context(), []*trackedAction{ready})
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
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	})

	ready := rt.sched.graph.Add(&Action{
		Type:    ActionUpload,
		Path:    "blocked.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, ready)

	require.NoError(t, eng.baseline.Close(t.Context()))

	dispatched, err := flow.admitReady(t.Context(), []*trackedAction{ready})
	require.Error(t, err)
	require.ErrorContains(t, err, "blocked retry_work")
	assert.Empty(t, dispatched)
	assert.Empty(t, rt.retries.heldByKey, "admission must not create in-memory held work when the blocked retry row was not durably recorded")
	assert.Equal(t, 1, rt.sched.graph.InFlightCount())
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
			held := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "retry.txt", DriveID: eng.driveID}, 1, nil)
			require.NotNil(t, held)
			work := retryWorkKeyForAction(&held.Action)
			flow.retries.rowsByKey[work] = RetryWorkRow{
				Path:         work.Path,
				ActionType:   work.ActionType,
				AttemptCount: 1,
				NextRetryAt:  tc.stored,
			}

			require.NoError(t, flow.holdActionFromPersistedRetryState(held, work))

			nextRetry, timed := flow.retries.earliestHeldRetryAt()
			require.True(t, timed, "the held retry has a time the retry timer arms for")
			assert.Equal(t, time.Unix(0, tc.stored), nextRetry)
			assert.Equal(t, tc.wantDue, flow.hasDueHeldWork(eng.nowFunc()))
		})
	}
}

// A delayed row whose time is before 1970 (negative) is still delayed work:
// with a clock that also reads before 1970, admission holds it until then.
//
// Validates: R-2.10.33
func TestRetryLedger_ADelayedRowTimedBefore1970IsHeldUntilItsTime(t *testing.T) {
	t.Parallel()

	now := time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC)
	due := now.Add(time.Hour)
	ledger := newRetryLedger()
	held := &trackedAction{Action: Action{Type: ActionUpload, Path: "moon.txt"}}
	work := retryWorkKeyForAction(&held.Action)
	ledger.rowsByKey[work] = RetryWorkRow{
		Path:         work.Path,
		ActionType:   work.ActionType,
		AttemptCount: 1,
		NextRetryAt:  storedTimeNano(due),
	}
	decision := admissionDecision{Action: held, RetryWorkKey: work}

	ledger.applyPersistedRetryAdmission(now, held, &decision)

	assert.Equal(t, admissionHoldRetry, decision.Kind, "held as delayed work, not dispatched at once")
	assert.True(t, decision.NextRetryAt.Equal(due))
}

// Validates: R-2.10.33
func TestEngineFlow_ScopeHasBlockedRetryWork_AnswersForOneScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	ctx := t.Context()
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, testRetryWorkKey("held.txt", "", ActionUpload), SKService())
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(ctx,
		&RetryWorkFailure{Work: testRetryWorkKey("timed.txt", "", ActionUpload), ScopeKey: SKQuotaOwn()},
		func(int) time.Duration { return time.Minute })
	require.NoError(t, err)

	blocked, err := flow.scopeHasBlockedRetryWork(ctx, SKService())
	require.NoError(t, err)
	assert.True(t, blocked, "a blocked row under the scope")

	blocked, err = flow.scopeHasBlockedRetryWork(ctx, SKQuotaOwn())
	require.NoError(t, err)
	assert.False(t, blocked, "a delayed row carrying the scope as evidence is not blocked under it")

	blocked, err = flow.scopeHasBlockedRetryWork(ctx, testThrottleScope())
	require.NoError(t, err)
	assert.False(t, blocked, "no row under the scope")
}
