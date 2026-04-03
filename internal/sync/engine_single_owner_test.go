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

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const cachedLocalHash = "cached-local-hash"

type countingDriveVerifier struct {
	drive *graph.Drive
	err   error
	calls int
}

func (m *countingDriveVerifier) Drive(_ context.Context, _ driveid.ID) (*graph.Drive, error) {
	m.calls++

	return m.drive, m.err
}

// newSingleOwnerEngine creates a minimal engine with syncdispatch.DepGraph plus the
// watch-mode active-scope working set for testing the single-owner engine
// methods. Uses a real syncstore.SyncStore (in-memory SQLite).
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
	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := &synctypes.TrackedAction{ID: 1, Action: action}
	require.NotNil(t, ta, "action should be immediately ready")

	// Cascade-record it as scope-blocked.
	cascadeRecordAndCompleteForTest(t, eng, ctx, ta, synctypes.SKQuotaOwn())

	// Verify it was completed in the graph.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// Verify sync_failure was recorded with scope_key.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, "test.txt", failures[0].Path)
	assert.Equal(t, synctypes.SKQuotaOwn(), failures[0].ScopeKey)
	assert.Equal(t, int64(0), failures[0].NextRetryAt, "scope-blocked failure should have next_retry_at = 0 (NULL)")
}

func TestEngine_CascadeRecordAndComplete_WithDependents(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent action.
	parent := synctypes.Action{
		Type:    synctypes.ActionFolderCreate,
		Path:    "dir",
		DriveID: driveID,
	}
	parentTA := testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)
	require.NotNil(t, parentTA)

	// Add child that depends on parent.
	child := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "dir/file.txt",
		DriveID: driveID,
	}
	childTA := testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})
	assert.Nil(t, childTA, "child should wait on parent")

	// Cascade-record parent → child should also be recorded.
	cascadeRecordAndCompleteForTest(t, eng, ctx, parentTA, synctypes.SKQuotaOwn())

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
	sk := synctypes.SKQuotaOwn()

	// Create a scope block.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       sk,
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	// Create scope-blocked failures.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "a.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleHeld,
		Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path: "b.txt", DriveID: driveID, Direction: synctypes.DirectionUpload,
		Role:     synctypes.FailureRoleHeld,
		Category: synctypes.CategoryTransient, ScopeKey: sk,
	}, nil))

	// Clear the scope.
	require.NoError(t, releaseTestScope(t, eng, ctx, sk))

	// Scope block should be gone.
	assert.False(t, isTestScopeBlocked(eng, sk))

	// Failures should now be retryable.
	now := eng.nowFn()
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "scope-blocked failures should now be retryable")
}

// Validates: R-2.10.11
func TestEngine_ReleaseScope_SignalsImmediateRetrySweep(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := synctypes.SKQuotaOwn()
	var events []engineDebugEventType
	eng.debugEventHook = func(event engineDebugEvent) {
		events = append(events, event.Type)
	}

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       scopeKey,
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "blocked.txt",
		DriveID:   driveid.New("drive1"),
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
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
	}, events)
}

func TestEngine_AssertCurrentScopeInvariants_DetectsDuplicateActiveScopes(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := synctypes.SKService()

	testWatchRuntime(t, eng).replaceActiveScopes([]synctypes.ScopeBlock{
		{Key: scopeKey, IssueType: synctypes.IssueServiceOutage},
		{Key: scopeKey, IssueType: synctypes.IssueServiceOutage},
	})

	err := assertTestCurrentScopeInvariants(t, eng, ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate active scope key")
}

func TestEngine_AssertCurrentScopeInvariants_DetectsOrphanedPermissionScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	scopeKey := synctypes.SKPermRemote("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:          scopeKey,
		IssueType:    synctypes.IssuePermissionDenied,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    eng.nowFn(),
	}))

	err := assertTestCurrentScopeInvariants(t, eng, ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy persisted perm:remote scope")
}

func TestEngine_ReleaseAndDiscardScope_MaintainInvariantsInOneShotMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("release", func(t *testing.T) {
		eng := newSingleOwnerEngineWithContext(t, ctx)
		scopeKey := synctypes.SKPermRemote("Shared/Docs")

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:          scopeKey,
			IssueType:    synctypes.IssuePermissionDenied,
			TimingSource: synctypes.ScopeTimingNone,
			BlockedAt:    eng.nowFn(),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      "Shared/Docs",
			DriveID:   driveid.New("drive1"),
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleBoundary,
			Category:  synctypes.CategoryActionable,
			IssueType: synctypes.IssuePermissionDenied,
			ScopeKey:  scopeKey,
			ErrMsg:    "read-only boundary",
		}, nil))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      "Shared/Docs/file.txt",
			DriveID:   driveid.New("drive1"),
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
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
		scopeKey := synctypes.SKQuotaShortcut("drive:item")

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:           scopeKey,
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     eng.nowFn(),
			TrialInterval: time.Minute,
			NextTrialAt:   eng.nowFn().Add(time.Minute),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      "Shared/Docs/file.txt",
			DriveID:   driveid.New("drive1"),
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
			ScopeKey:  scopeKey,
			ErrMsg:    "held by scope",
		}, nil))

		clearTestWatchRuntime(eng)

		require.NoError(t, discardTestScope(t, eng, ctx, scopeKey))
		require.NoError(t, assertDiscardedScopeForTest(t, eng, ctx, scopeKey))
		require.NoError(t, assertTestCurrentScopeInvariants(t, eng, ctx))
	})
}

