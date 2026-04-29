package sync

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_InvalidTerminatesAndRecordsRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), nil, &ResultDecision{
		Class:         errclass.ClassInvalid,
		ConditionKey:  ConditionInvalidFilename,
		Persistence:   persistRetryWork,
		ConditionType: IssueInvalidFilename,
		TrialHint:     trialHintReclassify,
	}, nil, &ActionCompletion{
		Path:       "invalid.txt",
		ActionType: ActionUpload,
		ErrMsg:     "invalid filename",
	}, nil)

	require.ErrorContains(t, err, "invalid failure class")
	assert.Empty(t, ready)
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "invalid.txt", retryRows[0].Path)
	assert.True(t, retryRows[0].ScopeKey.IsZero())
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_ShutdownReturnsWithoutPersistence(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), nil, &ResultDecision{
		Class:     errclass.ClassShutdown,
		TrialHint: trialHintShutdown,
	}, nil, &ActionCompletion{
		Path:       "shutdown.txt",
		ActionType: ActionUpload,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_FatalTerminatesWithFatalResultError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), nil, &ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    trialHintFatal,
	}, nil, &ActionCompletion{
		Path:       "auth.txt",
		ActionType: ActionUpload,
	}, nil)

	require.EqualError(t, err, "sync: unauthorized action completion for auth.txt")
	assert.Empty(t, ready)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_RetryableTransientScopeEvidenceStaysUnblockedUntilScopeActivates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), rt, &ResultDecision{
		Class:             errclass.ClassRetryableTransient,
		ConditionKey:      ConditionServiceOutage,
		ScopeEvidence:     SKService(),
		Persistence:       persistRetryWork,
		RunScopeDetection: true,
		TrialHint:         trialHintExtendOnMatchingScope,
		ConditionType:     IssueServiceOutage,
	}, nil, &ActionCompletion{
		Path:       "retry.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		ErrMsg:     "temporary outage",
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.False(t, rt.hasActiveScope(SKService()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry.txt", retryRows[0].Path)
	assert.Equal(t, SKService(), retryRows[0].ScopeKey)
	assert.False(t, retryRows[0].Blocked)
	assert.NotZero(t, retryRows[0].NextRetryAt)
}

// Validates: R-2.10.5, R-2.10.33
func TestEngineFlow_ApplyCompletionSuccess_ClearsRetryWorkAndAdmitsDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	now := eng.nowFn()

	row := RetryWorkRow{
		Path:         "sync.txt",
		ActionType:   ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Minute).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []RetryWorkRow{row}})

	current := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "sync.txt",
		DriveID: eng.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, current)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "next.txt",
		DriveID: eng.driveID,
		ItemID:  "next-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	dispatched, err := flow.applyCompletionSuccess(t.Context(), rt, current, &ActionCompletion{
		ActionID:   1,
		Path:       "sync.txt",
		ActionType: ActionDownload,
		Success:    true,
	})
	require.NoError(t, err)
	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(2), dispatched[0].ID)
	assert.Equal(t, 1, flow.succeeded)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.8.7, R-2.10.5
func TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)

	row := RetryWorkRow{
		Path:         "stale.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  eng.nowFunc().UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	flow.retryRowsByKey[row.WorkKey()] = row

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "stale.txt",
		DriveID: eng.driveID,
		ItemID:  "stale-item",
	}, 1, nil)
	require.NotNil(t, current)
	rt.markRunning(current)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "dependent.txt",
		DriveID: eng.driveID,
		ItemID:  "dependent-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	decision := classifyResult(&ActionCompletion{Err: ErrActionPreconditionChanged})
	ready, err := flow.applyNormalCompletionDecision(t.Context(), rt, &decision, current, &ActionCompletion{
		ActionID:   current.ID,
		Path:       "stale.txt",
		ActionType: ActionUpload,
		Err:        ErrActionPreconditionChanged,
		ErrMsg:     "source changed",
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.Equal(t, 0, rt.runningCount)
	assert.Empty(t, rt.runningByID)
	assert.Equal(t, 0, rt.depGraph.InFlightCount(), "superseded dependents must not become runnable old-plan work")
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))

	succeeded, failed, errs := flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 0, failed)
	assert.Empty(t, errs)

	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionPersistsDelayedRetryWork(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		actionType        ActionType
		failureCapability PermissionCapability
	}{
		{
			name:       "local read denied",
			actionType: ActionDownload,
		},
		{
			name:              "local write denied",
			actionType:        ActionUpload,
			failureCapability: PermissionCapabilityLocalWrite,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newTestEngine(t, &engineMockClient{})
			flow := testEngineFlow(t, eng)
			now := eng.nowFunc()

			require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

			r := &ActionCompletion{
				Path:              "accessible/file.txt",
				ActionType:        tc.actionType,
				FailureCapability: tc.failureCapability,
				Err:               os.ErrPermission,
				ErrMsg:            "permission denied",
			}
			decision := classifyResult(r)

			ready, err := flow.applyNormalCompletionDecision(t.Context(), nil, &decision, nil, r, nil)

			require.NoError(t, err)
			assert.Empty(t, ready)
			assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

			retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
			require.Len(t, retryRows, 1)
			assert.Equal(t, "accessible/file.txt", retryRows[0].Path)
			assert.True(t, retryRows[0].ScopeKey.IsZero(), "file-level permission retries no longer persist condition_type")
			assert.False(t, retryRows[0].Blocked)
			assert.Equal(t, 1, retryRows[0].AttemptCount)
			assert.Greater(t, retryRows[0].NextRetryAt, now.UnixNano())
		})
	}
}

