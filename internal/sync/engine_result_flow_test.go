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

	outcome := flow.processNormalDecision(t.Context(), nil, &ResultDecision{
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

	require.True(t, outcome.terminate)
	require.ErrorContains(t, outcome.terminateErr, "invalid failure class")
	assert.Empty(t, outcome.dispatched)
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "invalid.txt", retryRows[0].Path)
	assert.Equal(t, IssueInvalidFilename, retryRows[0].ConditionType)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_ShutdownReturnsWithoutPersistence(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	outcome := flow.processNormalDecision(t.Context(), nil, &ResultDecision{
		Class:     errclass.ClassShutdown,
		TrialHint: trialHintShutdown,
	}, nil, &ActionCompletion{
		Path:       "shutdown.txt",
		ActionType: ActionUpload,
	}, nil)

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_FatalTerminatesWithFatalResultError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	outcome := flow.processNormalDecision(t.Context(), nil, &ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    trialHintFatal,
	}, nil, &ActionCompletion{
		Path:       "auth.txt",
		ActionType: ActionUpload,
	}, nil)

	require.True(t, outcome.terminate)
	require.EqualError(t, outcome.terminateErr, "sync: unauthorized action completion for auth.txt")
	assert.Empty(t, outcome.dispatched)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_RetryableTransientScopeEvidenceStaysUnblockedUntilScopeActivates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	outcome := flow.processNormalDecision(t.Context(), rt, &ResultDecision{
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

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)
	assert.False(t, rt.hasActiveScope(SKService()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry.txt", retryRows[0].Path)
	assert.Equal(t, SKService(), retryRows[0].ScopeKey)
	assert.False(t, retryRows[0].Blocked)
	assert.NotZero(t, retryRows[0].NextRetryAt)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionPersistsDelayedRetryWork(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		actionType        ActionType
		failureCapability PermissionCapability
		wantIssueType     string
	}{
		{
			name:          "local read denied",
			actionType:    ActionDownload,
			wantIssueType: IssueLocalReadDenied,
		},
		{
			name:              "local write denied",
			actionType:        ActionUpload,
			failureCapability: PermissionCapabilityLocalWrite,
			wantIssueType:     IssueLocalWriteDenied,
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

			outcome := flow.processNormalDecision(t.Context(), nil, &decision, nil, r, nil)

			assert.False(t, outcome.terminate)
			require.NoError(t, outcome.terminateErr)
			assert.Empty(t, outcome.dispatched)
			assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

			retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
			require.Len(t, retryRows, 1)
			assert.Equal(t, "accessible/file.txt", retryRows[0].Path)
			assert.Equal(t, tc.wantIssueType, retryRows[0].ConditionType)
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

	outcome := flow.processNormalDecision(t.Context(), rt, &decision, current, r, nil)

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.False(t, retryRows[0].Blocked)
	assert.True(t, rt.hasRetryTimer(), "file-level permission retry_work should arm the watch retry timer")
}

// Validates: R-2.10.5, R-2.10.33, R-2.14.1
func TestEngineFlow_ProcessNormalDecision_RemoteBoundaryPermissionDoesNotArmRetryTimerInWatchMode(t *testing.T) {
	t.Parallel()

	const sharedRootItemID = "shared-root-id"

	eng, _ := newTestEngine(t, &engineMockClient{
		listItemPermissionsFn: func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
			assert.Equal(t, sharedRootItemID, itemID)
			return []graph.Permission{{
				Roles: []string{"read"},
			}}, nil
		},
	})
	eng.rootItemID = sharedRootItemID
	eng.permHandler.rootItemID = sharedRootItemID
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	r := &ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "folder is read-only",
	}

	outcome := flow.processNormalDecision(t.Context(), rt, &ResultDecision{
		Class:          errclass.ClassActionable,
		ConditionKey:   ConditionRemoteWriteDenied,
		ConditionType:  IssueRemoteWriteDenied,
		PermissionFlow: permissionFlowRemote403,
	}, nil, r, &Baseline{})

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)
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
		BlockedAt:     eng.nowFunc().Add(-time.Minute),
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	}))

	r := &ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "still read-only",
	}

	outcome := flow.processNormalDecision(t.Context(), rt, &ResultDecision{
		Class:          errclass.ClassActionable,
		ConditionKey:   ConditionRemoteWriteDenied,
		ConditionType:  IssueRemoteWriteDenied,
		PermissionFlow: permissionFlowRemote403,
	}, nil, r, &Baseline{})

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)
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

	outcome := flow.processTrialDecision(t.Context(), nil, scopeKey, &ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  ConditionInvalidFilename,
		ConditionType: IssueInvalidFilename,
		TrialHint:     trialHintReclassify,
	}, nil, &ActionCompletion{
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		TrialScopeKey: scopeKey,
		ErrMsg:        "still invalid",
	}, nil)

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)

	succeeded, failed, errs := flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 1, failed)
	assert.Empty(t, errs)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_ShutdownCompletesWithoutTerminating(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	outcome := flow.processTrialDecision(t.Context(), nil, SKService(), &ResultDecision{
		TrialHint: trialHintShutdown,
	}, nil, &ActionCompletion{
		Path:       "trial-shutdown.txt",
		ActionType: ActionUpload,
	}, nil)

	assert.False(t, outcome.terminate)
	require.NoError(t, outcome.terminateErr)
	assert.Empty(t, outcome.dispatched)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FatalTerminatesWithFatalResultError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	outcome := flow.processTrialDecision(t.Context(), nil, SKService(), &ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: ConditionAuthenticationRequired,
		TrialHint:    trialHintFatal,
	}, nil, &ActionCompletion{
		Path:       "trial-auth.txt",
		ActionType: ActionUpload,
	}, nil)

	require.True(t, outcome.terminate)
	require.EqualError(t, outcome.terminateErr, "sync: unauthorized action completion for trial-auth.txt")
	assert.Empty(t, outcome.dispatched)
}
