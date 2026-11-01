package sync

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-6.8.24
func TestNextReleaseAllowance_RampsFromTheWorkerCountAndDoubles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		transferWorkers int
		lastAllowance   int
		threshold       int
		want            int
	}{
		{name: "first batch is at least four with a small pool", transferWorkers: 1, lastAllowance: 0, want: 4},
		{name: "first batch is at least four with no pool size", transferWorkers: 0, lastAllowance: 0, want: 4},
		{name: "first batch is the worker count when larger", transferWorkers: 8, lastAllowance: 0, want: 8},
		{name: "first batch ignores the threshold", transferWorkers: 4, lastAllowance: 0, threshold: 16, want: 4},
		{name: "second batch doubles the first", transferWorkers: 4, lastAllowance: 4, want: 8},
		{name: "third batch doubles again", transferWorkers: 4, lastAllowance: 8, want: 16},
		{name: "doubling continues past the worker count", transferWorkers: 8, lastAllowance: 64, want: 128},
		{name: "doubling stops at the threshold", transferWorkers: 4, lastAllowance: 8, threshold: 16, want: 16},
		{name: "doubling lands on the threshold when it would pass it", transferWorkers: 4, lastAllowance: 8, threshold: 12, want: 12},
		{name: "above the threshold the batch grows by one", transferWorkers: 4, lastAllowance: 16, threshold: 16, want: 17},
		{name: "additive growth continues", transferWorkers: 4, lastAllowance: 17, threshold: 16, want: 18},
		{name: "the cap holds", transferWorkers: 4, lastAllowance: releaseRampMaxBatch, want: releaseRampMaxBatch},
		{name: "the last doubling lands on the cap", transferWorkers: 4, lastAllowance: releaseRampMaxBatch / 2, want: releaseRampMaxBatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, nextReleaseAllowance(tc.transferWorkers, tc.lastAllowance, tc.threshold))
		})
	}
}

// Validates: R-6.8.24
func TestSlowStartThresholdAfterThrottle_RemembersHalfTheBatchThatDrewThe429(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		transferWorkers     int
		allowanceAtThrottle int
		previous            int
		want                int
	}{
		{name: "half the batch", transferWorkers: 4, allowanceAtThrottle: 32, want: 16},
		{name: "never below the first batch", transferWorkers: 4, allowanceAtThrottle: 6, want: 4},
		{name: "the first batch follows the worker count", transferWorkers: 8, allowanceAtThrottle: 6, want: 8},
		{name: "a 429 outside a ramp keeps the previous threshold", transferWorkers: 4, allowanceAtThrottle: 0, previous: 16, want: 16},
		{name: "a 429 outside a ramp with no memory leaves none", transferWorkers: 4, allowanceAtThrottle: 0, want: 0},
		{name: "a later 429 replaces the memory", transferWorkers: 4, allowanceAtThrottle: 18, previous: 16, want: 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, slowStartThresholdAfterThrottle(tc.transferWorkers, tc.allowanceAtThrottle, tc.previous))
		})
	}
}

