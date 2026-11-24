package sync

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_InvalidTerminatesAndRecordsRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:         errclass.ClassInvalid,
		ConditionKey:  syncdomain.ConditionInvalidFilename,
		Persistence:   syncdomain.PersistRetryWork,
		ConditionType: syncdomain.IssueInvalidFilename,
		TrialHint:     syncdomain.TrialHintReclassify,
	}, nil, &syncdomain.ActionCompletion{
		Path:       "invalid.txt",
		ActionType: syncdomain.ActionUpload,
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

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:     errclass.ClassShutdown,
		TrialHint: syncdomain.TrialHintShutdown,
	}, nil, &syncdomain.ActionCompletion{
		Path:       "shutdown.txt",
		ActionType: syncdomain.ActionUpload,
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

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: syncdomain.ConditionAuthenticationRequired,
		TrialHint:    syncdomain.TrialHintFatal,
	}, nil, &syncdomain.ActionCompletion{
		Path:       "auth.txt",
		ActionType: syncdomain.ActionUpload,
	}, nil)

	require.EqualError(t, err, "sync: unauthorized action completion for auth.txt")
	assert.Empty(t, ready)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	blockScopes, blockErr := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, blockErr)
	assert.Empty(t, blockScopes)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_RetryableTransientScopeEvidenceStaysUnblockedUntilScopeActivates(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:             errclass.ClassRetryableTransient,
		ConditionKey:      syncdomain.ConditionServiceOutage,
		ScopeEvidence:     syncdomain.SKService(),
		Persistence:       syncdomain.PersistRetryWork,
		RunScopeDetection: true,
		TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
		ConditionType:     syncdomain.IssueServiceOutage,
	}, nil, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:       "retry.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		ErrMsg:     "temporary outage",
	}), nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.False(t, rt.sched.HasScope(syncdomain.SKService()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "retry.txt", retryRows[0].Path)
	assert.Equal(t, syncdomain.SKService(), retryRows[0].ScopeKey)
	assert.False(t, retryRows[0].Blocked)
	assert.NotZero(t, retryRows[0].NextRetryAt)
}

// Validates: R-2.10.3, R-2.10.5
//
// A 503 with Retry-After blocks the service scope at once, and the failure
// that said so is that scope's first blocked work. Recorded unblocked before
// the scope existed, it left a block scope with no blocked work, an invariant
// violation found by the chaos Graph layer (D-092).
func TestEngineFlow_ProcessNormalDecision_ServiceRetryAfterBlocksTheTriggeringWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	completion := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:       "outage.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusServiceUnavailable,
		RetryAfter: 30 * time.Second,
		ErrMsg:     "service unavailable",
	})
	decision := classifyResult(completion)
	require.Equal(t, errclass.ClassRetryableTransient, decision.Class)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, completion, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.sched.HasScope(syncdomain.SKService()))
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, syncdomain.SKService(), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked, "the work that activated the scope waits for its trial")
	assert.Equal(t, 1, retryRows[0].AttemptCount, "one failure is one attempt")
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.5
//
// A scope-blocking decision names its scope twice, as the scope it blocks
// (ScopeKey) and as the evidence the retry row records (ScopeEvidence).
// Classification sets them equal; if a decision ever carried two different
// keys, the row was blocked under the evidence while the scope activated was
// the other one, a block scope with no blocked work. The row now follows the
// scope that is activated.
func TestEngineFlow_ProcessNormalDecision_BlockedRowFollowsTheActivatedScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	_, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:         errclass.ClassScopeBlockingTransient,
		ScopeKey:      syncdomain.SKDiskLocal(),
		ScopeEvidence: syncdomain.SKService(),
		Persistence:   syncdomain.PersistRetryWork,
		TrialHint:     syncdomain.TrialHintExtendOnMatchingScope,
		ConditionType: syncdomain.IssueDiskFull,
	}, nil, &syncdomain.ActionCompletion{Path: "full.bin", ActionType: syncdomain.ActionDownload, ErrMsg: "no space"}, nil)
	require.NoError(t, err)

	assert.True(t, rt.sched.HasScope(syncdomain.SKDiskLocal()))
	rows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, rows, 1)
	assert.Equal(t, syncdomain.SKDiskLocal(), rows[0].ScopeKey)
	assert.True(t, rows[0].Blocked)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.28, R-2.10.29