// Validates: R-2.10.5, R-2.10.33
func TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionArmsRetryTimerInWatchMode(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

	r := &ActionCompletion{
		Path:       "accessible/file.txt",
		ActionType: ActionDownload,
		Err:        os.ErrPermission,
		ErrMsg:     "permission denied",
	}
	decision := classifyResult(r)

	current := rt.depGraph.Add(&Action{
		Path: "accessible/file.txt",
		Type: ActionDownload,
	}, 1, nil)
	require.NotNil(t, current)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), rt, &decision, current, r, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.False(t, retryRows[0].Blocked)
	assert.True(t, rt.hasRetryTimer(), "file-level permission retry_work should arm the watch retry timer")
}

// Validates: R-2.10.5, R-2.10.33, R-2.14.1
func TestEngineFlow_ProcessNormalDecision_RemoteBoundaryPermissionDoesNotArmRetryTimerInWatchMode(t *testing.T) {
	t.Parallel()

	const mountRemoteRootItemID = "mount-root-id"

	eng, _ := newTestEngine(t, &engineMockClient{
		listItemPermissionsFn: func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
			assert.Equal(t, mountRemoteRootItemID, itemID)
			return []graph.Permission{{
				Roles: []string{"read"},
			}}, nil
		},
	})
	eng.remoteRootItemID = mountRemoteRootItemID
	eng.permHandler.remoteRootItemID = mountRemoteRootItemID
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	r := &ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "folder is read-only",
	}

	ready, err := flow.applyNormalCompletionDecision(t.Context(), rt, &ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  ConditionRemoteWriteDenied,
		ConditionType: IssueRemoteWriteDenied,
	}, nil, r, &Baseline{})

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.False(t, rt.hasRetryTimer(), "boundary-blocked permission failures should not arm the ordinary retry timer")
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/file.txt", retryRows[0].Path)
	assert.Equal(t, SKPermRemoteWrite(""), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
	assert.Zero(t, retryRows[0].NextRetryAt)

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, SKPermRemoteWrite(""), blockScopes[0].Key)
}

// Validates: R-2.10.33, R-2.14.1
func TestEngineFlow_ProcessNormalDecision_KnownRemoteBoundaryNoOpDoesNotPersistOrArmRetryTimer(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKPermRemoteWrite("Shared")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	}))

	r := &ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "still read-only",
	}

	ready, err := flow.applyNormalCompletionDecision(t.Context(), rt, &ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  ConditionRemoteWriteDenied,
		ConditionType: IssueRemoteWriteDenied,
	}, nil, r, &Baseline{})

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.False(t, rt.hasRetryTimer())
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, scopeKey, blockScopes[0].Key)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_RearmOrDiscardRecordsFailureWithoutTerminating(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()

	ready, err := flow.applyTrialCompletionDecision(t.Context(), nil, scopeKey, &ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  ConditionInvalidFilename,
		ConditionType: IssueInvalidFilename,
		Persistence:   persistRetryWork,
		TrialHint:     trialHintReclassify,
	}, nil, &ActionCompletion{
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		TrialScopeKey: scopeKey,
		ErrMsg:        "still invalid",
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)

	succeeded, failed, errs := flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 1, failed)
	assert.Empty(t, errs)
}