func TestEngine_RepairPersistedScopes_ReleasesOrphanedRemotePermissionScope(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	ctx := t.Context()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	eng.nowFn = func() time.Time { return now }

	scopeKey := synctypes.SKPermRemote("Shared/Docs")
	require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
		Key:          scopeKey,
		IssueType:    synctypes.IssuePermissionDenied,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    now.Add(-time.Minute),
	}))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "Shared/Docs/file.txt",
		DriveID:   driveid.New("drive1"),
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "blocked by remote permission scope",
	}, nil))

	require.NoError(t, repairPersistedScopesForTest(t, eng, ctx))

	assert.False(t, isTestScopeBlocked(eng, scopeKey))

	retryable, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
	require.NoError(t, err)
	assert.Empty(t, retryable, "startup repair should forget remote blocked state when the blocked local work is already gone")

	remaining, err := eng.baseline.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

type repairPersistedScopesCase struct {
	name       string
	scopeBlock synctypes.ScopeBlock
	seed       func(t *testing.T, env repairPersistedScopesEnv)
	verify     func(t *testing.T, env repairPersistedScopesEnv)
}

type repairPersistedScopesEnv struct {
	eng *testEngine
	ctx func() context.Context
	now time.Time
}

func newRepairPersistedScopesEnv(t *testing.T) repairPersistedScopesEnv {
	t.Helper()

	eng, _ := newTestEngine(t, &engineMockClient{})
	ctx := t.Context()
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	eng.nowFn = func() time.Time { return now }

	return repairPersistedScopesEnv{
		eng: eng,
		ctx: func() context.Context { return ctx },
		now: now,
	}
}

func runRepairPersistedScopesCase(t *testing.T, tc *repairPersistedScopesCase) {
	t.Helper()
	t.Parallel()

	env := newRepairPersistedScopesEnv(t)
	block := tc.scopeBlock
	if block.BlockedAt.IsZero() {
		block.BlockedAt = env.now.Add(-time.Minute)
	}
	require.NoError(t, env.eng.baseline.UpsertScopeBlock(env.ctx(), &block))
	if tc.seed != nil {
		tc.seed(t, env)
	}

	require.NoError(t, repairPersistedScopesForTest(t, env.eng, env.ctx()))
	tc.verify(t, env)
}

func requireTrialDispatchScheduled(t *testing.T, env repairPersistedScopesEnv) {
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

func quotaRepairPersistedScopesCases() []*repairPersistedScopesCase {
	return []*repairPersistedScopesCase{
		quotaRepairCaseWithHeldFailure(),
		quotaRepairCaseWithActivePreserve(),
		quotaRepairCaseWithExpiredPreserve(),
		quotaRepairCaseWithRehomedCandidate(),
		quotaRepairCaseWithActionableCandidate(),
	}
}

func quotaRepairCaseWithHeldFailure() *repairPersistedScopesCase {
	return &repairPersistedScopesCase{
		name: "keeps scoped quota with failures",
		scopeBlock: synctypes.ScopeBlock{
			Key:           synctypes.SKQuotaOwn(),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
		},
		seed: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &synctypes.SyncFailureParams{
				Path:      "upload.txt",
				DriveID:   driveid.New("drive1"),
				Direction: synctypes.DirectionUpload,
				Role:      synctypes.FailureRoleHeld,
				Category:  synctypes.CategoryTransient,
				ScopeKey:  synctypes.SKQuotaOwn(),
				ErrMsg:    "quota blocked",
			}, nil))
		},
		verify: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, synctypes.SKQuotaOwn()))
		},
	}
}

func quotaRepairCaseWithActivePreserve() *repairPersistedScopesCase {
	return &repairPersistedScopesCase{
		name: "preserves empty scoped quota while preserve deadline is active",
		scopeBlock: synctypes.ScopeBlock{
			Key:           synctypes.SKQuotaShortcut("drive:item"),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 30, 0, time.UTC),
		},
		verify: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, synctypes.SKQuotaShortcut("drive:item")))
		},
	}
}

func quotaRepairCaseWithExpiredPreserve() *repairPersistedScopesCase {
	return &repairPersistedScopesCase{
		name: "discards empty scoped quota after preserve deadline expires",
		scopeBlock: synctypes.ScopeBlock{
			Key:           synctypes.SKQuotaShortcut("drive:item"),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			TrialInterval: 30 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
		},
		verify: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			assert.False(t, isTestScopeBlocked(env.eng, synctypes.SKQuotaShortcut("drive:item")))

			failures, err := env.eng.baseline.ListSyncFailures(env.ctx())
			require.NoError(t, err)
			assert.Empty(t, failures)
		},
	}
}

func quotaRepairCaseWithRehomedCandidate() *repairPersistedScopesCase {
	return &repairPersistedScopesCase{
		name: "preserved quota survives after candidate rehomes to a more specific scope",
		scopeBlock: synctypes.ScopeBlock{
			Key:           synctypes.SKQuotaOwn(),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			TrialInterval: 45 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
		},
		seed: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &synctypes.SyncFailureParams{
				Path:       "Shared/Docs",
				DriveID:    driveid.New("drive1"),
				Direction:  synctypes.DirectionUpload,
				Role:       synctypes.FailureRoleBoundary,
				Category:   synctypes.CategoryActionable,
				IssueType:  synctypes.IssuePermissionDenied,
				HTTPStatus: http.StatusForbidden,
				ScopeKey:   synctypes.SKPermRemote("Shared/Docs"),
				ErrMsg:     "boundary rehomed from preserved quota candidate",
			}, nil))
		},
		verify: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, synctypes.SKQuotaOwn()))
		},
	}
}