// rampTestEngine is a watch-shaped engine whose logs are captured, with a
// manual clock and a throttle scope that is due for a trial.
func rampTestEngine(t *testing.T, workers int) (*testEngine, *watchRuntime, *syncLockedBuffer, *manualClock) {
	t.Helper()

	logger, logs := newCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &engineMockClient{}, logger)
	eng.transferWorkers = workers
	clock := newManualClock(time.Date(2026, 11, 1, 9, 7, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	return eng, rt, logs, clock
}

// holdBlockedUploadsForTest registers count uploads of the engine's drive,
// persisted blocked under key and held for its trial, in held order.
func holdBlockedUploadsForTest(t *testing.T, eng *testEngine, rt *watchRuntime, key ScopeKey, firstID int64, count int) []*trackedAction {
	t.Helper()

	held := make([]*trackedAction, 0, count)
	for i := range count {
		id := firstID + int64(i)
		ta := rt.sched.graph.Add(&Action{
			Type:    ActionUpload,
			Path:    fmt.Sprintf("blocked-%02d.txt", id),
			DriveID: eng.driveID,
			View:    &pathView{Path: fmt.Sprintf("blocked-%02d.txt", id)},
		}, id, nil)
		require.NotNil(t, ta)
		require.NoError(t, rt.persistBlockedRetryWork(t.Context(), retryWorkKeyForAction(&ta.Action), key))
		rt.holdAction(ta, heldReasonScope, key, time.Time{})
		held = append(held, ta)
	}

	return held
}

func blockedRetryRowsForTest(t *testing.T, eng *testEngine, key ScopeKey) int {
	t.Helper()

	blocked := 0
	for _, row := range listRetryWorkForTest(t, eng.baseline, t.Context()) {
		if row.Blocked && row.ScopeKey == key {
			blocked++
		}
	}

	return blocked
}

func trialSuccessForTest(ta *trackedAction) *actionCompletion {
	return &actionCompletion{
		ActionID:             ta.ID,
		Path:                 ta.Action.Path,
		ActionType:           ta.Action.Type,
		DriveID:              ta.Action.DriveID,
		Success:              true,
		IsTrial:              true,
		TrialScopeKey:        ta.TrialScopeKey,
		TrialScopeGeneration: ta.TrialScopeGeneration,
	}
}

// Validates: R-6.8.24, R-6.6.30
//
// A successful trial releases one bounded batch in held order and keeps the
// scope on probation; the scope is removed, and "Resumed syncing" said, by
// the batch that releases the last blocked row, and a member that finishes
// after that is an ordinary success.
func TestEngineFlow_TrialSuccess_ReleasesOneBatchAndKeepsTheScopeOnProbation(t *testing.T) {
	t.Parallel()

	eng, rt, logs, _ := rampTestEngine(t, 2)
	ctx := t.Context()
	key := SKThrottleDrive(eng.driveID)
	setTestBlockScope(t, eng, &BlockScope{Key: key, TrialInterval: time.Minute, NextTrialAt: eng.nowFunc().Add(-time.Second)})
	held := holdBlockedUploadsForTest(t, eng, rt, key, 1, 10)

	trial := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: eng.driveID}, 99, nil)
	require.NotNil(t, trial)
	trial.IsTrial, trial.TrialScopeKey = true, key
	rt.sched.markRunning(trial)

	batch, err := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(trial), &Baseline{})
	require.NoError(t, err)
	require.Len(t, batch, 4, "two workers ramp from the minimum batch of four")
	for i, ta := range batch {
		assert.Same(t, held[i], ta, "the batch is released in held order")
		assert.True(t, ta.IsTrial, "a batch member is a trial of the scope")
		assert.Equal(t, key, ta.TrialScopeKey)
	}
	scope, active := rt.scopes.lookupActiveScope(key)
	require.True(t, active, "the scope stays on probation")
	assert.Equal(t, 4, scope.ReleaseAllowance)
	assert.True(t, isTestBlockScopeed(eng, key), "the scope row stays while blocked rows remain")
	assert.Equal(t, 6, blockedRetryRowsForTest(t, eng, key))
	assert.Len(t, rt.retries.heldByScope[key], 6)
	assert.Zero(t, strings.Count(logs.String(), "Resumed syncing"), "a batch is not a release")

	// The batch's first three successes only count it down: releasing on
	// every success would cascade the whole backlog off a few successes.
	for _, member := range batch[:3] {
		ready, memberErr := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(member), &Baseline{})
		require.NoError(t, memberErr)
		assert.Empty(t, ready, "%s finishing does not release the next batch yet", member.Action.Path)
	}
	scope, _ = rt.scopes.lookupActiveScope(key)
	assert.Equal(t, 1, scope.BatchOutstanding)
	assert.Equal(t, 6, blockedRetryRowsForTest(t, eng, key))

	// The batch's last success doubles the allowance; eight covers the six
	// left, and the last blocked row released removes the scope.
	rest, err := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(batch[3]), &Baseline{})
	require.NoError(t, err)
	require.Len(t, rest, 6)
	assert.False(t, rt.scopes.hasActiveScope(key), "the last blocked row released removes the scope")
	assert.False(t, isTestBlockScopeed(eng, key))
	assert.Zero(t, blockedRetryRowsForTest(t, eng, key))
	assert.Equal(t, 1, strings.Count(logs.String(), "Resumed syncing"))

	// A member that finishes after the scope is gone is an ordinary success.
	late, err := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(rest[0]), &Baseline{})
	require.NoError(t, err)
	assert.Empty(t, late)
	assert.Equal(t, 1, strings.Count(logs.String(), "Resumed syncing"))
	assert.Equal(t, 6, rt.results.succeeded)
}