//
// The outage failure that crosses the service window's threshold activates
// the scope with itself blocked under it, as the Retry-After case does.
func TestEngineFlow_ProcessNormalDecision_ServiceWindowBlocksTheCrossingWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	for i := range syncsched.ServiceWindowThreshold {
		completion := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
			Path:       fmt.Sprintf("outage-%d.txt", i),
			ActionType: syncdomain.ActionUpload,
			HTTPStatus: http.StatusBadGateway,
			ErrMsg:     "bad gateway",
		})
		decision := classifyResult(completion)
		_, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, completion, nil)
		require.NoError(t, err)
	}

	assert.True(t, rt.sched.HasScope(syncdomain.SKService()))
	blocked, err := eng.baseline.ListBlockedRetryWork(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, blocked)
	assert.Equal(t, fmt.Sprintf("outage-%d.txt", syncsched.ServiceWindowThreshold-1), blocked[len(blocked)-1].Path)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.5, R-2.10.33
func TestEngineFlow_ApplyCompletionSuccess_ClearsRetryWorkAndAdmitsDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	now := eng.nowFunc()

	row := syncdomain.RetryWorkRow{
		Path:         "sync.txt",
		ActionType:   syncdomain.ActionDownload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Minute).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []syncdomain.RetryWorkRow{row}})

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "sync.txt",
		DriveID: eng.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, current)

	dependent := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "next.txt",
		DriveID: eng.driveID,
		ItemID:  "next-item",
		View:    &syncdomain.PathView{Path: "next.txt"},
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	dispatched, err := flow.applyCompletionSuccess(t.Context(), current, &syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "sync.txt",
		ActionType: syncdomain.ActionDownload,
		Success:    true,
	})
	require.NoError(t, err)
	require.Len(t, dispatched, 1)
	assert.Equal(t, int64(2), dispatched[0].ID)
	assert.Equal(t, 1, flow.results.succeeded)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.8.7, R-2.10.5
func TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)

	row := syncdomain.RetryWorkRow{
		Path:         "stale.txt",
		ActionType:   syncdomain.ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  eng.nowFunc().UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	flow.retryWork.rows[row.WorkKey()] = row

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "stale.txt",
		DriveID: eng.driveID,
		ItemID:  "stale-item",
	}, 1, nil)
	require.NotNil(t, current)
	rt.sched.Running(current)

	dependent := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "dependent.txt",
		DriveID: eng.driveID,
		ItemID:  "dependent-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	decision := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: syncdomain.ErrActionPreconditionChanged}))
	ready, err := flow.applyNormalCompletionDecision(t.Context(), &decision, current, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:   current.ID,
		Path:       "stale.txt",
		ActionType: syncdomain.ActionUpload,
		Err:        syncdomain.ErrActionPreconditionChanged,
		ErrMsg:     "source changed",
	}), nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.Equal(t, 0, rt.sched.RunningCount())
	assert.Zero(t, rt.sched.RunningCount())
	assert.Equal(t, 0, rt.sched.InFlight(), "superseded dependents must not become runnable old-plan work")
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))

	succeeded, failed, errs := flow.resultStats()
	assert.Equal(t, 0, succeeded)
	assert.Equal(t, 0, failed)
	assert.Empty(t, errs)

	batch := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.8.7