func quotaRepairCaseWithActionableCandidate() *repairPersistedScopesCase {
	return &repairPersistedScopesCase{
		name: "preserved quota survives after candidate becomes actionable item failure",
		scopeBlock: synctypes.ScopeBlock{
			Key:           synctypes.SKQuotaOwn(),
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			TrialInterval: 45 * time.Second,
			NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
			PreserveUntil: time.Date(2025, 6, 15, 12, 0, 45, 0, time.UTC),
		},
		seed: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &synctypes.SyncFailureParams{
				Path:      "upload.txt",
				DriveID:   driveid.New("drive1"),
				Direction: synctypes.DirectionUpload,
				Role:      synctypes.FailureRoleItem,
				Category:  synctypes.CategoryActionable,
				IssueType: synctypes.IssueInvalidFilename,
				ErrMsg:    "candidate became actionable while original scope stayed preserved",
			}, nil))
		},
		verify: func(t *testing.T, env repairPersistedScopesEnv) {
			t.Helper()
			assert.True(t, isTestScopeBlocked(env.eng, synctypes.SKQuotaOwn()))
		},
	}
}

// Validates: R-2.10.5
func TestEngine_RepairPersistedScopes_QuotaPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range quotaRepairPersistedScopesCases() {
		t.Run(tc.name, func(t *testing.T) {
			runRepairPersistedScopesCase(t, tc)
		})
	}
}

func throttleAndServiceRepairPersistedScopesCases() []*repairPersistedScopesCase {
	return []*repairPersistedScopesCase{
		{
			name: "keeps server timed throttle and schedules immediate trial when overdue",
			scopeBlock: synctypes.ScopeBlock{
				Key:           synctypes.SKThrottleAccount(),
				IssueType:     synctypes.IssueRateLimited,
				TimingSource:  synctypes.ScopeTimingServerRetryAfter,
				TrialInterval: 20 * time.Second,
				NextTrialAt:   time.Date(2025, 6, 15, 11, 59, 59, 0, time.UTC),
			},
			verify: func(t *testing.T, env repairPersistedScopesEnv) {
				t.Helper()
				requireTrialDispatchScheduled(t, env)
			},
		},
		{
			name: "releases non server timed throttle",
			scopeBlock: synctypes.ScopeBlock{
				Key:           synctypes.SKThrottleAccount(),
				IssueType:     synctypes.IssueRateLimited,
				TimingSource:  synctypes.ScopeTimingBackoff,
				TrialInterval: 20 * time.Second,
				NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
				PreserveUntil: time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
			},
			seed: func(t *testing.T, env repairPersistedScopesEnv) {
				t.Helper()
				require.NoError(t, env.eng.baseline.RecordFailure(env.ctx(), &synctypes.SyncFailureParams{
					Path:      "upload.txt",
					DriveID:   driveid.New("drive1"),
					Direction: synctypes.DirectionUpload,
					Role:      synctypes.FailureRoleHeld,
					Category:  synctypes.CategoryTransient,
					ScopeKey:  synctypes.SKThrottleAccount(),
					ErrMsg:    "rate limited",
				}, nil))
			},
			verify: func(t *testing.T, env repairPersistedScopesEnv) {
				t.Helper()
				assert.False(t, isTestScopeBlocked(env.eng, synctypes.SKThrottleAccount()))
				retryable, err := env.eng.baseline.ListSyncFailuresForRetry(env.ctx(), env.now)
				require.NoError(t, err)
				require.Len(t, retryable, 1)
				assert.Equal(t, synctypes.FailureRoleItem, retryable[0].Role)
			},
		},
		{
			name: "releases preserved non server timed service scope",
			scopeBlock: synctypes.ScopeBlock{
				Key:           synctypes.SKService(),
				IssueType:     synctypes.IssueServiceOutage,
				TimingSource:  synctypes.ScopeTimingBackoff,
				TrialInterval: 20 * time.Second,
				NextTrialAt:   time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
				PreserveUntil: time.Date(2025, 6, 15, 12, 0, 20, 0, time.UTC),
			},
			verify: func(t *testing.T, env repairPersistedScopesEnv) {
				t.Helper()
				assert.False(t, isTestScopeBlocked(env.eng, synctypes.SKService()))
			},
		},
		{
			name: "keeps server timed service scope",
			scopeBlock: synctypes.ScopeBlock{
				Key:           synctypes.SKService(),
				IssueType:     synctypes.IssueServiceOutage,
				TimingSource:  synctypes.ScopeTimingServerRetryAfter,
				TrialInterval: time.Minute,
				NextTrialAt:   time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC),
			},
			verify: func(t *testing.T, env repairPersistedScopesEnv) {
				t.Helper()
				assert.True(t, isTestScopeBlocked(env.eng, synctypes.SKService()))
			},
		},
	}
}

// Validates: R-2.10.5
func TestEngine_RepairPersistedScopes_ThrottleAndServicePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range throttleAndServiceRepairPersistedScopesCases() {
		t.Run(tc.name, func(t *testing.T) {
			runRepairPersistedScopesCase(t, tc)
		})
	}
}

// Validates: R-2.10.45, R-2.10.46
func TestEngine_PrepareRunOnceState_RevalidatesPersistedAuthScope(t *testing.T) {
	t.Parallel()

	t.Run("successful probe clears persisted auth scope", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		verifier := &countingDriveVerifier{
			drive: &graph.Drive{ID: eng.driveID},
		}
		eng.driveVerifier = verifier

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:          synctypes.SKAuthAccount(),
			IssueType:    synctypes.IssueUnauthorized,
			TimingSource: synctypes.ScopeTimingNone,
			BlockedAt:    eng.nowFunc(),
		}))

		runner := newOneShotRunner(eng.Engine)
		_, _, err := runner.prepareRunOnceState(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, verifier.calls, "startup auth repair should use exactly one proof call")
		assert.False(t, isTestScopeBlocked(eng, synctypes.SKAuthAccount()))
	})

	t.Run("unauthorized probe keeps auth scope and aborts startup", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		verifier := &countingDriveVerifier{err: graph.ErrUnauthorized}
		eng.driveVerifier = verifier

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:          synctypes.SKAuthAccount(),
			IssueType:    synctypes.IssueUnauthorized,
			TimingSource: synctypes.ScopeTimingNone,
			BlockedAt:    eng.nowFunc(),
		}))

		runner := newOneShotRunner(eng.Engine)
		_, _, err := runner.prepareRunOnceState(ctx)
		require.ErrorIs(t, err, graph.ErrUnauthorized)
		assert.Equal(t, 1, verifier.calls)
		assert.True(t, isTestScopeBlocked(eng, synctypes.SKAuthAccount()))
	})

	t.Run("non auth probe error leaves auth scope untouched", func(t *testing.T) {
		t.Parallel()

		eng := newSingleOwnerEngine(t)
		ctx := t.Context()
		probeErr := errors.New("drive probe failed")
		verifier := &countingDriveVerifier{err: probeErr}
		eng.driveVerifier = verifier

		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:          synctypes.SKAuthAccount(),
			IssueType:    synctypes.IssueUnauthorized,
			TimingSource: synctypes.ScopeTimingNone,
			BlockedAt:    eng.nowFunc(),
		}))

		runner := newOneShotRunner(eng.Engine)
		_, _, err := runner.prepareRunOnceState(ctx)
		require.ErrorIs(t, err, probeErr)
		assert.Equal(t, 1, verifier.calls)
		assert.True(t, isTestScopeBlocked(eng, synctypes.SKAuthAccount()))
	})
}