// Validates: R-6.8.24, R-6.6.30
//
// A 429 by a probation member re-blocks the scope with the new Retry-After,
// takes the rest of the batch back out of the outbox, resets the ramp, and
// stays quiet (the scope was already announced). A success from before the
// re-block releases nothing; the next due trial ramps from the first batch.
func TestEngineFlow_ThrottledMemberDuringProbation_ReblocksRetiresTheOutboxAndResetsTheRamp(t *testing.T) {
	t.Parallel()

	eng, rt, logs, clock := rampTestEngine(t, 4)
	ctx := t.Context()
	key := SKThrottleDrive(eng.driveID)
	setTestBlockScope(t, eng, &BlockScope{Key: key, TrialInterval: time.Minute, NextTrialAt: eng.nowFunc().Add(-time.Second)})
	holdBlockedUploadsForTest(t, eng, rt, key, 1, 10)

	trial := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "trial.txt", DriveID: eng.driveID}, 99, nil)
	require.NotNil(t, trial)
	trial.IsTrial, trial.TrialScopeKey = true, key
	rt.sched.markRunning(trial)
	batch, err := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(trial), &Baseline{})
	require.NoError(t, err)
	require.Len(t, batch, 4)
	generation := batch[0].TrialScopeGeneration

	// The loop appends the batch to its outbox and a worker takes the head.
	require.NoError(t, rt.appendReadyFrontier(batch))
	rt.handleWatchDispatch(0)
	require.Len(t, rt.currentOutbox(), 3)

	throttled := trialSuccessForTest(batch[0])
	throttled.Success = false
	throttled.HTTPStatus = http.StatusTooManyRequests
	throttled.RetryAfter = 30 * time.Second
	throttled.Err = graph.ErrThrottled
	throttled.ErrMsg = "rate limited"
	ready, err := rt.applyRuntimeCompletionStage(ctx, throttled, &Baseline{})
	require.NoError(t, err)
	assert.Empty(t, ready)

	scope, active := rt.scopes.lookupActiveScope(key)
	require.True(t, active)
	assert.Zero(t, scope.ReleaseAllowance, "a 429 inside the ramp resets it")
	assert.Equal(t, 4, scope.SlowStartThreshold, "the ramp remembers half the batch that drew the 429, at least the first batch")
	assert.Equal(t, generation+1, scope.Generation)
	assert.Equal(t, 30*time.Second, scope.TrialInterval, "the new Retry-After is the trial interval")
	assert.Equal(t, clock.Now().Add(30*time.Second), scope.NextTrialAt)
	assert.Empty(t, rt.currentOutbox(), "admitted members the scope blocks are taken back")
	for _, ta := range batch[1:] {
		heldEntry := rt.retries.heldByKey[retryWorkKeyForAction(&ta.Action)]
		require.NotNil(t, heldEntry, "%s is held again", ta.Action.Path)
		assert.Equal(t, heldReasonScope, heldEntry.Reason)
		assert.Equal(t, key, heldEntry.ScopeKey)
	}
	assert.Equal(t, 10, blockedRetryRowsForTest(t, eng, key), "every member is blocked again")
	assert.Empty(t, rt.sched.queuedByID)
	output := logs.String()
	assert.Zero(t, strings.Count(output, "OneDrive is limiting requests"), "a re-block of an announced scope is not announced again")
	assert.Zero(t, strings.Count(output, "Resumed syncing"))

	requireStaleSuccessReleasesNothing(t, eng, rt, key, generation)
	requireNextTrialRampsFromTheFirstBatch(t, eng, rt, clock, key, generation+1)
}

// requireStaleSuccessReleasesNothing completes a member released under
// generation after the scope moved on: the success is ordinary and opens
// nothing.
func requireStaleSuccessReleasesNothing(t *testing.T, eng *testEngine, rt *watchRuntime, key ScopeKey, generation uint64) {
	t.Helper()

	stale := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "stale.txt", DriveID: eng.driveID}, 98, nil)
	require.NotNil(t, stale)
	stale.IsTrial, stale.TrialScopeKey, stale.TrialScopeGeneration = true, key, generation
	rt.sched.markRunning(stale)
	ready, err := rt.applyRuntimeCompletionStage(t.Context(), trialSuccessForTest(stale), &Baseline{})
	require.NoError(t, err)
	assert.Empty(t, ready)
	scope, _ := rt.scopes.lookupActiveScope(key)
	assert.Zero(t, scope.ReleaseAllowance)
	assert.Equal(t, 10, blockedRetryRowsForTest(t, eng, key))
}

