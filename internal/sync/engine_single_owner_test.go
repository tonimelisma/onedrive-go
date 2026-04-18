package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type countingDriveVerifier struct {
	drive *graph.Drive
	err   error
	calls int
}

func (m *countingDriveVerifier) Drive(_ context.Context, _ driveid.ID) (*graph.Drive, error) {
	m.calls++

	return m.drive, m.err
}

// newSingleOwnerEngine creates a minimal engine with DepGraph plus the
// watch-mode active-scope working set for testing the single-owner engine
// methods. Uses a real SyncStore (in-memory SQLite).
func newSingleOwnerEngine(t *testing.T) *testEngine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)

	return eng
}

func newSingleOwnerEngineWithContext(t *testing.T, ctx context.Context) *testEngine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngineWithContext(t, ctx, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	setupWatchEngine(t, eng)

	return eng
}

// ---------------------------------------------------------------------------
// cascadeRecordAndComplete
// ---------------------------------------------------------------------------

func TestEngine_CascadeRecordAndComplete_SingleAction(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	// Add a single action to the graph.
	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := testWatchRuntime(t, eng).depGraph.Add(&action, 1, nil)
	require.NotNil(t, ta, "action should be immediately ready")

	// Cascade-record it as scope-blocked.
	cascadeRecordAndCompleteForTest(t, eng, ctx, ta, SKQuotaOwn())

	// Verify it was completed in the graph.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// Verify sync_failure was recorded with scope_key.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "test.txt", failures[0].Path)
	assert.Equal(t, SKQuotaOwn(), failures[0].ScopeKey)
	assert.Equal(t, int64(0), failures[0].NextRetryAt, "scope-blocked failure should have next_retry_at = 0 (NULL)")
}

func TestEngine_CascadeRecordAndComplete_WithDependents(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent action.
	parent := Action{
		Type:    ActionFolderCreate,
		Path:    "dir",
		DriveID: driveID,
	}
	parentTA := testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)
	require.NotNil(t, parentTA)

	// Add child that depends on parent.
	child := Action{
		Type:    ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveID,
	}
	childTA := testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})
	assert.Nil(t, childTA, "child should wait on parent")

	// Cascade-record parent → child should also be recorded.
	cascadeRecordAndCompleteForTest(t, eng, ctx, parentTA, SKQuotaOwn())

	// Both should be completed.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// Both should have sync_failures.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 2)
}

// ---------------------------------------------------------------------------
// releaseScope
// ---------------------------------------------------------------------------

// Validates: R-2.10.11
func TestEngine_ReleaseScope(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	sk := SKQuotaOwn()

	// Create a scope block.
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       sk,
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	// Create scope-blocked failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: DirectionUpload,
		Role:     FailureRoleHeld,
		Category: CategoryTransient, ScopeKey: sk,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: DirectionUpload,
		Role:     FailureRoleHeld,
		Category: CategoryTransient, ScopeKey: sk,
	}, nil))

	// Clear the scope.
	require.NoError(t, releaseTestScope(t, eng, ctx, sk))

	// Scope block should be gone.
	assert.False(t, isTestScopeBlocked(eng, sk))

	// Failures should now be retryable.
	now := eng.nowFn()
	rows := readyRetryStateForTest(t, eng.baseline, ctx, now)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")
}

// Validates: R-2.10.11
func TestEngine_ReleaseScope_SignalsImmediateRetrySweep(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := SKQuotaOwn()
	recorder := attachDebugEventRecorder(eng)

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "blocked.txt",
		DriveID:   driveid.New("drive1"),
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "scope blocked",
	}, nil))

	require.NoError(t, releaseTestScope(t, eng, ctx, scopeKey))

	select {
	case <-testWatchRuntime(t, eng).retryTimerCh:
	case <-time.After(time.Second):
		require.Fail(t, "releaseScope should signal retryTimerCh for due-now failures")
	}

	assert.Equal(t, []engineDebugEventType{
		engineDebugEventScopeReleased,
		engineDebugEventRetryKicked,
	}, recorder.eventTypesSnapshot())
}

func TestEngine_AssertCurrentScopeInvariants_DetectsDuplicateActiveScopes(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := SKService()

	testWatchRuntime(t, eng).replaceActiveScopes([]ScopeBlock{
		{Key: scopeKey, IssueType: IssueServiceOutage},
		{Key: scopeKey, IssueType: IssueServiceOutage},
	})

	err := assertTestCurrentScopeInvariants(t, eng, ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate active scope key")
}

func TestEngine_AssertCurrentScopeInvariants_DetectsOrphanedPermissionScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          scopeKey,
		IssueType:    IssueRemoteWriteDenied,
		TimingSource: ScopeTimingNone,
		BlockedAt:    eng.nowFn(),
	}))

	err := assertTestCurrentScopeInvariants(t, eng, ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persisted remote-write scope")
}

func TestEngine_DrainingDispatchAdmissionPanicsWithQueuedOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.enterDraining()

	outbox := []*TrackedAction{{
		ID: 1,
		Action: Action{
			Type:    ActionUpload,
			Path:    "queued.txt",
			DriveID: eng.driveID,
		},
	}}

	require.PanicsWithValue(t,
		"dispatch channel for outbox: draining runtime must not attempt to admit 1 queued actions",
		func() {
			rt.replaceOutbox(outbox)
			rt.dispatchChannelForOutbox()
		})
}

func TestEngine_RunRetrierSweepPanicsAfterDrainBegins(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.enterDraining()

	bl, safety := testWorkDispatchState(t, eng, t.Context())

	require.PanicsWithValue(t,
		"run retrier sweep: runRetrierSweep must not start after drain begins",
		func() {
			rt.runRetrierSweep(t.Context(), bl, SyncBidirectional, safety)
		})
}

func TestEngine_RunTrialDispatchPanicsAfterDrainBegins(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.enterDraining()

	bl, safety := testWorkDispatchState(t, eng, t.Context())

	require.PanicsWithValue(t,
		"run trial dispatch: runTrialDispatch must not start after drain begins",
		func() {
			rt.runTrialDispatch(t.Context(), bl, SyncBidirectional, safety)
		})
}

func TestEngine_ReconcileBookkeepingPanicWhenStillActiveAfterShutdownDrop(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.reconcileActive = true

	require.PanicsWithValue(t,
		"test reconcile bookkeeping: draining reconcile bookkeeping must be cleared before continuing",
		func() {
			testEngineFlow(t, eng).mustAssertReconcileBookkeepingCleared(rt, "test reconcile bookkeeping")
		})
}

func TestEngine_DrainingObserverExitCannotBeTreatedAsFatal(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	rt.enterDraining()

	require.PanicsWithValue(t,
		"handle observer exit: draining runtime must not treat observer exit as fatal outside shutdown",
		func() {
			err := rt.handleObserverExit(&watchPipeline{activeObs: 1}, false)
			require.NoError(t, err)
		})
}