func TestEngine_RepairPersistedScopes_DiskPolicy(t *testing.T) {
	t.Parallel()

	t.Run("releases recovered disk scope", func(t *testing.T) {
		t.Parallel()

		eng, _ := newTestEngine(t, &engineMockClient{})
		ctx := t.Context()
		now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
		eng.nowFn = func() time.Time { return now }
		eng.minFreeSpace = 1024
		eng.diskAvailableFn = func(string) (uint64, error) { return 4096, nil }

		scopeKey := synctypes.SKDiskLocal()
		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:           scopeKey,
			IssueType:     synctypes.IssueDiskFull,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     now.Add(-time.Minute),
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(time.Minute),
		}))
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      "download.bin",
			DriveID:   driveid.New("drive1"),
			Direction: synctypes.DirectionDownload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
			ScopeKey:  scopeKey,
			ErrMsg:    "disk full",
		}, nil))

		require.NoError(t, repairPersistedScopesForTest(t, eng, ctx))

		assert.False(t, isTestScopeBlocked(eng, scopeKey))
		retryable, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
		require.NoError(t, err)
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

		scopeKey := synctypes.SKDiskLocal()
		require.NoError(t, eng.baseline.UpsertScopeBlock(ctx, &synctypes.ScopeBlock{
			Key:           scopeKey,
			IssueType:     synctypes.IssueDiskFull,
			TimingSource:  synctypes.ScopeTimingServerRetryAfter,
			BlockedAt:     now.Add(-10 * time.Minute),
			TrialInterval: 10 * time.Minute,
			NextTrialAt:   now.Add(10 * time.Minute),
			TrialCount:    7,
		}))

		require.NoError(t, repairPersistedScopesForTest(t, eng, ctx))

		block, ok := getTestScopeBlock(eng, scopeKey)
		require.True(t, ok)
		assert.Equal(t, synctypes.ScopeTimingBackoff, block.TimingSource)
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

	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: driveid.New("drive1"),
	}
	ta := testEngineFlow(t, eng).depGraph.Add(&action, 1, nil)

	dispatched := admitReadyForTest(t, eng, ctx, []*synctypes.TrackedAction{ta})
	assert.Len(t, dispatched, 1, "without watch-mode active scopes, action should pass through")
}

func TestEngine_AdmitReady_ScopeBlocked(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	// Set up a scope block.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       synctypes.SKQuotaOwn(),
		IssueType: synctypes.IssueQuotaExceeded,
		BlockedAt: eng.nowFn(),
	})

	action := synctypes.Action{
		Type:    synctypes.ActionUpload,
		Path:    "test.txt",
		DriveID: eng.driveID, // own drive
	}
	ta := testWatchRuntime(t, eng).depGraph.Add(&action, 1, nil)

	dispatched := admitReadyForTest(t, eng, ctx, []*synctypes.TrackedAction{ta})
	assert.Empty(t, dispatched, "scope-blocked action should not be dispatched")

	// Action should be completed in graph (cascade).
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())

	// sync_failure should exist.
	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 1)
}

// ---------------------------------------------------------------------------
// processWorkerResult — success path
// ---------------------------------------------------------------------------

func TestEngine_ProcessAndRoute_Success(t *testing.T) {
	t.Parallel()
	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Add parent + child to syncdispatch.DepGraph.
	parent := synctypes.Action{Type: synctypes.ActionUpload, Path: "parent.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)

	child := synctypes.Action{Type: synctypes.ActionUpload, Path: "child.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate successful result for parent.
	r := &synctypes.WorkerResult{
		Path:       "parent.txt",
		DriveID:    driveID,
		ActionType: synctypes.ActionUpload,
		Success:    true,
		ActionID:   1,
	}

	dispatched := processWorkerResultForTest(t, eng, ctx, r, bl)

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
	parent := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "dir", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&parent, 1, nil)

	child := synctypes.Action{Type: synctypes.ActionUpload, Path: "dir/file.txt", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&child, 2, []int64{1})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Simulate failed result for parent.
	r := &synctypes.WorkerResult{
		Path:       "dir",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		ErrMsg:     "network error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processWorkerResultForTest(t, eng, ctx, r, bl)

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
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B and C should both be cascade-failed and completed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processWorkerResultForTest(t, eng, ctx, r, bl)
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
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/b/c.txt", DriveID: driveID, ItemID: "ic"}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{2})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Shutdown parent A — B and C should be silently completed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		Err:        context.Canceled,
		ActionID:   1,
	}

	dispatched := processWorkerResultForTest(t, eng, ctx, r, bl)
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
	a := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&a, 1, nil)

	b := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/b", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&b, 2, []int64{1})

	c := synctypes.Action{Type: synctypes.ActionFolderCreate, Path: "a/c", DriveID: driveID}
	testWatchRuntime(t, eng).depGraph.Add(&c, 3, []int64{1})

	d := synctypes.Action{Type: synctypes.ActionDownload, Path: "a/d.txt", DriveID: driveID, ItemID: "id"}
	testWatchRuntime(t, eng).depGraph.Add(&d, 4, []int64{2, 3})

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	// Fail parent A — B, C, and D should all be cascade-failed.
	r := &synctypes.WorkerResult{
		Path:       "a",
		DriveID:    driveID,
		ActionType: synctypes.ActionFolderCreate,
		Success:    false,
		ErrMsg:     "server error",
		HTTPStatus: 500,
		ActionID:   1,
	}

	dispatched := processWorkerResultForTest(t, eng, ctx, r, bl)
	assert.Empty(t, dispatched)

	// All 4 actions should be completed — D completed exactly once.
	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount(),
		"diamond dependency must not strand any action")
}