// requireNextTrialRampsFromTheFirstBatch advances to the scope's trial and
// checks that its success starts the ramp over at the first batch, under the
// expected generation.
func requireNextTrialRampsFromTheFirstBatch(
	t *testing.T, eng *testEngine, rt *watchRuntime, clock *manualClock, key ScopeKey, generation uint64,
) {
	t.Helper()

	scope, _ := rt.scopes.lookupActiveScope(key)
	clock.Advance(scope.NextTrialAt.Sub(clock.Now()))
	trials, err := rt.drainDueHeldWorkNow(t.Context())
	require.NoError(t, err)
	require.Len(t, trials, 1)
	assert.Equal(t, generation, trials[0].TrialScopeGeneration)
	rt.sched.markRunning(trials[0])
	batch, err := rt.applyRuntimeCompletionStage(t.Context(), trialSuccessForTest(trials[0]), &Baseline{})
	require.NoError(t, err)
	assert.Len(t, batch, nextReleaseAllowance(eng.transferWorkers, 0, 0))
	scope, _ = rt.scopes.lookupActiveScope(key)
	assert.Equal(t, nextReleaseAllowance(eng.transferWorkers, 0, 0), scope.ReleaseAllowance)
}

// Validates: R-6.8.24
//
// When a scope activates, the watch loop's outbox gives up the admitted work
// the scope blocks: it is held under the scope and persisted blocked, and
// work on another drive stays queued.
func TestWatchRuntime_ScopeActivationRetiresAdmittedWorkItBlocks(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.transferWorkers = 2
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	key := SKThrottleDrive(eng.driveID)
	otherDrive := driveid.New("0000000000000002")

	first := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "first.txt", DriveID: eng.driveID}, 1, nil)
	other := rt.sched.graph.Add(&Action{Type: ActionDownload, Path: "other.txt", DriveID: otherDrive, ItemID: "other-item"}, 2, nil)
	second := rt.sched.graph.Add(&Action{Type: ActionUpload, Path: "second.txt", DriveID: eng.driveID}, 3, nil)
	for _, ta := range []*trackedAction{first, other, second} {
		require.NotNil(t, ta)
		rt.sched.markQueued(ta)
	}
	rt.replaceOutbox([]*trackedAction{first, other, second})

	// The failure that activates the scope is persisted blocked first.
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, testRetryWorkKey("failed.txt", "", ActionUpload), key)
	require.NoError(t, err)
	require.NoError(t, rt.applyBlockScope(ctx, scopeUpdateResult{
		Block: true, ScopeKey: key, ConditionType: issueRateLimited, RetryAfter: 10 * time.Second,
	}))

	require.Equal(t, []*trackedAction{other}, rt.currentOutbox(), "only work the scope does not block stays admitted")
	for _, ta := range []*trackedAction{first, second} {
		heldEntry := rt.retries.heldByKey[retryWorkKeyForAction(&ta.Action)]
		require.NotNil(t, heldEntry, "%s is held under the scope", ta.Action.Path)
		assert.Equal(t, heldReasonScope, heldEntry.Reason)
		assert.Equal(t, key, heldEntry.ScopeKey)
		_, queued := rt.sched.queuedByID[ta.ID]
		assert.False(t, queued)
	}
	assert.Equal(t, 3, blockedRetryRowsForTest(t, eng, key), "the retired work is persisted blocked under the scope")
	assert.Equal(t, 3, rt.sched.graph.InFlightCount(), "retiring must not complete dependency nodes")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventAdmittedWorkRetired && event.ScopeKey == key && event.Count == 2
	}, 1, "the retirement is instrumented once with its count")
}

// Validates: R-6.8.24
//
// The one-shot results loop owns its outbox the same way: a 429 completion
// activates the scope and the pending uploads of that drive are held instead
// of handed to the workers.
func TestOneShotRunner_ScopeActivationRetiresPendingOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	runner := newOneShotRunner(eng.Engine)
	runner.sched.graph = newDepGraph(eng.logger)
	runner.sched.dispatchCh = make(chan *trackedAction)
	runner.scopes.state = newScopeState(eng.nowFunc, eng.logger)
	ctx := t.Context()
	key := SKThrottleDrive(eng.driveID)
	otherDrive := driveid.New("0000000000000002")

	failing := runner.sched.graph.Add(&Action{Type: ActionUpload, Path: "failing.txt", DriveID: eng.driveID}, 1, nil)
	pending := runner.sched.graph.Add(&Action{Type: ActionUpload, Path: "pending.txt", DriveID: eng.driveID}, 2, nil)
	other := runner.sched.graph.Add(&Action{Type: ActionDownload, Path: "other.txt", DriveID: otherDrive, ItemID: "other-item"}, 3, nil)
	require.NotNil(t, failing)
	require.NotNil(t, pending)
	require.NotNil(t, other)
	runner.sched.markRunning(failing)
	runner.sched.markQueued(pending)
	runner.sched.markQueued(other)
	runner.outbox = []*trackedAction{pending, other}

	err := runner.handleOneShotCompletion(ctx, nil, nil, nil, &actionCompletion{
		ActionID:   1,
		Path:       "failing.txt",
		ActionType: ActionUpload,
		DriveID:    eng.driveID,
		HTTPStatus: http.StatusTooManyRequests,
		RetryAfter: 10 * time.Second,
		Err:        graph.ErrThrottled,
		ErrMsg:     "rate limited",
	})
	require.NoError(t, err)

	assert.Equal(t, []*trackedAction{other}, runner.outbox)
	heldEntry := runner.retries.heldByKey[retryWorkKeyForAction(&pending.Action)]
	require.NotNil(t, heldEntry)
	assert.Equal(t, heldReasonScope, heldEntry.Reason)
	assert.Equal(t, key, heldEntry.ScopeKey)
	assert.Equal(t, 2, blockedRetryRowsForTest(t, eng, key))
	assert.True(t, runner.scopes.hasActiveScope(key))
	assert.Zero(t, runner.sched.runningCount)
}