func TestEngine_ReleaseAndDiscardScope_MaintainInvariantsInOneShotMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("release", func(t *testing.T) {
		eng := newSingleOwnerEngineWithContext(t, ctx)
		scopeKey := SKPermRemoteWrite("Shared/Docs")

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &ScopeBlock{
			Key:          scopeKey,
			IssueType:    IssueRemoteWriteDenied,
			TimingSource: ScopeTimingNone,
			BlockedAt:    eng.nowFn(),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      "Shared/Docs",
			DriveID:   driveid.New("drive1"),
			Direction: DirectionUpload,
			Role:      FailureRoleBoundary,
			Category:  CategoryActionable,
			IssueType: IssueRemoteWriteDenied,
			ScopeKey:  scopeKey,
			ErrMsg:    "read-only boundary",
		}, nil))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      "Shared/Docs/file.txt",
			DriveID:   driveid.New("drive1"),
			Direction: DirectionUpload,
			Role:      FailureRoleHeld,
			Category:  CategoryTransient,
			ScopeKey:  scopeKey,
			ErrMsg:    "held by scope",
		}, nil))

		clearTestWatchRuntime(eng)

		require.NoError(t, releaseTestScope(t, eng, ctx, scopeKey))
		require.NoError(t, assertReleasedScopeForTest(t, eng, ctx, scopeKey))
		require.NoError(t, assertTestCurrentScopeInvariants(t, eng, ctx))
	})

	t.Run("discard", func(t *testing.T) {
		eng := newSingleOwnerEngineWithContext(t, ctx)
		scopeKey := SKQuotaOwn()

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &ScopeBlock{
			Key:           scopeKey,
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     eng.nowFn(),
			TrialInterval: time.Minute,
			NextTrialAt:   eng.nowFn().Add(time.Minute),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      "Shared/Docs/file.txt",
			DriveID:   driveid.New("drive1"),
			Direction: DirectionUpload,
			Role:      FailureRoleHeld,
			Category:  CategoryTransient,
			ScopeKey:  scopeKey,
			ErrMsg:    "held by scope",
		}, nil))

		clearTestWatchRuntime(eng)

		require.NoError(t, discardTestScope(t, eng, ctx, scopeKey))
		require.NoError(t, assertDiscardedScopeForTest(t, eng, ctx, scopeKey))
		require.NoError(t, assertTestCurrentScopeInvariants(t, eng, ctx))
	})
}

type normalizePersistedScopesCase struct {
	name       string
	scopeBlock ScopeBlock
	seed       func(t *testing.T, env normalizePersistedScopesEnv)
	verify     func(t *testing.T, env normalizePersistedScopesEnv)
}

type normalizePersistedScopesEnv struct {
	eng *testEngine
	ctx func() context.Context
	now time.Time
}

func newNormalizePersistedScopesEnv(t *testing.T) normalizePersistedScopesEnv {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	ctx := t.Context()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	eng.nowFn = func() time.Time { return now }

	return normalizePersistedScopesEnv{
		eng: eng,
		ctx: func() context.Context { return ctx },
		now: now,
	}
}

func runNormalizePersistedScopesCase(t *testing.T, tc *normalizePersistedScopesCase) {
	t.Helper()
	t.Parallel()

	env := newNormalizePersistedScopesEnv(t)
	block := tc.scopeBlock
	if block.BlockedAt.IsZero() {
		block.BlockedAt = env.now.Add(-time.Minute)
	}
	require.NoError(t, env.eng.baseline.UpsertScopeBlock(env.ctx(), &block))
	if tc.seed != nil {
		tc.seed(t, env)
	}

	require.NoError(t, normalizePersistedScopesForTest(t, env.eng, env.ctx()))
	tc.verify(t, env)
}

func requireTrialDispatchScheduled(t *testing.T, env normalizePersistedScopesEnv) {
	t.Helper()

	newTestWatchState(t, env.eng)
	require.NoError(t, loadActiveScopesForTest(t, env.eng, env.ctx()))
	testWatchRuntime(t, env.eng).armTrialTimer()

	select {
	case <-testWatchRuntime(t, env.eng).trialCh:
	case <-time.After(time.Second):
		require.Fail(t, "expired server-timed scope should schedule an immediate trial on startup")
	}
}

func quotaNormalizePersistedScopesCases() []*normalizePersistedScopesCase {
	return []*normalizePersistedScopesCase{
		quotaNormalizeCaseWithHeldFailure(),
		quotaNormalizeCaseWithActivePreserve(),
		quotaNormalizeCaseWithExpiredPreserve(),
		quotaNormalizeCaseWithRehomedCandidate(),
		quotaNormalizeCaseWithActionableCandidate(),
	}
}

func quotaNormalizeCaseWithHeldFailure() *normalizePersistedScopesCase {
	return &normalizePersistedScopesCase{
		name: "keeps scoped quota with failures",
		scopeBlock: ScopeBlock{
			Key:           SKQuotaOwn(),
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
		},
		seed: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &SyncFailureParams{
				Path:      "upload.txt",
				DriveID:   driveid.New("drive1"),
				Direction: DirectionUpload,
				Role:      FailureRoleHeld,
				Category:  CategoryTransient,
				ScopeKey:  SKQuotaOwn(),
				ErrMsg:    "quota blocked",
			}, nil))
		},
		verify: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, SKQuotaOwn()))
		},
	}
}

func quotaNormalizeCaseWithActivePreserve() *normalizePersistedScopesCase {
	return &normalizePersistedScopesCase{
		name: "preserves empty scoped quota while preserve deadline is active",
		scopeBlock: ScopeBlock{
			Key:           SKQuotaOwn(),
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
		},
		verify: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, SKQuotaOwn()))
		},
	}
}

func quotaNormalizeCaseWithExpiredPreserve() *normalizePersistedScopesCase {
	return &normalizePersistedScopesCase{
		name: "discards empty scoped quota after preserve deadline expires",
		scopeBlock: ScopeBlock{
			Key:           SKQuotaOwn(),
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
		},
		verify: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			assert.False(t, isTestScopeBlocked(env.eng, SKQuotaOwn()))

			failures, err := env.eng.baseline.ListSyncFailures(env.ctx())
			require.NoError(t, err)
			assert.Empty(t, failures)
		},
	}
}

func quotaNormalizeCaseWithRehomedCandidate() *normalizePersistedScopesCase {
	return &normalizePersistedScopesCase{
		name: "preserved quota survives after candidate rehomes to a more specific scope",
		scopeBlock: ScopeBlock{
			Key:           SKQuotaOwn(),
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			TrialInterval: 45 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
		},
		seed: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &SyncFailureParams{
				Path:       "Shared/Docs",
				DriveID:    driveid.New("drive1"),
				Direction:  DirectionUpload,
				Role:       FailureRoleBoundary,
				Category:   CategoryActionable,
				IssueType:  IssueRemoteWriteDenied,
				HTTPStatus: http.StatusForbidden,
				ScopeKey:   SKPermRemoteWrite("Shared/Docs"),
				ErrMsg:     "boundary rehomed from preserved quota candidate",
			}, nil))
		},
		verify: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, SKQuotaOwn()))
		},
	}
}

func quotaNormalizeCaseWithActionableCandidate() *normalizePersistedScopesCase {
	return &normalizePersistedScopesCase{
		name: "preserved quota survives after candidate becomes actionable item failure",
		scopeBlock: ScopeBlock{
			Key:           SKQuotaOwn(),
			IssueType:     IssueQuotaExceeded,
			TimingSource:  ScopeTimingBackoff,
			TrialInterval: 45 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
		},
		seed: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &SyncFailureParams{
				Path:      "upload.txt",
				DriveID:   driveid.New("drive1"),
				Direction: DirectionUpload,
				Role:      FailureRoleItem,
				Category:  CategoryActionable,
				IssueType: IssueInvalidFilename,
				ErrMsg:    "candidate became actionable while original scope stayed preserved",
			}, nil))
		},
		verify: func(t *testing.T, env normalizePersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, SKQuotaOwn()))
		},
	}
}