// ---------------------------------------------------------------------------
// syncdispatch.DepGraph.Done
// ---------------------------------------------------------------------------

func TestDepGraph_DoneClosesWhenAllComplete(t *testing.T) {
	t.Parallel()
	dg := syncdispatch.NewDepGraph(testLogger(t))

	action1 := synctypes.Action{Type: synctypes.ActionUpload, Path: "a.txt"}
	action2 := synctypes.Action{Type: synctypes.ActionUpload, Path: "b.txt"}

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
	obs := make([]synctypes.ObservedItem, total)
	for i := range total {
		obs[i] = synctypes.ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%d", i),
			Path:     fmt.Sprintf("file-%d.txt", i),
			ItemType: synctypes.ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%d", i),
			Size:     int64(i * 100),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed one full test-sized retry batch plus a few extra sync_failures with
	// past next_retry_at so the retrier must re-arm for a second sweep. delayFn
	// returns -1 minute so next_retry_at = now - 1m (in the past).
	for i := range total {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      fmt.Sprintf("file-%d.txt", i),
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Verify seeding — all rows should be retryable.
	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, now)
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
	obs := make([]synctypes.ObservedItem, len(names))
	for i, name := range names {
		obs[i] = synctypes.ObservedItem{
			DriveID:  driveID,
			ItemID:   fmt.Sprintf("item-%s", name),
			Path:     name,
			ItemType: synctypes.ItemTypeFile,
			Hash:     fmt.Sprintf("hash-%s", name),
			Size:     int64(100 * (i + 1)),
		}
	}

	require.NoError(t, eng.baseline.CommitObservation(ctx, obs, "", driveID))

	// Seed 3 sync_failures.
	for _, name := range names {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      name,
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
		}, func(_ int) time.Duration {
			return -time.Minute
		}))
	}

	// Add "b.txt" to the syncdispatch.DepGraph so it's in-flight.
	testWatchRuntime(t, eng).depGraph.Add(&synctypes.Action{
		Type:    synctypes.ActionDownload,
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
func TestTrialDispatch_NoCandidates_PreservesScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	now := eng.nowFn()

	// Set a scope block with NextTrialAt in the past.
	sk := synctypes.SKQuotaOwn()
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	// Do NOT seed any sync_failures for this scope — no candidates.
	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	block, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	assert.Equal(t, 10*time.Second, block.TrialInterval)
	assert.Equal(t, now.Add(10*time.Second), block.NextTrialAt)
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
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-abc",
			Path:     "docs/report.pdf",
			ParentID: "parent-1",
			ItemType: synctypes.ItemTypeFile,
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
	assert.Equal(t, synctypes.SyncStatusPendingDownload, row.SyncStatus)
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
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-sparse",
			Path:     "folder/",
			ItemType: synctypes.ItemTypeFolder,
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
// remoteStateToChangeEvent
// ---------------------------------------------------------------------------

func TestRemoteStateToChangeEvent_Download(t *testing.T) {
	t.Parallel()

	rs := &synctypes.RemoteStateRow{
		DriveID:    driveid.New("drive1"),
		ItemID:     "item-42",
		Path:       "docs/file.txt",
		ParentID:   "parent-7",
		ItemType:   synctypes.ItemTypeFile,
		Hash:       "xorhash-42",
		Size:       8192,
		Mtime:      2000000000,
		ETag:       "etag-42",
		SyncStatus: synctypes.SyncStatusPendingDownload,
	}

	ev := remoteStateToChangeEvent(rs, "docs/file.txt")

	assert.Equal(t, synctypes.SourceRemote, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "docs/file.txt", ev.Path)
	assert.Equal(t, "item-42", ev.ItemID)
	assert.Equal(t, "parent-7", ev.ParentID)
	assert.Equal(t, driveid.New("drive1"), ev.DriveID)
	assert.Equal(t, synctypes.ItemTypeFile, ev.ItemType)
	assert.Equal(t, "file.txt", ev.Name)
	assert.Equal(t, int64(8192), ev.Size)
	assert.Equal(t, "xorhash-42", ev.Hash)
	assert.Equal(t, int64(2000000000), ev.Mtime)
	assert.Equal(t, "etag-42", ev.ETag)
	assert.False(t, ev.IsDeleted)
}

func TestRemoteStateToChangeEvent_Delete(t *testing.T) {
	t.Parallel()

	// Test all delete-family statuses.
	for _, status := range []synctypes.SyncStatus{synctypes.SyncStatusDeleted, synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed, synctypes.SyncStatusPendingDelete} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()

			rs := &synctypes.RemoteStateRow{
				DriveID:    driveid.New("drive1"),
				ItemID:     "item-del",
				Path:       "trash/old.txt",
				SyncStatus: status,
				ItemType:   synctypes.ItemTypeFile,
			}

			ev := remoteStateToChangeEvent(rs, "trash/old.txt")

			assert.Equal(t, synctypes.ChangeDelete, ev.Type)
			assert.True(t, ev.IsDeleted)
			assert.Equal(t, "old.txt", ev.Name)
		})
	}
}