//
// A superseded completion never reaches the permission probes, whatever its
// error chain also carries: the stale verdict decides. Probed first, a stale
// error holding a 403 or a local permission denial would record permission
// retry work or a write-denied scope for work the replan replaces.
func TestEngineFlow_ProcessNormalDecision_StaleCompletionIsNeverProbedForPermissions(t *testing.T) {
	t.Parallel()

	const remoteRootItemID = "mount-root-id"
	testCases := []struct {
		name       string
		completion syncdomain.ActionCompletion
	}{
		{
			name: "local permission denial",
			completion: *syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Path:       "accessible/file.txt",
				ActionType: syncdomain.ActionDownload,
				Err:        fmt.Errorf("%w: %w", syncdomain.ErrActionPreconditionChanged, os.ErrPermission),
			}),
		},
		{
			name: "remote 403",
			completion: *syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Path:       "Shared/file.txt",
				ActionType: syncdomain.ActionUpload,
				HTTPStatus: http.StatusForbidden,
				Err: fmt.Errorf("%w: %w", syncdomain.ErrActionPreconditionChanged,
					&graph.GraphError{StatusCode: http.StatusForbidden, Err: graph.ErrForbidden}),
			}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var probes atomic.Int32
			eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{
				ListItemPermissionsFn: func(context.Context, driveid.ID, string) ([]graph.Permission, error) {
					probes.Add(1)
					return []graph.Permission{{Roles: []string{"read"}}}, nil
				},
			})
			eng.remoteRootItemID = remoteRootItemID
			eng.permHandler.remoteRootItemID = remoteRootItemID
			flow := testEngineFlow(t, eng)
			require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

			r := tc.completion
			r.ErrMsg = r.Err.Error()
			decision := classifyResult(&r)
			require.Equal(t, resultSuperseded, decision.Class)

			ready, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, &r, syncdomain.NewBaselineSnapshot(nil))

			require.NoError(t, err)
			assert.Empty(t, ready)
			assert.Equal(t, 1, flow.results.superseded)
			assert.Zero(t, probes.Load(), "no remote permission probe")
			assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
			blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
			require.NoError(t, err)
			assert.Empty(t, blockScopes)
		})
	}
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessNormalDecision_FileLevelLocalPermissionPersistsDelayedRetryWork(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name              string
		actionType        syncdomain.ActionType
		failureCapability syncdomain.PermissionCapability
	}{
		{
			name:       "local read denied",
			actionType: syncdomain.ActionDownload,
		},
		{
			name:              "local write denied",
			actionType:        syncdomain.ActionUpload,
			failureCapability: syncdomain.PermissionCapabilityLocalWrite,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
			flow := testEngineFlow(t, eng)
			now := eng.nowFunc()

			require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

			r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Path:              "accessible/file.txt",
				ActionType:        tc.actionType,
				FailureCapability: tc.failureCapability,
				Err:               os.ErrPermission,
				ErrMsg:            "permission denied",
			})
			decision := classifyResult(r)

			ready, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, r, nil)

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

	eng, syncRoot := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)

	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

	r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:       "accessible/file.txt",
		ActionType: syncdomain.ActionDownload,
		Err:        os.ErrPermission,
		ErrMsg:     "permission denied",
	})
	decision := classifyResult(r)

	current := rt.sched.AddForTest(&syncdomain.Action{
		Path: "accessible/file.txt",
		Type: syncdomain.ActionDownload,
	}, 1, nil)
	require.NotNil(t, current)

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &decision, current, r, nil)

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

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{
		ListItemPermissionsFn: func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
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

	r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "folder is read-only",
	})

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  syncdomain.ConditionRemoteWriteDenied,
		ConditionType: syncdomain.IssueRemoteWriteDenied,
	}, nil, r, syncdomain.NewBaselineSnapshot(nil))

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.False(t, rt.hasRetryTimer(), "boundary-blocked permission failures should not arm the ordinary retry timer")
	assert.Empty(t, actionableObservationIssuesForTest(t, eng.baseline, t.Context()))

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/file.txt", retryRows[0].Path)
	assert.Equal(t, syncdomain.SKPermRemoteWrite(""), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
	assert.Zero(t, retryRows[0].NextRetryAt)

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, syncdomain.SKPermRemoteWrite(""), blockScopes[0].Key)
}