// Validates: R-2.10.5, R-2.10.33, R-2.14.1
func TestEngineFlow_ProcessTrialDecision_UnmatchedPermissionEvidenceFallsBackToRetryPersistence(t *testing.T) {
	t.Parallel()

	const mountRemoteRootItemID = "mount-root-id"

	eng, _ := newTestEngine(t, &engineMockClient{
		listItemPermissionsFn: func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
			assert.Equal(t, mountRemoteRootItemID, itemID)
			return []graph.Permission{{
				Roles: []string{"write"},
			}}, nil
		},
	})
	eng.remoteRootItemID = mountRemoteRootItemID
	eng.permHandler.remoteRootItemID = mountRemoteRootItemID
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	blockedRow := &RetryWorkRow{
		Path:         "file.txt",
		ActionType:   ActionUpload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), blockedRow))
	flow.retryRowsByKey[blockedRow.WorkKey()] = *blockedRow

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "file.txt",
		DriveID: driveid.New(engineTestDriveID),
	}, 1, nil)
	require.NotNil(t, current)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), rt, scopeKey, &ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  ConditionRemoteWriteDenied,
		ConditionType: IssueRemoteWriteDenied,
		Persistence:   persistRetryWork,
		TrialHint:     trialHintReclassify,
	}, current, &ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    ActionUpload,
		HTTPStatus:    http.StatusForbidden,
		ErrMsg:        "permission evidence inconclusive",
		IsTrial:       true,
		TrialScopeKey: scopeKey,
	}, bl)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.hasRetryTimer(), "generic retry fallback should arm the ordinary retry timer")

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "file.txt", retryRows[0].Path)
	assert.False(t, retryRows[0].Blocked)
	assert.True(t, retryRows[0].ScopeKey.IsZero())
	assert.NotZero(t, retryRows[0].NextRetryAt)

	assert.False(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FallbackActivatesReclassifiedBlockedScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	trialScopeKey := SKService()
	throttleScopeKey := SKThrottleDrive(eng.driveID)

	setTestBlockScope(t, eng, &BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "file.txt",
		ActionType:   ActionUpload,
		ScopeKey:     trialScopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "file.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), rt, trialScopeKey, &ResultDecision{
		Class:             errclass.ClassBlockScopeingTransient,
		ConditionKey:      ConditionRateLimited,
		ConditionType:     IssueRateLimited,
		ScopeKey:          throttleScopeKey,
		ScopeEvidence:     throttleScopeKey,
		Persistence:       persistRetryWork,
		RunScopeDetection: true,
		TrialHint:         trialHintExtendOnMatchingScope,
	}, current, &ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    ActionUpload,
		DriveID:       eng.driveID,
		HTTPStatus:    http.StatusTooManyRequests,
		RetryAfter:    time.Minute,
		ErrMsg:        "throttled",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.hasActiveScope(throttleScopeKey),
		"trial retry fallback should activate the newly classified blocked scope")
	assert.False(t, rt.hasActiveScope(trialScopeKey),
		"the old trial scope should be discarded once its blocked work is reclassified")
	assert.True(t, rt.hasTrialTimer(), "new blocked scope should arm the watch trial timer")

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, throttleScopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FallbackKeepsOriginalScopeWithRemainingBlockedWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	trialScopeKey := SKService()
	throttleScopeKey := SKThrottleDrive(driveid.New("drive-throttle"))

	setTestBlockScope(t, eng, &BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	upsertBlockedRetryWorkForTest(t, eng, "file.txt", trialScopeKey)
	upsertBlockedRetryWorkForTest(t, eng, "other.txt", trialScopeKey)

	other := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "other.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, other)
	rt.holdAction(other, heldReasonScope, trialScopeKey, time.Time{})

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "file.txt",
		DriveID: eng.driveID,
	}, 2, nil)
	require.NotNil(t, current)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), rt, trialScopeKey, &ResultDecision{
		Class:             errclass.ClassBlockScopeingTransient,
		ConditionKey:      ConditionRateLimited,
		ConditionType:     IssueRateLimited,
		ScopeKey:          throttleScopeKey,
		ScopeEvidence:     throttleScopeKey,
		Persistence:       persistRetryWork,
		RunScopeDetection: true,
		TrialHint:         trialHintExtendOnMatchingScope,
	}, current, &ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    ActionUpload,
		DriveID:       eng.driveID,
		HTTPStatus:    http.StatusTooManyRequests,
		RetryAfter:    time.Minute,
		ErrMsg:        "throttled",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.hasActiveScope(throttleScopeKey),
		"trial retry fallback should activate the newly classified blocked scope")
	assert.True(t, rt.hasActiveScope(trialScopeKey),
		"the original scope must stay active while unrelated blocked work still remains under it")
	assert.Contains(t, rt.heldByKey, retryWorkKeyForAction(&other.Action),
		"existing blocked work under the original scope must remain held in the runtime")

	rowsByPath := retryRowsByPathForTest(t, eng)
	require.Contains(t, rowsByPath, "file.txt")
	require.Contains(t, rowsByPath, "other.txt")
	assert.Equal(t, throttleScopeKey, rowsByPath["file.txt"].ScopeKey)
	assert.True(t, rowsByPath["file.txt"].Blocked)
	assert.Equal(t, trialScopeKey, rowsByPath["other.txt"].ScopeKey)
	assert.True(t, rowsByPath["other.txt"].Blocked)

	blockScopes := blockScopesByKeyForTest(t, eng)
	require.Contains(t, blockScopes, trialScopeKey)
	require.Contains(t, blockScopes, throttleScopeKey)
	assert.Equal(t, eng.nowFunc().Add(time.Minute), blockScopes[trialScopeKey].NextTrialAt,
		"retained original scope must rearm to the next trial interval instead of staying immediately due")
	assert.Equal(t, eng.nowFunc().Add(time.Minute), blockScopes[throttleScopeKey].NextTrialAt)
}

