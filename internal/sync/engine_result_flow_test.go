package sync

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_InvalidTerminatesAndRecordsRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	outcome := flow.processNormalDecision(t.Context(), nil, &ResultDecision{
		Class:       errclass.ClassInvalid,
		SummaryKey:  SummaryInvalidFilename,
		Persistence: persistRetryWork,
		IssueType:   IssueInvalidFilename,
		TrialHint:   trialHintPreserve,
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
	assert.Equal(t, IssueInvalidFilename, retryRows[0].IssueType)
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
		Class:      errclass.ClassFatal,
		SummaryKey: SummaryAuthenticationRequired,
		TrialHint:  trialHintFatal,
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
		SummaryKey:        SummaryServiceOutage,
		ScopeEvidence:     SKService(),
		Persistence:       persistRetryWork,
		RunScopeDetection: true,
		TrialHint:         trialHintExtendOnMatchingScope,
		IssueType:         IssueServiceOutage,
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
func TestEngineFlow_ProcessTrialDecision_PreserveRecordsFailureWithoutTerminating(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := SKService()

	outcome := flow.processTrialDecision(t.Context(), nil, scopeKey, &ResultDecision{
		Class:      errclass.ClassActionable,
		SummaryKey: SummaryInvalidFilename,
		IssueType:  IssueInvalidFilename,
		TrialHint:  trialHintPreserve,
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
		Class:      errclass.ClassFatal,
		SummaryKey: SummaryAuthenticationRequired,
		TrialHint:  trialHintFatal,
	}, nil, &ActionCompletion{
		Path:       "trial-auth.txt",
		ActionType: ActionUpload,
	}, nil)

	require.True(t, outcome.terminate)
	require.EqualError(t, outcome.terminateErr, "sync: unauthorized action completion for trial-auth.txt")
	assert.Empty(t, outcome.dispatched)
}