// Validates: R-2.10.33, R-2.14.1
func TestEngineFlow_ProcessNormalDecision_KnownRemoteBoundaryNoOpDoesNotPersistOrArmRetryTimer(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKPermRemoteWrite("Shared")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	}))

	r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:       "Shared/file.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "still read-only",
	})

	ready, err := flow.applyNormalCompletionDecision(t.Context(), &syncdomain.ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  syncdomain.ConditionRemoteWriteDenied,
		ConditionType: syncdomain.IssueRemoteWriteDenied,
	}, nil, r, syncdomain.NewBaselineSnapshot(nil))

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
	scopeKey := syncdomain.SKService()

	ready, err := flow.applyTrialCompletionDecision(t.Context(), scopeKey, &syncdomain.ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  syncdomain.ConditionInvalidFilename,
		ConditionType: syncdomain.IssueInvalidFilename,
		Persistence:   syncdomain.PersistRetryWork,
		TrialHint:     syncdomain.TrialHintReclassify,
	}, nil, &syncdomain.ActionCompletion{
		Path:          "trial.txt",
		ActionType:    syncdomain.ActionUpload,
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

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{
		ListItemPermissionsFn: func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
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
	scopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	blockedRow := &syncdomain.RetryWorkRow{
		Path:         "file.txt",
		ActionType:   syncdomain.ActionUpload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), blockedRow))
	flow.retryWork.rows[blockedRow.WorkKey()] = *blockedRow

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "file.txt",
		DriveID: driveid.New(syncfixture.EngineTestDriveID),
	}, 1, nil)
	require.NotNil(t, current)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), scopeKey, &syncdomain.ResultDecision{
		Class:         errclass.ClassActionable,
		ConditionKey:  syncdomain.ConditionRemoteWriteDenied,
		ConditionType: syncdomain.IssueRemoteWriteDenied,
		Persistence:   syncdomain.PersistRetryWork,
		TrialHint:     syncdomain.TrialHintReclassify,
	}, current, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    syncdomain.ActionUpload,
		HTTPStatus:    http.StatusForbidden,
		ErrMsg:        "permission evidence inconclusive",
		IsTrial:       true,
		TrialScopeKey: scopeKey,
	}), bl)

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
	trialScopeKey := syncdomain.SKService()
	throttleScopeKey := syncdomain.SKThrottleDrive(eng.driveID)

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "file.txt",
		ActionType:   syncdomain.ActionUpload,
		ScopeKey:     trialScopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "file.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), trialScopeKey, &syncdomain.ResultDecision{
		Class:             errclass.ClassScopeBlockingTransient,
		ConditionKey:      syncdomain.ConditionRateLimited,
		ConditionType:     syncdomain.IssueRateLimited,
		ScopeKey:          throttleScopeKey,
		ScopeEvidence:     throttleScopeKey,
		Persistence:       syncdomain.PersistRetryWork,
		RunScopeDetection: true,
		TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
	}, current, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    syncdomain.ActionUpload,
		DriveID:       eng.driveID,
		HTTPStatus:    http.StatusTooManyRequests,
		RetryAfter:    time.Minute,
		ErrMsg:        "throttled",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}), nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.sched.HasScope(throttleScopeKey),
		"trial retry fallback should activate the newly classified blocked scope")
	assert.False(t, rt.sched.HasScope(trialScopeKey),
		"the old trial scope should be discarded once its blocked work is reclassified")
	assert.True(t, rt.hasTrialTimer(), "new blocked scope should arm the watch trial timer")

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, throttleScopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
}