// Validates: R-2.8.7, R-2.10.5
func TestEngineFlow_ProcessTrialDecision_SupersededClearsExactRetryAndDiscardsEmptyScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	trialScopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	row := RetryWorkRow{
		Path:         "trial-stale.txt",
		ActionType:   ActionUpload,
		ScopeKey:     trialScopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	flow.retryRowsByKey[row.WorkKey()] = row

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial-stale.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)
	rt.markRunning(current)

	decision := classifyResult(&ActionCompletion{Err: ErrActionPreconditionChanged})
	ready, err := flow.applyTrialCompletionDecision(t.Context(), rt, trialScopeKey, &decision, current, &ActionCompletion{
		ActionID:      current.ID,
		Path:          "trial-stale.txt",
		ActionType:    ActionUpload,
		Err:           ErrActionPreconditionChanged,
		ErrMsg:        "source changed",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.Equal(t, 0, rt.depGraph.InFlightCount())
	assert.Equal(t, 0, rt.runningCount)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.False(t, isTestBlockScopeed(eng, trialScopeKey), "empty trial scope should be discarded, not extended")

	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
}

func upsertBlockedRetryWorkForTest(t *testing.T, eng *testEngine, path string, scopeKey ScopeKey) {
	t.Helper()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         path,
		ActionType:   ActionUpload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))
}

func retryRowsByPathForTest(t *testing.T, eng *testEngine) map[string]RetryWorkRow {
	t.Helper()

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 2)

	rowsByPath := make(map[string]RetryWorkRow, len(retryRows))
	for i := range retryRows {
		row := retryRows[i]
		rowsByPath[row.Path] = row
	}

	return rowsByPath
}

func blockScopesByKeyForTest(t *testing.T, eng *testEngine) map[ScopeKey]BlockScope {
	t.Helper()

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)

	byKey := make(map[ScopeKey]BlockScope, len(blockScopes))
	for _, block := range blockScopes {
		byKey[block.Key] = *block
	}

	return byKey
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessActionCompletion_TrialSuccessReleasesScopeBeforeAdmittingDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		NextTrialAt:   eng.nowFunc().Add(-time.Second),
		TrialInterval: time.Minute,
	})

	current := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, current)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "dependent.txt",
		DriveID: eng.driveID,
		ItemID:  "dependent-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	ready, err := rt.applyRuntimeCompletionStage(t.Context(), rt, &ActionCompletion{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		DriveID:       eng.driveID,
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: scopeKey,
	}, &Baseline{})

	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, int64(2), ready[0].ID)
	assert.Equal(t, "dependent.txt", ready[0].Action.Path)
	assert.False(t, rt.hasActiveScope(scopeKey))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_ShutdownCompletesWithoutTerminating(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), nil, SKService(), &ResultDecision{
		TrialHint: trialHintShutdown,
	}, nil, &ActionCompletion{
		Path:       "trial-shutdown.txt",
		ActionType: ActionUpload,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FatalTerminatesWithFatalResultError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), nil, SKService(), &ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    trialHintFatal,
	}, nil, &ActionCompletion{
		Path:       "trial-auth.txt",
		ActionType: ActionUpload,
	}, nil)

	require.EqualError(t, err, "sync: unauthorized action completion for trial-auth.txt")
	assert.Empty(t, ready)
}

// Validates: R-6.8
func TestEngineFlow_ProcessActionCompletion_RetryPersistenceFailureTerminatesAndMarksFinished(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	current := flow.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "retry.txt",
	}, 1, nil)
	require.NotNil(t, current)
	flow.markRunning(current)

	require.NoError(t, eng.baseline.Close(t.Context()))

	ready, err := flow.applyRuntimeCompletionStage(t.Context(), nil, &ActionCompletion{
		ActionID:   1,
		Path:       "retry.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		ErrMsg:     "temporary outage",
	}, nil)

	require.ErrorContains(t, err, "record retry_work")
	assert.Empty(t, ready)
	assert.Zero(t, flow.runningCount, "control-state persistence failure must still finish the completed action bookkeeping")
	assert.Empty(t, flow.runningByID)
	assert.Equal(t, 1, flow.depGraph.InFlightCount(), "the unresolved action must remain in the graph when the runtime fails closed")
}