func TestRemoteStateToChangeEvent_Folder(t *testing.T) {
	t.Parallel()

	rs := &synctypes.RemoteStateRow{
		DriveID:    driveid.New("drive1"),
		ItemID:     "item-folder",
		Path:       "photos/vacation",
		SyncStatus: synctypes.SyncStatusPendingDownload,
		ItemType:   synctypes.ItemTypeFolder,
	}

	ev := remoteStateToChangeEvent(rs, "photos/vacation")

	assert.Equal(t, synctypes.ItemTypeFolder, ev.ItemType)
	assert.Equal(t, "vacation", ev.Name)
}

// ---------------------------------------------------------------------------
// createEventFromDB
// ---------------------------------------------------------------------------

func TestCreateEventFromDB_Upload_FileExists(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Create a real file in the sync root.
	syncRoot := eng.syncRoot
	testFile := "upload-test.txt"
	require.NoError(t, os.WriteFile(
		filepath.Join(syncRoot, testFile),
		[]byte("hello world"),
		0o600,
	))

	row := &synctypes.SyncFailureRow{
		Path:      testFile,
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev := createEventFromDBForTest(t, eng, ctx, row)

	require.NotNil(t, ev, "should create event for existing file")
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, testFile, ev.Path)
	assert.Equal(t, "upload-test.txt", ev.Name)
	assert.Equal(t, synctypes.ItemTypeFile, ev.ItemType)
	assert.Positive(t, ev.Size, "size should be populated")
	assert.NotEmpty(t, ev.Hash, "hash should be computed")
	assert.Positive(t, ev.Mtime, "mtime should be populated")
}

// Validates: R-2.10.7
func TestCreateEventFromDB_Upload_ReusesBaselineHashWhenMetadataMatches(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")
	testFile := "upload-fast-path.txt"
	actualContent := []byte("actual data")
	cachedHash := cachedLocalHash
	oldTime := eng.nowFn().Add(-2 * time.Second)

	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, testFile), actualContent, 0o600))
	require.NoError(t, os.Chtimes(filepath.Join(eng.syncRoot, testFile), oldTime, oldTime))

	info, err := os.Stat(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)

	actualHash, err := syncobserve.ComputeStableHash(filepath.Join(eng.syncRoot, testFile))
	require.NoError(t, err)
	require.NotEqual(t, actualHash, cachedHash, "test needs a distinct cached hash to prove reuse")

	require.NoError(t, eng.baseline.CommitOutcome(ctx, &synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       testFile,
		DriveID:    driveID,
		ItemID:     "upload-fast-path-item",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  cachedHash,
		RemoteHash: cachedHash,
		Size:       info.Size(),
		Mtime:      info.ModTime().UnixNano(),
	}))

	ev := createEventFromDBForTest(t, eng, ctx, &synctypes.SyncFailureRow{
		Path:      testFile,
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	})

	require.NotNil(t, ev)
	assert.Equal(t, cachedHash, ev.Hash, "matching metadata outside the racily-clean window should reuse the baseline hash")
}

func TestCreateEventFromDB_Upload_FileGone(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:      "nonexistent-upload.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionUpload,
	}

	ev := createEventFromDBForTest(t, eng, ctx, row)

	assert.Nil(t, ev, "missing upload paths are treated as resolved retry candidates")
}

func TestCreateEventFromDB_Download_RemoteStateExists(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "dl-item",
			Path:     "download-test.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "dl-hash",
			Size:     1024,
			Mtime:    5000000000,
			ETag:     "dl-etag",
		},
	}, "", driveID))

	row := &synctypes.SyncFailureRow{
		Path:      "download-test.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	ev := createEventFromDBForTest(t, eng, ctx, row)

	require.NotNil(t, ev, "should create event from remote_state")
	assert.Equal(t, synctypes.SourceRemote, ev.Source)
	assert.Equal(t, synctypes.ChangeModify, ev.Type)
	assert.Equal(t, "download-test.txt", ev.Path)
	assert.Equal(t, "dl-item", ev.ItemID)
	assert.Equal(t, "dl-hash", ev.Hash)
	assert.Equal(t, int64(1024), ev.Size)
	assert.Equal(t, int64(5000000000), ev.Mtime)
	assert.Equal(t, "dl-etag", ev.ETag)
	assert.Equal(t, "download-test.txt", ev.Name)
}

func TestCreateEventFromDB_Download_RemoteStateGone(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// No remote_state seeded.
	row := &synctypes.SyncFailureRow{
		Path:      "no-remote.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
	}

	ev := createEventFromDBForTest(t, eng, ctx, row)

	assert.Nil(t, ev, "should return nil when no remote_state")
}

// ---------------------------------------------------------------------------
// isFailureResolved
// ---------------------------------------------------------------------------