// Validates: R-2.10.3, R-2.10.5
//
// A throttle trial answered by a 503 with Retry-After moves its work to the
// service scope it activates and discards the throttle scope it was the only
// blocked work of, in one step: activated first, the service scope's
// invariant check met an empty throttle scope (found by the chaos Graph
// layer, D-092).
func TestEngineFlow_ProcessTrialDecision_ServiceRetryAfterMovesTheTrialsOnlyWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	trialScopeKey := syncdomain.SKThrottleDrive(eng.driveID)

	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: trialScopeKey, NextTrialAt: eng.nowFunc()})
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "file.txt",
		ActionType:   syncdomain.ActionDownload,
		ScopeKey:     trialScopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))
	current := rt.sched.AddForTest(&syncdomain.Action{Type: syncdomain.ActionDownload, Path: "file.txt", DriveID: eng.driveID}, 1, nil)
	require.NotNil(t, current)

	completion := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    syncdomain.ActionDownload,
		DriveID:       eng.driveID,
		HTTPStatus:    http.StatusServiceUnavailable,
		RetryAfter:    time.Minute,
		ErrMsg:        "service unavailable",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	})
	decision := classifyResult(completion)
	ready, err := flow.applyTrialCompletionDecision(t.Context(), trialScopeKey, &decision, current, completion, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.sched.HasScope(syncdomain.SKService()))
	assert.False(t, rt.sched.HasScope(trialScopeKey), "the throttle scope's only work moved away")
	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, syncdomain.SKService(), retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)
	require.NoError(t, flow.assertPersistedInvariants(t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FallbackKeepsOriginalScopeWithRemainingBlockedWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	flow := testEngineFlow(t, eng)
	trialScopeKey := syncdomain.SKService()
	throttleScopeKey := syncdomain.SKThrottleDrive(driveid.New("drive-throttle"))

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	upsertBlockedRetryWorkForTest(t, eng, "file.txt", trialScopeKey)
	upsertBlockedRetryWorkForTest(t, eng, "other.txt", trialScopeKey)

	other := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "other.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, other)
	rt.sched.Hold(other, syncsched.HeldScope, trialScopeKey, time.Time{})

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "file.txt",
		DriveID: eng.driveID,
	}, 2, nil)
	require.NotNil(t, current)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), trialScopeKey, &syncdomain.ResultDecision{
		Class:             errclass.ClassScopeBlockingTransient,
		ConditionKey:      syncdomain.ConditionRateLimited,
		ConditionType:     syncdomain.IssueRateLimited,
		ScopeKey:          throttleScopeKey,
		ScopeEvidence:     throttleScopeKey,
		Persistence:       syncdomain.PersistRetryWork,
		RunScopeDetection: true,
		TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
	}, current, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      current.ID,
		Path:          "file.txt",
		ActionType:    syncdomain.ActionUpload,
		DriveID:       eng.driveID,
		HTTPStatus:    http.StatusTooManyRequests,
		RetryAfter:    time.Minute,
		ErrMsg:        "throttled",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}), nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.True(t, rt.sched.HasScope(throttleScopeKey),
		"trial retry fallback should activate the newly classified blocked scope")
	assert.True(t, rt.sched.HasScope(trialScopeKey),
		"the original scope must stay active while unrelated blocked work still remains under it")
	assert.True(t, rt.sched.IsHeld(syncdomain.RetryWorkKeyForAction(&other.Action)),
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
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)
	flow := testEngineFlow(t, eng)
	trialScopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           trialScopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc(),
	})

	row := syncdomain.RetryWorkRow{
		Path:         "trial-stale.txt",
		ActionType:   syncdomain.ActionUpload,
		ScopeKey:     trialScopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &row))
	flow.retryWork.rows[row.WorkKey()] = row

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "trial-stale.txt",
		DriveID: eng.driveID,
	}, 1, nil)
	require.NotNil(t, current)
	rt.sched.Running(current)

	decision := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: syncdomain.ErrActionPreconditionChanged}))
	ready, err := flow.applyTrialCompletionDecision(t.Context(), trialScopeKey, &decision, current, syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      current.ID,
		Path:          "trial-stale.txt",
		ActionType:    syncdomain.ActionUpload,
		Err:           syncdomain.ErrActionPreconditionChanged,
		ErrMsg:        "source changed",
		IsTrial:       true,
		TrialScopeKey: trialScopeKey,
	}), nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
	assert.Equal(t, 0, rt.sched.InFlight())
	assert.Equal(t, 0, rt.sched.RunningCount())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
	assert.False(t, isTestBlockScopeed(eng, trialScopeKey), "empty trial scope should be discarded, not extended")

	batch := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
}

func upsertBlockedRetryWorkForTest(t *testing.T, eng *testEngine, path string, scopeKey syncdomain.ScopeKey) {
	t.Helper()

	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         path,
		ActionType:   syncdomain.ActionUpload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))
}