// Validates: R-2.8.5, R-6.8.24
//
// A batch release reports through the signal seam rather than reaching for a
// concrete watch runtime, so one-shot and watch travel the same code path; a
// scope with nothing blocked under it is removed by its first release.
func TestEngineFlow_ReleaseScopeBatch_ReportsThroughRuntimeSignals(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)

	rec := &recordingSignals{}
	flow.signals = rec

	key := SKDiskLocal()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           key,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	})
	released, err := flow.releaseScopeBatch(context.WithoutCancel(t.Context()), key, 0)
	require.NoError(t, err)

	assert.Empty(t, released)
	assert.Equal(t, 1, rec.trial, "release must ask the runtime to rearm the trial timer")
	assert.False(t, flow.scopes.hasActiveScope(key))
	assert.False(t, isTestBlockScopeed(eng, key))
}

// Validates: R-6.8.24
//
// The transfers in flight when a throttle hits each bring their own
// Retry-After, later ones longer (every request inside the penalty extends
// it), and their completions reach the loop in scheduler order. A shorter
// Retry-After applied after a longer one must not pull the trial inside the
// penalty the drive still holds.
func TestEngineFlow_LaterShorterRetryAfterNeverPullsTheTrialEarlier(t *testing.T) {
	t.Parallel()

	eng, rt, _, clock := rampTestEngine(t, 4)
	ctx := t.Context()
	key := SKThrottleDrive(eng.driveID)
	holdBlockedUploadsForTest(t, eng, rt, key, 1, 3)
	now := clock.Now()

	require.NoError(t, rt.applyBlockScope(ctx, scopeUpdateResult{
		Block: true, ScopeKey: key, ConditionType: issueRateLimited, RetryAfter: 40 * time.Second,
	}))
	require.NoError(t, rt.applyBlockScope(ctx, scopeUpdateResult{
		Block: true, ScopeKey: key, ConditionType: issueRateLimited, RetryAfter: 10 * time.Second,
	}))
	scope, active := rt.scopes.lookupActiveScope(key)
	require.True(t, active)
	assert.Equal(t, now.Add(40*time.Second), scope.NextTrialAt, "a re-activation keeps the later deadline")
	assert.Equal(t, uint64(2), scope.Generation, "and is still a new activation")

	require.NoError(t, rt.extendScopeTrial(ctx, key, 20*time.Second))
	scope, _ = rt.scopes.lookupActiveScope(key)
	assert.Equal(t, now.Add(40*time.Second), scope.NextTrialAt, "an extension keeps the later deadline too")

	require.NoError(t, rt.extendScopeTrial(ctx, key, 60*time.Second))
	scope, _ = rt.scopes.lookupActiveScope(key)
	assert.Equal(t, now.Add(60*time.Second), scope.NextTrialAt, "a later deadline moves the trial out")

	persisted, ok := getTestBlockScope(eng, key)
	require.True(t, ok)
	assert.Equal(t, scope.NextTrialAt, persisted.NextTrialAt, "the store holds the same deadline")

	// A scope on probation holds the stall-recovery re-arm, not a server
	// deadline: the 429 that ends the probation sets the deadline outright.
	scope.ReleaseAllowance = 4
	scope.NextTrialAt = now.Add(5 * time.Minute)
	rt.scopes.upsertActiveScope(&scope)
	require.NoError(t, rt.extendScopeTrial(ctx, key, 30*time.Second))
	scope, _ = rt.scopes.lookupActiveScope(key)
	assert.Equal(t, now.Add(30*time.Second), scope.NextTrialAt, "a 429 during probation is the deadline")
	assert.Zero(t, scope.ReleaseAllowance)
}
