package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.33
func TestRunRetrierSweep_ReleasesHeldRetryEntriesOnly(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ta := rt.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "retry.txt",
	}, 1, nil)
	require.NotNil(t, ta)

	rt.holdAction(ta, heldReasonRetry, ScopeKey{}, eng.nowFn().Add(-time.Second))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runRetrierSweep(t.Context(), bl, SyncBidirectional)
	require.Len(t, outbox, 1)
	assert.Equal(t, "retry.txt", outbox[0].Action.Path)
	assert.False(t, outbox[0].IsTrial)
	assert.Empty(t, rt.heldByKey)
}

// Validates: R-2.10.33
func TestRunRetrierSweep_DoesNotConsultDurableRetryRowsWithoutHeldRuntimeEntry(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	now := eng.nowFn()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "retry.txt",
		ActionType:   ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
		FirstSeenAt:  now.Add(-time.Minute).UnixNano(),
		LastSeenAt:   now.UnixNano(),
	}))

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runRetrierSweep(t.Context(), bl, SyncBidirectional)
	assert.Empty(t, outbox)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry.txt", retryRows[0].Path)
}

// Validates: R-2.10.5
func TestRunTrialDispatch_ReleasesFirstHeldScopeCandidateAsTrial(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	first := rt.depGraph.Add(&Action{
		Type: ActionUpload,
		Path: "first.txt",
	}, 1, nil)
	second := rt.depGraph.Add(&Action{
		Type: ActionDownload,
		Path: "second.txt",
	}, 2, nil)
	require.NotNil(t, first)
	require.NotNil(t, second)

	rt.holdAction(first, heldReasonScope, scopeKey, time.Time{})
	rt.holdAction(second, heldReasonScope, scopeKey, time.Time{})

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runTrialDispatch(t.Context(), bl, SyncBidirectional)
	require.Len(t, outbox, 1)
	assert.Equal(t, "first.txt", outbox[0].Action.Path)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, scopeKey, outbox[0].TrialScopeKey)

	require.Len(t, rt.heldByKey, 1)
	assert.Contains(t, rt.heldByKey, retryWorkKeyForAction(&second.Action))
}

// Validates: R-2.10.5
func TestRunTrialDispatch_SkipsScopesWithoutHeldDependencyReadyCandidates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runTrialDispatch(t.Context(), bl, SyncBidirectional)
	assert.Empty(t, outbox)
	assert.True(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestRunTrialDispatch_DoesNotConsultDurableBlockedRetryRowsWithoutHeldRuntimeEntry(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := SKService()

	setTestBlockScope(t, eng, &BlockScope{
		Key:           scopeKey,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})
	_, err := eng.baseline.RecordRetryWorkFailure(t.Context(), &RetryWorkFailure{
		Path:          "blocked.txt",
		ActionType:    ActionUpload,
		ConditionType: IssueServiceOutage,
		ScopeKey:      scopeKey,
		LastError:     "blocked",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	outbox := rt.runTrialDispatch(t.Context(), bl, SyncBidirectional)
	assert.Empty(t, outbox)
	assert.True(t, isTestBlockScopeed(eng, scopeKey))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.33
func TestClearRetryWorkOnSuccess_RemovesResolvedRetryRow(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	now := eng.nowFunc().UnixNano()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &RetryWorkRow{
		Path:         "done.txt",
		ActionType:   ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  now,
		FirstSeenAt:  now - 10,
		LastSeenAt:   now,
	}))

	flow.clearRetryWorkOnSuccess(t.Context(), &ActionCompletion{
		Path:       "done.txt",
		ActionType: ActionUpload,
	})

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}