func retryRowsByPathForTest(t *testing.T, eng *testEngine) map[string]syncdomain.RetryWorkRow {
	t.Helper()

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 2)

	rowsByPath := make(map[string]syncdomain.RetryWorkRow, len(retryRows))
	for i := range retryRows {
		row := retryRows[i]
		rowsByPath[row.Path] = row
	}

	return rowsByPath
}

func blockScopesByKeyForTest(t *testing.T, eng *testEngine) map[syncdomain.ScopeKey]syncdomain.BlockScope {
	t.Helper()

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)

	byKey := make(map[syncdomain.ScopeKey]syncdomain.BlockScope, len(blockScopes))
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
	scopeKey := syncdomain.SKService()

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           scopeKey,
		NextTrialAt:   eng.nowFunc().Add(-time.Second),
		TrialInterval: time.Minute,
	})

	current := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, current)

	dependent := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "dependent.txt",
		DriveID: eng.driveID,
		ItemID:  "dependent-item",
		View:    &syncdomain.PathView{Path: "dependent.txt"},
	}, 2, []int64{1})
	assert.Nil(t, dependent)

	ready, err := rt.applyRuntimeCompletionStage(t.Context(), &syncdomain.ActionCompletion{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    syncdomain.ActionUpload,
		DriveID:       eng.driveID,
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: scopeKey,
	}, syncdomain.NewBaselineSnapshot(nil))

	require.NoError(t, err)
	require.Len(t, ready, 1)
	assert.Equal(t, int64(2), ready[0].ID)
	assert.Equal(t, "dependent.txt", ready[0].Action.Path)
	assert.False(t, rt.sched.HasScope(scopeKey))
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_ShutdownCompletesWithoutTerminating(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), syncdomain.SKService(), &syncdomain.ResultDecision{
		TrialHint: syncdomain.TrialHintShutdown,
	}, nil, &syncdomain.ActionCompletion{
		Path:       "trial-shutdown.txt",
		ActionType: syncdomain.ActionUpload,
	}, nil)

	require.NoError(t, err)
	assert.Empty(t, ready)
}

// Validates: R-2.10.5
func TestEngineFlow_ProcessTrialDecision_FatalTerminatesWithFatalResultError(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	ready, err := flow.applyTrialCompletionDecision(t.Context(), syncdomain.SKService(), &syncdomain.ResultDecision{
		Class:        errclass.ClassFatal,
		ConditionKey: syncdomain.ConditionAuthenticationRequired,
		TrialHint:    syncdomain.TrialHintFatal,
	}, nil, &syncdomain.ActionCompletion{
		Path:       "trial-auth.txt",
		ActionType: syncdomain.ActionUpload,
	}, nil)

	require.EqualError(t, err, "sync: unauthorized action completion for trial-auth.txt")
	assert.Empty(t, ready)
}

// Validates: R-6.8
func TestEngineFlow_ProcessActionCompletion_RetryPersistenceFailureTerminatesAndMarksFinished(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	current := flow.sched.AddForTest(&syncdomain.Action{
		Type: syncdomain.ActionUpload,
		Path: "retry.txt",
	}, 1, nil)
	require.NotNil(t, current)
	flow.sched.Running(current)

	require.NoError(t, eng.baseline.Close(t.Context()))

	ready, err := flow.applyRuntimeCompletionStage(t.Context(), syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "retry.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusBadGateway,
		ErrMsg:     "temporary outage",
	}), nil)

	require.ErrorContains(t, err, "record retry_work")
	assert.Empty(t, ready)
	assert.Zero(t, flow.sched.RunningCount(), "control-state persistence failure must still finish the completed action bookkeeping")
	assert.Zero(t, flow.sched.RunningCount())
	assert.Equal(t, 1, flow.sched.InFlight(), "the unresolved action must remain in the graph when the runtime fails closed")
}

