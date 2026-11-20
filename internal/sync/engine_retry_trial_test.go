package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.33
func TestReleaseDueHeldRetriesNow_ReleasesHeldRetryEntriesOnly(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ta := rt.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "retry.txt",
		View: &syncdomain.PathView{Path: "retry.txt"},
	}, 1, nil)
	require.NotNil(t, ta)

	rt.holdAction(ta, heldReasonRetry, syncdomain.ScopeKey{}, eng.nowFunc().Add(-time.Second))

	outbox, err := rt.releaseDueHeldRetriesNow(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, "retry.txt", outbox[0].Action.Path)
	assert.False(t, outbox[0].IsTrial)
	assert.Empty(t, rt.retries.heldByKey)
}

// Validates: R-2.10.33
func TestReleaseDueHeldRetriesNow_DoesNotConsultDurableRetryRowsWithoutHeldRuntimeEntry(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "retry.txt",
		ActionType:   syncdomain.ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}))

	outbox, err := rt.releaseDueHeldRetriesNow(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, outbox)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry.txt", retryRows[0].Path)
}

// Validates: R-2.10.5
func TestReleaseDueHeldTrialsNow_ReleasesFirstHeldScopeCandidateAsTrial(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		NextTrialAt:   eng.nowFunc().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	first := rt.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "first.txt",
		View: &syncdomain.PathView{Path: "first.txt"},
	}, 1, nil)
	second := rt.sched.graph.Add(&syncdomain.Action{
		Type: syncdomain.ActionDownload,
		Path: "second.txt",
		View: &syncdomain.PathView{Path: "second.txt"},
	}, 2, nil)
	require.NotNil(t, first)
	require.NotNil(t, second)

	rt.holdAction(first, heldReasonScope, scopeKey, time.Time{})
	rt.holdAction(second, heldReasonScope, scopeKey, time.Time{})

	outbox, err := rt.releaseDueHeldTrialsNow(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, "first.txt", outbox[0].Action.Path)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, scopeKey, outbox[0].TrialScopeKey)

	require.Len(t, rt.retries.heldByKey, 1)
	assert.Contains(t, rt.retries.heldByKey, syncdomain.RetryWorkKeyForAction(&second.Action))
}

// Validates: R-2.10.5
func TestReleaseDueHeldTrialsNow_SkipsScopesWithoutHeldDependencyReadyCandidates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		NextTrialAt:   eng.nowFunc().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	outbox, err := rt.releaseDueHeldTrialsNow(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.True(t, isTestBlockScopeed(eng, scopeKey))
}

// Validates: R-2.10.5
func TestReleaseDueHeldTrialsNow_DoesNotConsultDurableBlockedRetryRowsWithoutHeldRuntimeEntry(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	scopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		NextTrialAt:   eng.nowFunc().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})
	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(), syncfixture.TestRetryWorkKey("blocked.txt", "", syncdomain.ActionUpload), scopeKey)
	require.NoError(t, err)

	outbox, err := rt.releaseDueHeldTrialsNow(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, outbox)
	assert.True(t, isTestBlockScopeed(eng, scopeKey))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.33
func TestClearRetryWorkOnSuccess_RemovesResolvedRetryRow(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	flow := testEngineFlow(t, eng)
	now := eng.nowFunc().UnixNano()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "done.txt",
		ActionType:   syncdomain.ActionUpload,
		AttemptCount: 2,
		NextRetryAt:  now,
	}))

	require.NoError(t, flow.clearRetryWorkOnSuccess(t.Context(), &syncdomain.ActionCompletion{
		Path:       "done.txt",
		ActionType: syncdomain.ActionUpload,
	}))

	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.33, R-6.6.28
//
// A store failure while clearing the row used to be Debug-only: the row stayed,
// `status` kept listing a file that had just synced as retrying, and it was
// retried again. It is returned like any store write failure in the
// completion path.
func TestClearRetryWorkOnSuccess_StoreFailureIsReturned(t *testing.T) {
	t.Parallel()

	flow := &engineFlow{
		deps: runtimeDeps{store: closedTestStore(t), logger: syncfixture.TestLogger(t)},
	}

	err := flow.clearRetryWorkOnSuccess(t.Context(), &syncdomain.ActionCompletion{
		Path:       "done.txt",
		ActionType: syncdomain.ActionUpload,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clear retry work after success")

	err = flow.clearRetryWorkOnActionSuccess(t.Context(), &syncdomain.Action{Path: "done.txt", Type: syncdomain.ActionUpload})
	require.Error(t, err)
}