func TestIsFailureResolved_Download_Synced(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state and set it to synced (simulates a download that
	// completed through the normal pipeline).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "resolved-item",
			Path:     "resolved.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "resolved-hash",
			Size:     512,
		},
	}, "", driveID))

	_, err := eng.baseline.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE item_id = ?`,
		synctypes.SyncStatusSynced, "resolved-item",
	)
	require.NoError(t, err)

	// Seed a sync_failure for this path.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "resolved.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
	}, nil))

	row := &synctypes.SyncFailureRow{
		Path:       "resolved.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDownload,
		ActionType: synctypes.ActionDownload,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download with synced remote_state should be resolved")

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

	row := &synctypes.SyncFailureRow{
		Path:       "deleted-remotely.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDownload,
		ActionType: synctypes.ActionDownload,
	}

	assert.True(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download with no remote_state should be resolved")
}

func TestIsFailureResolved_Download_StillPending(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	// Seed remote_state with pending_download.
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "still-pending.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "pending-hash",
		},
	}, "", driveID))

	row := &synctypes.SyncFailureRow{
		Path:       "still-pending.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDownload,
		ActionType: synctypes.ActionDownload,
	}

	assert.False(t, isFailureResolvedForTest(t, eng, ctx, row),
		"download with pending_download remote_state should NOT be resolved")
}

func TestIsFailureResolved_Upload_FileGone(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:       "gone-upload.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
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

	row := &synctypes.SyncFailureRow{
		Path:       "still-here.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
	}

	assert.False(t, isFailureResolvedForTest(t, eng, ctx, row),
		"upload for existing file should NOT be resolved")
}

func TestIsFailureResolved_Delete_NoBaseline(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()

	driveID := driveid.New("drive1")

	row := &synctypes.SyncFailureRow{
		Path:       "already-deleted.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDelete,
		ActionType: synctypes.ActionRemoteDelete,
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
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "baseline-item",
			Path:     "still-in-baseline.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "bl-hash",
			Size:     100,
		},
	}, "", driveID))

	require.NoError(t, eng.baseline.CommitOutcome(ctx, &synctypes.Outcome{
		Action:     synctypes.ActionDownload,
		Success:    true,
		Path:       "still-in-baseline.txt",
		DriveID:    driveID,
		ItemID:     "baseline-item",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "bl-hash",
		RemoteHash: "bl-hash",
		Size:       100,
	}))

	row := &synctypes.SyncFailureRow{
		Path:       "still-in-baseline.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionDelete,
		ActionType: synctypes.ActionRemoteDelete,
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
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "d9-item",
			Path:     "d9-test.txt",
			ParentID: "d9-parent",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "d9-hash",
			Size:     9999,
			Mtime:    7777777777,
			ETag:     "d9-etag",
		},
	}, "", driveID))

	// Seed a sync_failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "d9-test.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Category:  synctypes.CategoryTransient,
	}, func(_ int) time.Duration {
		return -time.Minute
	}))

	outbox := runTestRetrierSweep(t, eng, ctx)

	require.Len(t, outbox, 1)
	assert.Equal(t, synctypes.ActionDownload, outbox[0].Action.Type)
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

	// Seed remote_state: d11-synced will be set to synctypes.SyncStatusSynced (resolved),
	// d11-pending stays at synctypes.SyncStatusPendingDownload (not resolved).
	require.NoError(t, eng.baseline.CommitObservation(ctx, []synctypes.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "synced-item",
			Path:     "d11-synced.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "synced-hash",
			Size:     100,
		},
		{
			DriveID:  driveID,
			ItemID:   "pending-item",
			Path:     "d11-pending.txt",
			ItemType: synctypes.ItemTypeFile,
			Hash:     "pending-hash",
			Size:     200,
		},
	}, "", driveID))

	// Directly set d11-synced to synced status (simulates a completed download
	// through the normal pipeline). The full download lifecycle
	// (pending_download → downloading → synced) isn't needed for this test.
	_, err := eng.baseline.DB().ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE item_id = ?`,
		synctypes.SyncStatusSynced, "synced-item",
	)
	require.NoError(t, err)

	// Seed sync_failures for both.
	for _, path := range []string{"d11-synced.txt", "d11-pending.txt"} {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      path,
			DriveID:   driveID,
			Direction: synctypes.DirectionDownload,
			Category:  synctypes.CategoryTransient,
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

	eng := newSingleOwnerEngine(t)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
	var events []engineDebugEvent
	eng.debugEventHook = func(event engineDebugEvent) {
		events = append(events, event)
	}

	ctx := context.Background()
	now := eng.nowFn()

	sk := synctypes.SKQuotaOwn()

	// Set up a scope block with NextTrialAt in the past.
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	absPath := filepath.Join(eng.syncRoot, "trial.txt")
	require.NoError(t, os.WriteFile(absPath, []byte("trial payload"), 0o600))

	// Seed a scope-blocked failure.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	// Capture the scope block's TrialInterval before dispatch.
	blockBefore, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	intervalBefore := blockBefore.TrialInterval

	outbox := runTestTrialDispatch(t, eng, ctx)
	require.Len(t, outbox, 1)
	assert.Equal(t, "trial.txt", outbox[0].Action.Path)
	assert.Equal(t, synctypes.ActionUpload, outbox[0].Action.Type)
	assert.True(t, outbox[0].IsTrial)
	assert.Equal(t, sk, outbox[0].TrialScopeKey)
	require.Contains(t, events, engineDebugEvent{
		Type:     engineDebugEventTrialDispatched,
		ScopeKey: sk,
		Path:     "trial.txt",
	})

	// After successful dispatch, the scope block's TrialInterval should NOT
	// be extended — interval stays unmutated until the worker result arrives.
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

	sk := synctypes.SKQuotaOwn()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "missing.txt",
		DriveID:   driveID,
		Direction: synctypes.DirectionDownload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
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
	require.NoError(t, file.Truncate(syncobserve.MaxOneDriveFileSize+1))
	require.NoError(t, file.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryTransient,
	}, func(int) time.Duration {
		return -time.Minute
	}))

	outbox := runTestRetrierSweep(t, eng, ctx)
	assert.Empty(t, outbox)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, synctypes.CategoryActionable, failures[0].Category)
	assert.Equal(t, synctypes.FailureRoleItem, failures[0].Role)
	assert.Equal(t, synctypes.IssueFileTooLarge, failures[0].IssueType)
}