// A 403 whose permission check cannot read the persisted boundaries fails the
// completion with that store failure, as every other store failure of the
// stage does, instead of going on to record it as ordinary retry work.
//
// Validates: R-2.14.1
func TestEngineFlow_ProcessActionCompletion_BoundaryReadFailureFailsTheCompletion(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	current := flow.sched.AddForTest(&syncdomain.Action{Type: syncdomain.ActionUpload, Path: "Shared/file.txt"}, 1, nil)
	require.NotNil(t, current)
	flow.sched.Running(current)

	require.NoError(t, eng.baseline.Close(t.Context()))

	ready, err := flow.applyRuntimeCompletionStage(t.Context(), syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "Shared/file.txt",
		ActionType: syncdomain.ActionUpload,
		HTTPStatus: http.StatusForbidden,
		ErrMsg:     "folder is read-only",
	}), syncdomain.NewBaselineSnapshot(nil))

	require.ErrorContains(t, err, "list remote block scopes")
	assert.NotContains(t, err.Error(), "record retry_work", "the failure is the boundary read, not a later write")
	assert.Empty(t, ready)
	assert.Zero(t, flow.sched.RunningCount(), "the completed action's bookkeeping is finished")
}

// A scope trial's 403 whose permission check cannot read the persisted
// boundaries fails the completion with that store failure too, instead of
// going on to record the trial as ordinary retry work.
//
// Validates: R-2.14.1
func TestEngineFlow_ProcessTrialDecision_BoundaryReadFailureFailsTheCompletion(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKService()
	setTestBlockScope(t, eng, &syncdomain.BlockScope{Key: scopeKey, NextTrialAt: eng.nowFunc()})
	current := flow.sched.AddForTest(&syncdomain.Action{Type: syncdomain.ActionUpload, Path: "Shared/file.txt"}, 1, nil)
	require.NotNil(t, current)
	flow.sched.Running(current)

	require.NoError(t, eng.baseline.Close(t.Context()))

	ready, err := flow.applyRuntimeCompletionStage(t.Context(), syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		ActionID:      1,
		Path:          "Shared/file.txt",
		ActionType:    syncdomain.ActionUpload,
		HTTPStatus:    http.StatusForbidden,
		ErrMsg:        "folder is read-only",
		IsTrial:       true,
		TrialScopeKey: scopeKey,
	}), syncdomain.NewBaselineSnapshot(nil))

	require.ErrorContains(t, err, "list remote block scopes")
	assert.NotContains(t, err.Error(), "record retry_work", "the failure is the boundary read, not a later write")
	assert.Empty(t, ready)
	assert.Zero(t, flow.sched.RunningCount(), "the completed action's bookkeeping is finished")
}

// Validates: R-2.10.66, R-2.14.1
func TestEngineFlow_Quota403OnSharedMountIsQuotaNotPermission(t *testing.T) {
	t.Parallel()

	probes := 0
	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{
		ListItemPermissionsFn: func(context.Context, driveid.ID, string) ([]graph.Permission, error) {
			probes++
			return []graph.Permission{{Roles: []string{"read"}}}, nil
		},
	})
	const quotaMountRoot = "quota-mount-root"
	eng.remoteRootItemID = quotaMountRoot
	eng.permHandler.remoteRootItemID = quotaMountRoot
	setupWatchEngine(t, eng)
	flow := testEngineFlow(t, eng)

	for _, path := range []string{"Shared/a.txt", "Shared/b.txt", "Shared/c.txt"} {
		r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
			Path:       path,
			ActionType: syncdomain.ActionUpload,
			DriveID:    driveid.New(syncfixture.EngineTestDriveID),
			HTTPStatus: http.StatusForbidden,
			Err: &graph.GraphError{
				StatusCode: http.StatusForbidden, Code: "accessDenied", InnerCodes: []string{"quotaLimitReached"},
				Err: graph.ErrForbidden,
			},
		})
		decision := classifyResult(r)
		_, err := flow.applyNormalCompletionDecision(t.Context(), &decision, nil, r, syncdomain.NewBaselineSnapshot(nil))
		require.NoError(t, err)
	}

	assert.Zero(t, probes, "a full drive is not a permission boundary: no ListItemPermissions")
	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	keys := make([]syncdomain.ScopeKey, 0, len(blockScopes))
	for _, scope := range blockScopes {
		keys = append(keys, scope.Key)
	}
	assert.Contains(t, keys, syncdomain.SKQuotaOwn())
	assert.NotContains(t, keys, syncdomain.SKPermRemoteWrite(""))
}