// Validates: R-2.10.5
func TestEngine_NormalizePersistedScopes_QuotaPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range quotaNormalizePersistedScopesCases() {
		t.Run(tc.name, func(t *testing.T) {
			runNormalizePersistedScopesCase(t, tc)
		})
	}
}

func throttleAndServiceNormalizePersistedScopesCases() []*normalizePersistedScopesCase {
	return []*normalizePersistedScopesCase{
		{
			name: "keeps server timed throttle and schedules immediate trial when overdue",
			scopeBlock: ScopeBlock{
				Key:           testThrottleScope(),
				IssueType:     IssueRateLimited,
				TimingSource:  ScopeTimingServerRetryAfter,
				TrialInterval: 20 * time.Second,
				NextTrialAt:   time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
			},
			verify: func(t *testing.T, env normalizePersistedScopesEnv) {
				t.Helper()
				requireTrialDispatchScheduled(t, env)
			},
		},
		{
			name: "releases preserved non server timed service scope",
			scopeBlock: ScopeBlock{
				Key:           SKService(),
				IssueType:     IssueServiceOutage,
				TimingSource:  ScopeTimingBackoff,
				TrialInterval: 20 * time.Second,
				NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
				PreserveUntil: time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
			},
			verify: func(t *testing.T, env normalizePersistedScopesEnv) {
				t.Helper()
				assert.False(t, isTestScopeBlocked(env.eng, SKService()))
			},
		},
		{
			name: "keeps server timed service scope",
			scopeBlock: ScopeBlock{
				Key:           SKService(),
				IssueType:     IssueServiceOutage,
				TimingSource:  ScopeTimingServerRetryAfter,
				TrialInterval: time.Minute,
				NextTrialAt:   time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC),
			},
			verify: func(t *testing.T, env normalizePersistedScopesEnv) {
				t.Helper()
				assert.True(t, isTestScopeBlocked(env.eng, SKService()))
			},
		},
	}
}

// Validates: R-2.10.5
func TestEngine_NormalizePersistedScopes_ThrottleAndServicePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range throttleAndServiceNormalizePersistedScopesCases() {
		t.Run(tc.name, func(t *testing.T) {
			runNormalizePersistedScopesCase(t, tc)
		})
	}
}