func TestTrialDispatch_SkippedHeldCandidateBecomesActionableAndContinues(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	sk := synctypes.SKQuotaOwn()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	oversized, err := os.Create(filepath.Join(eng.syncRoot, "oversized.bin"))
	require.NoError(t, err)
	require.NoError(t, oversized.Truncate(syncobserve.MaxOneDriveFileSize+1))
	require.NoError(t, oversized.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	absPath := filepath.Join(eng.syncRoot, "trial.txt")
	require.NoError(t, os.WriteFile(absPath, []byte("trial payload"), 0o600))

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "trial.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	outbox := runTestTrialDispatch(t, eng, ctx)
	require.Len(t, outbox, 1)
	assert.Equal(t, "trial.txt", outbox[0].Action.Path)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 2)

	var actionableBad, heldTrial bool
	for i := range failures {
		switch failures[i].Path {
		case "oversized.bin":
			actionableBad = true
			assert.Equal(t, synctypes.CategoryActionable, failures[i].Category)
			assert.Equal(t, synctypes.IssueFileTooLarge, failures[i].IssueType)
		case "trial.txt":
			heldTrial = true
			assert.Equal(t, synctypes.FailureRoleHeld, failures[i].Role)
		}
	}

	assert.True(t, actionableBad)
	assert.True(t, heldTrial)
	assert.True(t, isTestScopeBlocked(eng, sk))
}

// Validates: R-2.10.5
func TestTrialDispatch_OnlySkippedHeldCandidatesPreserveScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	sk := synctypes.SKQuotaOwn()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           sk,
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     eng.nowFn().Add(-time.Minute),
		NextTrialAt:   eng.nowFn().Add(-time.Second),
		TrialInterval: 10 * time.Second,
	})

	oversized, err := os.Create(filepath.Join(eng.syncRoot, "oversized.bin"))
	require.NoError(t, err)
	require.NoError(t, oversized.Truncate(syncobserve.MaxOneDriveFileSize+1))
	require.NoError(t, oversized.Close())

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      "oversized.bin",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Role:      synctypes.FailureRoleHeld,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  sk,
	}, nil))

	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, synctypes.CategoryActionable, failures[0].Category)
	assert.Equal(t, synctypes.IssueFileTooLarge, failures[0].IssueType)
	block, ok := getTestScopeBlock(eng, sk)
	require.True(t, ok)
	assert.Equal(t, 10*time.Second, block.TrialInterval)
	assert.Equal(t, eng.nowFn().Add(10*time.Second), block.NextTrialAt)
}

func TestEngine_ClearFailureCandidate_RemovesSyncFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &synctypes.SyncFailureRow{
		Path:    "clear-me.txt",
		DriveID: eng.driveID,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryTransient,
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
	row := &synctypes.SyncFailureRow{
		Path:    "internal.tmp",
		DriveID: eng.driveID,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryTransient,
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &synctypes.SkippedItem{Path: row.Path})

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

// Validates: R-2.10.2
func TestEngine_RecordRetryTrialSkippedItem_ReasonlessSkipWithZeroDriveIDClearsEngineDriveFailure(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &synctypes.SyncFailureRow{
		Path: "internal.tmp",
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      row.Path,
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
		Category:  synctypes.CategoryActionable,
		IssueType: synctypes.IssueInvalidFilename,
		ErrMsg:    "leading space",
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &synctypes.SkippedItem{Path: row.Path})

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestEngine_RecordRetryTrialSkippedItem_ZeroDriveIDFallsBackToEngineDrive(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &synctypes.SyncFailureRow{
		Path:      "oversized.bin",
		Direction: synctypes.DirectionUpload,
	}

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &synctypes.SkippedItem{
		Path:     row.Path,
		Reason:   synctypes.IssueFileTooLarge,
		Detail:   "file size exceeds limit",
		FileSize: syncobserve.MaxOneDriveFileSize + 1,
	})

	failures, err := eng.baseline.ListActionableFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, row.Path, failures[0].Path)
	assert.Equal(t, eng.driveID, failures[0].DriveID)
	assert.Equal(t, synctypes.CategoryActionable, failures[0].Category)
	assert.Equal(t, synctypes.IssueFileTooLarge, failures[0].IssueType)
	assert.Equal(t, synctypes.FailureRoleItem, failures[0].Role)
}

// Validates: R-2.11.5
func TestEngine_RecordRetryTrialSkippedItem_ReplacesExistingActionableIssueType(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	row := &synctypes.SyncFailureRow{
		Path:      "problem.txt",
		DriveID:   eng.driveID,
		Direction: synctypes.DirectionUpload,
	}

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      row.Path,
		DriveID:   row.DriveID,
		Direction: row.Direction,
		Category:  synctypes.CategoryActionable,
		IssueType: synctypes.IssueInvalidFilename,
		ErrMsg:    "contains ':'",
	}, nil))

	recordRetryTrialSkippedItemForTest(t, eng, ctx, row, &synctypes.SkippedItem{
		Path:   row.Path,
		Reason: synctypes.IssuePathTooLong,
		Detail: "path exceeds 400-character limit",
	})

	failures, err := eng.baseline.ListActionableFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 1)
	assert.Equal(t, row.Path, failures[0].Path)
	assert.Equal(t, row.DriveID, failures[0].DriveID)
	assert.Equal(t, synctypes.IssuePathTooLong, failures[0].IssueType)
	assert.Equal(t, "path exceeds 400-character limit", failures[0].LastError)
	assert.Equal(t, synctypes.FailureRoleItem, failures[0].Role)
}

func TestTrialDispatch_DoesNotMutateStateWhenNoScopeIsDue(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := context.Background()
	now := eng.nowFn()

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKQuotaOwn(),
		IssueType:     synctypes.IssueQuotaExceeded,
		BlockedAt:     now.Add(-time.Minute),
		NextTrialAt:   now.Add(time.Minute),
		TrialInterval: 10 * time.Second,
	})

	outbox := runTestTrialDispatch(t, eng, ctx)
	assert.Empty(t, outbox)

	blocks, err := eng.baseline.ListScopeBlocks(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, synctypes.SKQuotaOwn(), blocks[0].Key)
}