// Validates: R-2.10.5
func TestEngine_NormalizePersistedScopes_IgnoresUnknownLegacyScopeKeysWithoutDeletingUnscopedFailures(t *testing.T) {
	t.Parallel()

	env := newNormalizePersistedScopesEnv(t)
	ctx := env.ctx()

	_, err := env.eng.baseline.db.ExecContext(
		ctx,
		`INSERT INTO scope_blocks
			(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth:account",
		IssueUnauthorized,
		ScopeTimingNone,
		env.now.UnixNano(),
		int64(0),
		int64(0),
		int64(0),
		0,
	)
	require.NoError(t, err)

	require.NoError(t, env.eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "keep.txt",
		DriveID:   driveid.New("drive1"),
		Direction: DirectionUpload,
		Role:      FailureRoleItem,
		Category:  CategoryActionable,
		IssueType: IssueInvalidFilename,
		ErrMsg:    "keep unrelated failure",
	}, nil))

	require.NoError(t, normalizePersistedScopesForTest(t, env.eng, ctx))

	blocks, err := env.eng.baseline.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	failures, err := env.eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "keep.txt", failures[0].Path)
	assert.True(t, failures[0].ScopeKey.IsZero())
}

// Validates: R-2.10.45, R-2.10.46
func TestEngine_PrepareRunOnceState_RevalidatesPersistedCatalogAuthRequirement(t *testing.T) {
	t.Parallel()

	t.Run("successful probe clears persisted catalog auth requirement", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		verifier := &countingDriveVerifier{
			drive: &graph.Drive{ID: eng.driveID},
		}
		eng.driveVerifier = verifier

		setCatalogAuthRequirementForTest(t, eng, authstate.ReasonSyncAuthRejected)

		runner := newOneShotRunner(eng.Engine)
		err := runner.prepareRunOnceState(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, verifier.calls, "startup auth repair should use exactly one proof call")
		assert.Empty(t, loadCatalogAuthRequirementForTest(t, eng))
	})

	t.Run("unauthorized probe keeps catalog auth requirement and aborts startup", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		verifier := &countingDriveVerifier{err: graph.ErrUnauthorized}
		eng.driveVerifier = verifier

		setCatalogAuthRequirementForTest(t, eng, authstate.ReasonSyncAuthRejected)

		runner := newOneShotRunner(eng.Engine)
		err := runner.prepareRunOnceState(ctx)
		require.ErrorIs(t, err, graph.ErrUnauthorized)
		assert.Equal(t, 1, verifier.calls)
		assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))
	})

	t.Run("non auth probe error leaves catalog auth requirement untouched", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		probeErr := errors.New("drive probe failed")
		verifier := &countingDriveVerifier{err: probeErr}
		eng.driveVerifier = verifier

		setCatalogAuthRequirementForTest(t, eng, authstate.ReasonSyncAuthRejected)

		runner := newOneShotRunner(eng.Engine)
		err := runner.prepareRunOnceState(ctx)
		require.ErrorIs(t, err, probeErr)
		assert.Equal(t, 1, verifier.calls)
		assert.Equal(t, authstate.ReasonSyncAuthRejected, loadCatalogAuthRequirementForTest(t, eng))
	})
}

func TestEngine_NormalizePersistedScopes_DiskPolicy(t *testing.T) {
	t.Parallel()

	t.Run("releases recovered disk scope", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &engineMockClient{})
		ctx := t.Context()
		now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		eng.nowFn = func() time.Time { return now }
		eng.minFreeSpace = 1024
		eng.diskAvailableFn = func(string) (uint64, error) { return 4096, nil }

		scopeKey := SKDiskLocal()
		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &ScopeBlock{
			Key:           scopeKey,
			IssueType:     IssueDiskFull,
			TimingSource:  ScopeTimingBackoff,
			BlockedAt:     now.Add(-time.Minute),
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(time.Minute),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      "download.bin",
			DriveID:   driveid.New("drive1"),
			Direction: DirectionDownload,
			Role:      FailureRoleHeld,
			Category:  CategoryTransient,
			ScopeKey:  scopeKey,
			ErrMsg:    "disk full",
		}, nil))

		require.NoError(t, normalizePersistedScopesForTest(t, eng, ctx))

		assert.False(t, isTestScopeBlocked(eng, scopeKey))
		retryable := readyRetryStateForTest(t, eng.baseline, ctx, now)
		require.Len(t, retryable, 1)
	})

	t.Run("refreshes unhealthy disk scope from current truth", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &engineMockClient{})
		ctx := t.Context()
		now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		eng.nowFn = func() time.Time { return now }
		eng.minFreeSpace = 4096
		eng.diskAvailableFn = func(string) (uint64, error) { return 512, nil }

		scopeKey := SKDiskLocal()
		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &ScopeBlock{
			Key:           scopeKey,
			IssueType:     IssueDiskFull,
			TimingSource:  ScopeTimingServerRetryAfter,
			BlockedAt:     now.Add(-10 * time.Minute),
			TrialInterval: 10 * time.Minute,
			NextTrialAt:   now.Add(10 * time.Minute),
			TrialCount:    7,
		}))

		require.NoError(t, normalizePersistedScopesForTest(t, eng, ctx))

		block, ok := getTestScopeBlock(eng, scopeKey)
		require.True(t, ok)
		assert.Equal(t, ScopeTimingBackoff, block.TimingSource)
		assert.Equal(t, diskScopeInitialTrialInterval, block.TrialInterval)
		assert.Equal(t, now, block.BlockedAt)
		assert.Equal(t, now.Add(diskScopeInitialTrialInterval), block.NextTrialAt)
		assert.Zero(t, block.TrialCount)
	})
}

// ---------------------------------------------------------------------------
// admitReady — active-scope admission checks
// ---------------------------------------------------------------------------

func TestEngine_AdmitReady_OneShotMode_NoActiveScopes(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	// nil watch → one-shot mode, all actions pass through.
	clearTestWatchRuntime(eng)

	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := testEngineFlow(t, eng).depGraph.Add(&action, 1, nil)

	dispatched := admitReadyForTest(t, eng, ctx, []*TrackedAction{ta})
	assert.Len(t, dispatched, 1, "without watch-mode active scopes, action should pass through")
}

func TestEngine_AdmitReady_ScopeBlocked(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	// Set up a scope block.
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       SKQuotaOwn(),
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn(),
	})

	action := Action{
		Type:    ActionUpload,
		Path:    "test.txt",
		DriveID: eng.driveID, // own drive
	}
	ta := testWatchRuntime(t, eng).depGraph.Add(&action, 1, nil)

	dispatched := admitReadyForTest(t, eng, ctx, []*TrackedAction{ta})
	assert.Empty(t, dispatched, "scope-blocked action should not be dispatched")

	// Action should be completed in graph (cascade).
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// sync_failure should exist.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 1)
}

func TestDispatchCurrentPlan_StaleTrialClearsOnlySelectedHeldRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := SKQuotaOwn()

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "trial.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   scopeKey,
		IssueType:  IssueQuotaExceeded,
		ErrMsg:     "held upload",
	}, nil))

	other := retryStateIdentityForWork("trial.txt", "old.txt", ActionRemoteMove)
	other.ScopeKey = scopeKey
	other.Blocked = true
	other.AttemptCount = 2
	other.FirstSeenAt = eng.nowFn().UnixNano()
	other.LastSeenAt = eng.nowFn().UnixNano()
	require.NoError(t, eng.baseline.UpsertRetryState(ctx, &other))

	plan := &ActionPlan{
		Actions: []Action{{
			Type:    ActionDownload,
			Path:    "trial.txt",
			DriveID: eng.driveID,
		}},
		Deps: [][]int{{}},
	}

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	outbox, accepted, err := testWatchRuntime(t, eng).dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{
		trialScopeKey: scopeKey,
		trialPath:     "trial.txt",
		trialWork: RetryWorkKey{
			Path:       "trial.txt",
			ActionType: ActionUpload,
		},
	})
	require.NoError(t, err)
	assert.False(t, accepted)
	assert.Empty(t, outbox)

	rows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "trial.txt", rows[0].Path)
	assert.Equal(t, ActionRemoteMove, rows[0].ActionType)
	assert.Equal(t, "old.txt", rows[0].OldPath)
}

func TestEngine_AdmitReady_TrialMismatchClearsOnlySelectedHeldRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := SKQuotaOwn()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueQuotaExceeded,
		BlockedAt: eng.nowFn(),
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "trial.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionDownload,
		ActionType: ActionDownload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ScopeKey:   scopeKey,
		IssueType:  IssueQuotaExceeded,
		ErrMsg:     "held download",
	}, nil))

	other := retryStateIdentityForWork("trial.txt", "", ActionUpload)
	other.ScopeKey = scopeKey
	other.Blocked = true
	other.AttemptCount = 2
	other.FirstSeenAt = eng.nowFn().UnixNano()
	other.LastSeenAt = eng.nowFn().UnixNano()
	require.NoError(t, eng.baseline.UpsertRetryState(ctx, &other))

	action := Action{
		Type:    ActionDownload,
		Path:    "trial.txt",
		DriveID: eng.driveID,
	}
	ta := testWatchRuntime(t, eng).depGraph.Add(&action, 1, nil)
	require.NotNil(t, ta)
	ta.IsTrial = true
	ta.TrialScopeKey = scopeKey

	dispatched := admitReadyForTest(t, eng, ctx, []*TrackedAction{ta})
	require.Len(t, dispatched, 1)
	assert.Equal(t, "trial.txt", dispatched[0].Action.Path)
	assert.Equal(t, ActionDownload, dispatched[0].Action.Type)

	rows, err := eng.baseline.ListRetryState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "trial.txt", rows[0].Path)
	assert.Equal(t, ActionUpload, rows[0].ActionType)
	assert.True(t, isTestScopeBlocked(eng, scopeKey))
}

func TestEngine_DispatchCurrentPlan_PublicationOnlySyncedUpdateCommitsWithoutOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	plan := &ActionPlan{
		Actions: []Action{{
			Type:    ActionUpdateSynced,
			Path:    "publish-synced.txt",
			DriveID: eng.driveID,
			View: &PathView{
				Remote: &RemoteState{
					ItemID:   "remote-item",
					DriveID:  eng.driveID,
					ParentID: "root",
					ItemType: ItemTypeFile,
					Hash:     "remote-hash",
					Size:     42,
					Mtime:    99,
					ETag:     "etag-1",
				},
				Local: &LocalState{
					Hash:  "local-hash",
					Size:  42,
					Mtime: 100,
				},
			},
		}},
		Deps: [][]int{{}},
	}

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	outbox, accepted, err := testWatchRuntime(t, eng).dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{})
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Empty(t, outbox)
	assert.Zero(t, testWatchRuntime(t, eng).depGraph.InFlightCount())

	entry, ok := eng.baseline.Baseline().GetByPath("publish-synced.txt")
	require.True(t, ok)
	assert.Equal(t, "remote-item", entry.ItemID)
	assert.Equal(t, "local-hash", entry.LocalHash)
	assert.Equal(t, "remote-hash", entry.RemoteHash)
}

func TestEngine_DispatchCurrentPlan_PublicationOnlyActionReleasesDependentOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	plan := &ActionPlan{
		Actions: []Action{
			{
				Type:    ActionUpdateSynced,
				Path:    "publish-parent.txt",
				DriveID: eng.driveID,
				View: &PathView{
					Remote: &RemoteState{
						ItemID:   "publish-parent-item",
						DriveID:  eng.driveID,
						ParentID: "root",
						ItemType: ItemTypeFile,
						Hash:     "remote-hash",
						Size:     42,
						Mtime:    99,
						ETag:     "etag-1",
					},
					Local: &LocalState{
						Hash:  "local-hash",
						Size:  42,
						Mtime: 100,
					},
				},
			},
			{
				Type:    ActionUpload,
				Path:    "publish-child.txt",
				DriveID: eng.driveID,
			},
		},
		Deps: [][]int{
			{},
			{0},
		},
	}

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	outbox, accepted, err := testWatchRuntime(t, eng).dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{})
	require.NoError(t, err)
	assert.True(t, accepted)
	require.Len(t, outbox, 1)
	assert.Equal(t, "publish-child.txt", outbox[0].Action.Path)
	assert.Equal(t, ActionUpload, outbox[0].Action.Type)

	entry, ok := eng.baseline.Baseline().GetByPath("publish-parent.txt")
	require.True(t, ok)
	assert.Equal(t, "publish-parent-item", entry.ItemID)
}

func TestEngine_DispatchCurrentPlan_PublicationOnlyCleanupCommitsWithoutOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "publish-cleanup.txt",
		DriveID:         eng.driveID,
		ItemID:          "cleanup-item",
		ItemType:        ItemTypeFile,
		LocalHash:       "hash",
		RemoteHash:      "hash",
		LocalSize:       9,
		LocalSizeKnown:  true,
		RemoteSize:      9,
		RemoteSizeKnown: true,
		LocalMtime:      1,
		RemoteMtime:     1,
	}))

	plan := &ActionPlan{
		Actions: []Action{{
			Type:    ActionCleanup,
			Path:    "publish-cleanup.txt",
			DriveID: eng.driveID,
			View: &PathView{
				Baseline: &BaselineEntry{
					Path:     "publish-cleanup.txt",
					DriveID:  eng.driveID,
					ItemID:   "cleanup-item",
					ItemType: ItemTypeFile,
				},
			},
		}},
		Deps: [][]int{{}},
	}

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	outbox, accepted, err := testWatchRuntime(t, eng).dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{})
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Empty(t, outbox)
	assert.Zero(t, testWatchRuntime(t, eng).depGraph.InFlightCount())

	_, ok := eng.baseline.Baseline().GetByPath("publish-cleanup.txt")
	assert.False(t, ok)
}

func TestEngine_DispatchCurrentPlan_PublicationOnlyCommitFailureCountsAsFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	require.NoError(t, eng.baseline.CommitObservation(ctx, nil, "", eng.driveID))

	plan := &ActionPlan{
		Actions: []Action{{
			Type:    ActionCleanup,
			Path:    "publish-failure.txt",
			DriveID: driveid.New("other-drive"),
		}},
		Deps: [][]int{{}},
	}

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	outbox, accepted, err := testWatchRuntime(t, eng).dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{})
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.Empty(t, outbox)
	assert.Zero(t, testWatchRuntime(t, eng).depGraph.InFlightCount())
	assert.Equal(t, 1, testEngineFlow(t, eng).failed)

	_, ok := eng.baseline.Baseline().GetByPath("publish-failure.txt")
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// processActionCompletion — success path
// ---------------------------------------------------------------------------

func TestEngine_ProcessAndRoute_Success(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent + child to DepGraph.
	parent := Action{Type: ActionUpload, Path: "parent.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)

	child := Action{Type: ActionUpload, Path: "child.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate successful result for parent.
	r := &ActionCompletion{
		Path:       "parent.txt",
		DriveID:    driveID,
		ActionType: ActionUpload,
		Success:    true,
		ActionID:   1,
	}

	dispatched := processActionCompletionForTest(t, eng, ctx, r, bl)

	// Child should be returned as ready (no scope gate → dispatched).
	assert.Len(t, dispatched, 1)
	assert.Equal(t, "child.txt", dispatched[0].Action.Path)

	// Succeeded counter should increment.
	assert.Equal(t, 1, testEngineFlow(t, eng).succeeded)
}

func TestEngine_ProcessAndRoute_FailureCascadesChildren(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent + child.
	parent := Action{Type: ActionFolderCreate, Path: "dir", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)

	child := Action{Type: ActionUpload, Path: "dir/file.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate failed result for parent.
	r := &ActionCompletion{
		Path:       "dir",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		Success:    false,
		ErrMsg:     "network error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processActionCompletionForTest(t, eng, ctx, r, bl)

	// Child should NOT be dispatched — it's cascade-recorded.
	assert.Empty(t, dispatched)

	// Both actions should be completed.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// Child should have a cascade sync_failure.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	// At least the child's cascade failure + parent's failure = 2.
	assert.GreaterOrEqual(t, len(failures), 2)
}

// ---------------------------------------------------------------------------
// Grandchild cascade tests (Fix 1: BFS prevents grandchild stranding)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestCascadeFailAndComplete_Grandchildren(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// 3-level chain: A → B → C
	a := Action{Type: ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := Action{Type: ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := Action{Type: ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B and C should both be cascade-failed and completed.
	r := &ActionCompletion{
		Path:       "a",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processActionCompletionForTest(t, eng, ctx, r, bl)
	assert.Empty(t, dispatched, "no actions should be dispatched on failure")

	// All 3 actions should be completed — none stranded.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount(),
		"grandchild must not be stranded in DepGraph")

	// B and C should both have cascade sync_failures.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	// Parent's failure + B's cascade + C's cascade = 3.
	assert.GreaterOrEqual(t, len(failures), 3)
}

// Validates: R-6.8.9
func TestCompleteSubtree_Grandchildren(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// 3-level chain: A → B → C
	a := Action{Type: ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := Action{Type: ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := Action{Type: ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Shutdown parent A — B and C should be silently completed.
	r := &ActionCompletion{
		Path:       "a",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		Success:    false,
		Err:        context.Canceled,
		ActionID:   1,
	}

	dispatched := processActionCompletionForTest(t, eng, ctx, r, bl)
	assert.Empty(t, dispatched)

	// All 3 actions should be completed.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount(),
		"grandchild must not be stranded on shutdown")

	// No cascade failures should be recorded (shutdown is not a failure).
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "shutdown should not record failures")
}

// Validates: R-2.10.5
func TestCascadeFailAndComplete_Diamond(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Diamond: A → B, A → C, B → D, C → D
	a := Action{Type: ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := Action{Type: ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := Action{Type: ActionFolderCreate, Path: "a/c", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{1})

	d := Action{Type: ActionDownload, Path: "a/d.txt", DriveID: driveID, ItemID: "id"}
	testWatchRuntime(t, eng).depGraph.Add(&d, 4, []int64{2, 3})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B, C, and D should all be cascade-failed.
	r := &ActionCompletion{
		Path:       "a",
		DriveID:    driveID,
		ActionType: ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processActionCompletionForTest(t, eng, ctx, r, bl)
	assert.Empty(t, dispatched)

	// All 4 actions should be completed — D completed exactly once.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount(),
		"diamond dependency must not strand any action")
}

// ---------------------------------------------------------------------------
// DepGraph.Done
// ---------------------------------------------------------------------------

func TestDepGraph_DoneClosesWhenAllComplete(t *testing.T) {
	t.Parallel()
	dg := NewDepGraph(testLogger(t))

	action1 := Action{Type: ActionUpload, Path: "a.txt"}
	action2 := Action{Type: ActionUpload, Path: "b.txt"}

	dg.Add(&action1, 1, nil)
	dg.Add(&action2, 2, nil)

	// Done should not be closed yet.
	select {
	case <-dg.Done():
		require.Fail(t, "Done should not be closed before all actions complete")
	default:
	}

	dg.Complete(1)

	// Still not done.
	select {
	case <-dg.Done():
		require.Fail(t, "Done should not be closed with 1 action remaining")
	default:
	}

	dg.Complete(2)

	// Now it should be closed.
	select {
	case <-dg.Done():
		// expected
	case <-time.After(time.Second):
		require.Fail(t, "Done should be closed when all actions are complete")
	}
}

func TestEngineFlow_CompleteDepGraphActionPanicsOnUnknownID(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := newEngineFlow(eng.Engine)
	flow.depGraph = NewDepGraph(eng.logger)

	require.PanicsWithValue(t,
		"dep_graph: complete unknown action ID 99 during unit test",
		func() {
			flow.completeDepGraphAction(99, "unit test")
		},
	)
}

// ---------------------------------------------------------------------------
// runRetrierSweep
// ---------------------------------------------------------------------------

func TestRetrierSweep_BatchLimit(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	now := eng.nowFn()

	// Align store clock with engine clock so next_retry_at is computed
	// relative to the same fixed time.
	eng.baseline.SetNowFunc(eng.nowFn)
	eng.retryBatchLimit = 64

	total := eng.effectiveRetryBatchLimit() + 5

	// Seed remote_state rows so createEventFromDB can build full events.
	// Each download failure needs a corresponding remote_state row.
	obs := make([]ObservedItem, total)
	for i := range total {
		obs[i] = ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%d", i),
			Path:     fmt.Sprintf("file-%d.txt", i),
			ItemType: ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%d", i),
			Size:     int64(i * 100),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed one full test-sized retry batch plus a few extra transient failures
	// with past next_retry_at so the mirrored retry_state queue must re-arm for
	// a second sweep. delayFn returns -1 minute so next_retry_at = now - 1m.
	for i := range total {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      fmt.Sprintf("file-%d.txt", i),
			DriveID:   driveID,
			Direction: DirectionDownload,
			Category:  CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Verify seeding — all mirrored retry_state rows should be retryable.
	rows, err := eng.baseline.ListRetryStateReady(ctx, now)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), total)

	outbox := runTestRetrierSweep(t, eng, ctx)

	// Should dispatch exactly one test-sized retry batch.
	assert.Len(t, outbox, eng.effectiveRetryBatchLimit(),
		"sweep should be batch-limited to the configured retry batch size")

	// retryTimerCh should have a signal for remaining items.
	select {
	case <-testWatchRuntime(t, eng).retryTimerCh:
		// Good — re-arm signal sent.
	default:
		require.Fail(t, "retryTimerCh should have a signal for remaining batch items")
	}
}

func TestRetrierSweep_SkipsInFlight(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Align store clock with engine clock.
	eng.baseline.SetNowFunc(eng.nowFn)

	names := []string{"a.txt", "b.txt", "c.txt"}

	// Seed remote_state rows so createEventFromDB can build full events.
	obs := make([]ObservedItem, len(names))
	for i, name := range names {
		obs[i] = ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%s", name),
			Path:     name,
			ItemType: ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%s", name),
			Size:     int64(100 * (i + 1)),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed 3 transient failures; RecordFailure mirrors them into retry_state.
	for _, name := range names {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      name,
			DriveID:   driveID,
			Direction: DirectionDownload,
			Category:  CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Add "b.txt" to the DepGraph so it's in-flight.
	testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "b.txt",
		DriveID: driveID,
		ItemID:  "in-flight-item",
	}, 1, nil)

	outbox := runTestRetrierSweep(t, eng, ctx)

	// Should dispatch 2 items (a.txt and c.txt), skipping b.txt.
	assert.Len(t, outbox, 2, "sweep should skip in-flight items")
}

// ---------------------------------------------------------------------------
// runTrialDispatch
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestTrialDispatch_NoCandidates_DiscardsScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	now := eng.nowFn()

	// Set a scope block with NextTrialAt in the past.
	sk := SKQuotaOwn()
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	// Do NOT seed any sync_failures for this scope — no candidates.
	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	assert.False(t, isTestScopeBlocked(eng, sk))

	blocks, err := eng.baseline.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// ---------------------------------------------------------------------------
// GetRemoteStateByPath
// ---------------------------------------------------------------------------

func TestGetRemoteStateByPath_Found(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Insert a remote_state row via CommitObservation.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     "docs/report.pdf",
			ParentID: "parent-1",
			ItemType: ItemTypeFile,
			Hash:     "xorhash-abc",
			Size:     4096,
			Mtime:    1000000000,
			ETag:     "etag-1",
		},
	}, "", driveID))

	row, found, err := eng.baseline.GetRemoteStateByPath(ctx, "docs/report.pdf", driveID)
	require.NoError(t, err)
	require.True(t, found, "should find the row")
	require.NotNil(t, row, "should find the row")

	assert.Equal(t, "item-abc", row.ItemID)
	assert.Equal(t, "docs/report.pdf", row.Path)
	assert.Equal(t, "parent-1", row.ParentID)
	assert.Equal(t, "xorhash-abc", row.Hash)
	assert.Equal(t, int64(4096), row.Size)
	assert.Equal(t, int64(1000000000), row.Mtime)
	assert.Equal(t, "etag-1", row.ETag)
}

func TestGetRemoteStateByPath_NotFound(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row, found, err := eng.baseline.GetRemoteStateByPath(ctx, "nonexistent.txt", driveID)
	require.NoError(t, err)
	assert.False(t, found, "missing path should report found=false")
	assert.Nil(t, row, "should return nil for missing path")
}

func TestGetRemoteStateByPath_NullableFields(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Insert with minimal fields (no hash, no size, no mtime).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-sparse",
			Path:     "folder/",
			ItemType: ItemTypeFolder,
		},
	}, "", driveID))

	row, found, err := eng.baseline.GetRemoteStateByPath(ctx, "folder/", driveID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, row)

	assert.Empty(t, row.Hash, "hash should be empty string from NULL")
	assert.Equal(t, int64(0), row.Size, "size should be 0 from NULL")
	assert.Equal(t, int64(0), row.Mtime, "mtime should be 0 from NULL")
}

// ---------------------------------------------------------------------------
// isFailureResolved
// ---------------------------------------------------------------------------

func TestIsFailureResolved_Download_RemoteMirrorMatchesBaseline(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state and set it to synced (simulates a download that
	// completed through the normal pipeline).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "resolved-item",
			Path:     "resolved.txt",
			ItemType: ItemTypeFile,
			Hash:     "resolved-hash",
			Size:     512,
		},
	}, "", driveID))

	require.NoError(t, eng.baseline.CommitMutation(ctx, mutationFromActionOutcome(&ActionOutcome{
		Action:          ActionDownload,
		Success:         true,
		Path:            "resolved.txt",
		DriveID:         driveID,
		ItemID:          "resolved-item",
		ItemType:        ItemTypeFile,
		LocalHash:       "resolved-hash",
		RemoteHash:      "resolved-hash",
		LocalSize:       512,
		LocalSizeKnown:  true,
		RemoteSize:      512,
		RemoteSizeKnown: true,
	})))

	// Seed a sync_failure for this path.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "resolved.txt",
		DriveID:   driveID,
		Direction: DirectionDownload,
		Category:  CategoryTransient,
	}, nil))

	row := &SyncFailureRow{
		Path:       "resolved.txt",
		DriveID:    driveID,
		Direction:  DirectionDownload,
		ActionType: ActionDownload,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download should be resolved when remote mirror truth already matches baseline")

	// The sync_failure should have been cleared.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "resolved failure should be cleared from DB")
}

func TestIsFailureResolved_Download_NoRemoteState(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &SyncFailureRow{
		Path:       "deleted-remotely.txt",
		DriveID:    driveID,
		Direction:  DirectionDownload,
		ActionType: ActionDownload,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download with no remote_state should be resolved")
}

func TestIsFailureResolved_Download_RemoteMirrorStillDrifted(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state without a converged baseline entry.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "still-pending.txt",
			ItemType: ItemTypeFile,
			Hash:     "pending-hash",
		},
	}, "", driveID))

	row := &SyncFailureRow{
		Path:       "still-pending.txt",
		DriveID:    driveID,
		Direction:  DirectionDownload,
		ActionType: ActionDownload,
	}

	assert.False(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download should not be resolved while remote mirror truth still differs from baseline")
}

func TestIsFailureResolved_Upload_FileGone(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &SyncFailureRow{
		Path:       "gone-upload.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"upload for non-existent file should be resolved")
}

func TestIsFailureResolved_Upload_FileExists(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a real file.
	require.NoError(t, os.WriteFile(
		filepath.Join(eng.syncRoot, "still-here.txt"),
		[]byte("content"),
		0o600,
	))

	row := &SyncFailureRow{
		Path:       "still-here.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
	}

	assert.False(t, isFailureResolvedForTest(t, eng, ctx, row),
		"upload for existing file should NOT be resolved")
}

func TestIsFailureResolved_Delete_NoBaseline(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &SyncFailureRow{
		Path:       "already-deleted.txt",
		DriveID:    driveID,
		Direction:  DirectionDelete,
		ActionType: ActionRemoteDelete,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"delete with no baseline entry should be resolved")
}

func TestIsFailureResolved_Delete_BaselineExists(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a baseline entry via a successful download outcome.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "baseline-item",
			Path:     "still-in-baseline.txt",
			ItemType: ItemTypeFile,
			Hash:     "bl-hash",
			Size:     100,
		},
	}, "", driveID))

	require.NoError(t, eng.baseline.CommitMutation(ctx, mutationFromActionOutcome(&ActionOutcome{
		Action:          ActionDownload,
		Success:         true,
		Path:            "still-in-baseline.txt",
		DriveID:         driveID,
		ItemID:          "baseline-item",
		ItemType:        ItemTypeFile,
		LocalHash:       "bl-hash",
		RemoteHash:      "bl-hash",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
	})))

	row := &SyncFailureRow{
		Path:       "still-in-baseline.txt",
		DriveID:    driveID,
		Direction:  DirectionDelete,
		ActionType: ActionRemoteDelete,
	}

	assert.False(t, isFailureResolvedForTest(t, eng, ctx, row),
		"delete with baseline entry should NOT be resolved")
}

// ---------------------------------------------------------------------------
// Integration: D-9 — retrier sweep creates full-fidelity events
// ---------------------------------------------------------------------------

// Validates: R-2.10.7
func TestRetrierSweep_FullFidelityEvents_D9(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	eng.baseline.SetNowFunc(eng.nowFn)

	// Seed remote_state with full metadata.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "d9-item",
			Path:     "d9-test.txt",
			ParentID: "d9-parent",
			ItemType: ItemTypeFile,
			Hash:     "d9-hash",
			Size:     9999,
			Mtime:    7777777777,
			ETag:     "d9-etag",
		},
	}, "", driveID))

	// Seed a sync_failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "d9-test.txt",
		DriveID:   driveID,
		Direction: DirectionDownload,
		Category:  CategoryTransient,
	}, func(_ int) time.Duration {
		return -time.Minute
	}))

	outbox := runTestRetrierSweep(t, eng, ctx)

	require.Len(t, outbox, 1)
	assert.Equal(t, ActionDownload, outbox[0].Action.Type)
	assert.Equal(t, "d9-test.txt", outbox[0].Action.Path)
	assert.Equal(t, driveID, outbox[0].Action.DriveID)
}

// ---------------------------------------------------------------------------
// Integration: D-11 — retrier sweep skips resolved failures
// ---------------------------------------------------------------------------

// Validates: R-2.10.7
func TestRetrierSweep_SkipsResolvedFailures_D11(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	eng.baseline.SetNowFunc(eng.nowFn)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "synced-item",
			Path:     "d11-synced.txt",
			ItemType: ItemTypeFile,
			Hash:     "synced-hash",
			Size:     100,
			Mtime:    101,
		},
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "d11-pending.txt",
			ItemType: ItemTypeFile,
			Hash:     "pending-hash",
			Size:     200,
			Mtime:    202,
		},
	}, "", driveID))

	require.NoError(t, eng.baseline.CommitMutation(ctx, mutationFromActionOutcome(&ActionOutcome{
		Action:          ActionDownload,
		Success:         true,
		Path:            "d11-synced.txt",
		DriveID:         driveID,
		ItemID:          "synced-item",
		ItemType:        ItemTypeFile,
		LocalHash:       "synced-hash",
		RemoteHash:      "synced-hash",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      100,
		RemoteSizeKnown: true,
		LocalMtime:      101,
		RemoteMtime:     101,
	})))

	// Seed sync_failures for both.
	for _, path := range []string{"d11-synced.txt", "d11-pending.txt"} {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
			Path:      path,
			DriveID:   driveID,
			Direction: DirectionDownload,
			Category:  CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	outbox := runTestRetrierSweep(t, eng, ctx)

	// Only d11-pending should be dispatched (d11-synced is resolved).
	require.Len(t, outbox, 1, "D-11: resolved failure should be skipped")
	assert.Equal(t, "d11-pending.txt", outbox[0].Action.Path)

	// The resolved failure should have been cleared from the DB.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)

	// Only d11-pending should remain.
	require.Len(t, failures, 1)
	assert.Equal(t, "d11-pending.txt", failures[0].Path)
}

// ---------------------------------------------------------------------------
// Integration: D-9 — trial dispatch uses engine-owned planner work
// ---------------------------------------------------------------------------

func TestTrialDispatch_UsesPlannerWorkRequest(t *testing.T) {
	t.Parallel()

	const trialPath = "trial.txt"

	eng := newSingleOwnerEngine(t)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	recorder := attachDebugEventRecorder(eng)

	ctx := context.Background()
	now := eng.nowFn()

	sk := SKQuotaOwn()

	// Set up a scope block with NextTrialAt in the past.
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	absPath := filepath.Join(eng.syncRoot, trialPath)
	require.NoError(t, os.WriteFile(absPath, []byte("trial payload"), 0o600))

	// Seed a scope-blocked failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      trialPath,
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	// Capture the scope block's TrialInterval before dispatch.
	blockBefore, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	intervalBefore := blockBefore.TrialInterval

	outbox := runTestTrialDispatch(t, eng, ctx)
	require.Len(t, outbox, 1)
	assert.Equal(t, trialPath, outbox[0].Action.Path)
	assert.Equal(t, ActionUpload, outbox[0].Action.Type)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, sk, outbox[0].TrialScopeKey)
	assert.True(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventTrialDispatched &&
			event.ScopeKey == sk &&
			event.Path == trialPath
	}))

	// After successful dispatch, the scope block's TrialInterval should NOT
	// be extended — interval stays unmutated until the action completion arrives.
	blockAfter, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	assert.Equal(t, intervalBefore, blockAfter.TrialInterval,
		"trial interval should NOT be extended after successful dispatch")
}

// Validates: R-2.10.5
func TestTrialDispatch_NoEventWhenCurrentStateMissingPreservesScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)

	ctx := context.Background()
	driveID := driveid.New("drive1")

	sk := SKQuotaOwn()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "missing.txt",
		DriveID:   driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  sk,
		ItemID:    "missing-item",
	}, nil))

	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox, "missing current state should not dispatch a trial action")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures, "resolved held candidate should be cleared from sync_failures")

	block, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	assert.Equal(t, 10*time.Second, block.TrialInterval)
	assert.Equal(t, eng.nowFn().Add(10*time.Second), block.NextTrialAt)
}

func TestRetrierSweep_UploadSkippedCandidateBecomesActionableFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	eng.baseline.SetNowFunc(eng.nowFn)

	file, err := os.Create(filepath.Join(eng.syncRoot, "oversized.bin"))
	require.NoError(t, err)
	require.NoError(t, file.Truncate(MaxOneDriveFileSize+1))
	require.NoError(t, file.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Category:  CategoryTransient,
	}, func(int) time.Duration {
		return -time.Minute
	}))

	outbox := runTestRetrierSweep(t, eng, ctx)
	assert.Empty(t, outbox)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, CategoryActionable, failures[0].Category)
	assert.Equal(t, FailureRoleItem, failures[0].Role)
	assert.Equal(t, IssueFileTooLarge, failures[0].IssueType)
}

func TestTrialDispatch_RandomBlockedRetrySelectionLeavesNonSelectedHeldFailuresUntouched(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	sk := SKQuotaOwn()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	oversized, err := os.Create(filepath.Join(eng.syncRoot, "oversized.bin"))
	require.NoError(t, err)
	require.NoError(t, oversized.Truncate(MaxOneDriveFileSize+1))
	require.NoError(t, oversized.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	absPath := filepath.Join(eng.syncRoot, "trial.txt")
	require.NoError(t, os.WriteFile(absPath, []byte("trial payload"), 0o600))

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	outbox := runTestTrialDispatch(t, eng, ctx)
	require.Len(t, outbox, 1)
	assert.Equal(t, "trial.txt", outbox[0].Action.Path)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 2)

	var oversizedSeen, heldTrial bool
	for i := range failures {
		switch failures[i].Path {
		case "oversized.bin":
			oversizedSeen = true
			if failures[i].Category == CategoryActionable {
				assert.Equal(t, IssueFileTooLarge, failures[i].IssueType)
			} else {
				assert.Equal(t, CategoryTransient, failures[i].Category)
				assert.Equal(t, FailureRoleHeld, failures[i].Role)
			}
		case "trial.txt":
			heldTrial = true
			assert.Equal(t, FailureRoleHeld, failures[i].Role)
		}
	}

	assert.True(t, oversizedSeen)
	assert.True(t, heldTrial)
	assert.True(t, isTestScopeBlocked(eng, sk))
}

// Validates: R-2.10.5
func TestTrialDispatch_OnlySkippedHeldCandidatesPreserveScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	sk := SKQuotaOwn()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           sk,
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	oversized, err := os.Create(filepath.Join(eng.syncRoot, "oversized.bin"))
	require.NoError(t, err)
	require.NoError(t, oversized.Truncate(MaxOneDriveFileSize+1))
	require.NoError(t, oversized.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, CategoryActionable, failures[0].Category)
	assert.Equal(t, IssueFileTooLarge, failures[0].IssueType)
	block, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	assert.Equal(t, 10*time.Second, block.TrialInterval)
	assert.Equal(t, eng.nowFn().Add(10*time.Second), block.NextTrialAt)
}

func TestEngine_ClearFailureCandidate_RemovesSyncFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &SyncFailureRow{
		Path:       "clear-me.txt",
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: DirectionUpload,
		Category:  CategoryTransient,
	}, nil))

	clearFailureCandidateForTest(t, eng, ctx, row, "TestEngine_ClearFailureCandidate_RemovesSyncFailure")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

// Validates: R-2.10.2
func TestEngine_RecordRetryTrialSkippedItem_ReasonlessSkipClearsFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &SyncFailureRow{
		Path:       "internal.tmp",
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: DirectionUpload,
		Category:  CategoryTransient,
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &SkippedItem{Path: row.Path})

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

// Validates: R-2.10.2
func TestEngine_RecordRetryTrialSkippedItem_ReasonlessSkipWithZeroDriveIDClearsEngineDriveFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &SyncFailureRow{
		Path:       "internal.tmp",
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      row.Path,
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		Category:  CategoryTransient,
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &SkippedItem{Path: row.Path})

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestEngine_RecordRetryTrialSkippedItem_ZeroDriveIDFallsBackToEngineDrive(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &SyncFailureRow{
		Path:      "oversized.bin",
		Direction: DirectionUpload,
	}

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &SkippedItem{
		Path:     row.Path,
		Reason:   IssueFileTooLarge,
		Detail:   "file size exceeds limit",
		FileSize: MaxOneDriveFileSize + 1,
	})

	failures := actionableSyncFailuresForTest(t, eng.baseline, ctx)
	require.Len(t, failures, 1)
	assert.Equal(t, row.Path, failures[0].Path)
	assert.Equal(t, eng.driveID, failures[0].DriveID)
	assert.Equal(t, CategoryActionable, failures[0].Category)
	assert.Equal(t, IssueFileTooLarge, failures[0].IssueType)
	assert.Equal(t, FailureRoleItem, failures[0].Role)
}

// Validates: R-2.11.5
func TestEngine_RecordRetryTrialSkippedItem_ReplacesExistingActionableIssueType(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &SyncFailureRow{
		Path:      "problem.txt",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: row.Direction,
		Category:  CategoryActionable,
		IssueType: IssueInvalidFilename,
		ErrMsg:    "contains ':'",
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &SkippedItem{
		Path:   row.Path,
		Reason: IssuePathTooLong,
		Detail: "path exceeds 400-character limit",
	})

	failures := actionableSyncFailuresForTest(t, eng.baseline, ctx)
	require.Len(t, failures, 1)
	assert.Equal(t, row.Path, failures[0].Path)
	assert.Equal(t, row.DriveID, failures[0].DriveID)
	assert.Equal(t, IssuePathTooLong, failures[0].IssueType)
	assert.Equal(t, "path exceeds 400-character limit", failures[0].LastError)
	assert.Equal(t, FailureRoleItem, failures[0].Role)
}

func TestTrialDispatch_DoesNotMutateStateWhenNoScopeIsDue(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	now := eng.nowFn()

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           SKQuotaOwn(),
		IssueType:     IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(time.Minute),
		TrialInterval: 10 * time.Second,
	})

	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	blocks, err := eng.baseline.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKQuotaOwn(), blocks[0].Key)
}
