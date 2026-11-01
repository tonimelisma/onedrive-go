package sync

import (
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// dispatchTrace records when (engine clock) each action was handed to a
// worker, per pass.
type dispatchTrace struct {
	mu     stdsync.Mutex // guards the fields below
	pass   int
	passes [][]time.Time
}

//nolint:gocritic // Value semantics match SetDebugEventHook, so the hook cannot mutate engine-owned events.
func (tr *dispatchTrace) record(event DebugEvent) {
	if event.Type != engineDebugEventActionDispatched {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.passes[tr.pass-1] = append(tr.passes[tr.pass-1], event.At)
}

func (tr *dispatchTrace) beginPass() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.pass++
	tr.passes = append(tr.passes, nil)
}

func (tr *dispatchTrace) dispatchesInPass(pass int) []time.Time {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return append([]time.Time(nil), tr.passes[pass-1]...)
}

// distinctJitter answers Jitter with a different instant on every call:
// multiples of 61 ms, so up to sixteen members of one batch are spread
// over distinct instants of the window.
func distinctJitter() func(time.Duration) time.Duration {
	calls := 0
	return func(maxDelay time.Duration) time.Duration {
		calls++
		return (time.Duration(calls) * 61 * time.Millisecond) % maxDelay
	}
}

// Validates: R-6.8.24
//
// A released ramp batch reached the drive in one instant. Its members must
// be admitted at distinct instants spread over releaseBatchAdmissionWindow
// of the release, on the engine clock; the trial that honors the
// server-given Retry-After is not jittered; and a one-shot pass, which has
// no runtime signals, waits for its parked members in-loop and still
// completes the backlog.
func TestReleaseJitter_OneShotBatchIsAdmittedOverAWindow(t *testing.T) {
	t.Parallel()

	const (
		files        = 20
		workers      = 4
		windowBudget = 3
		window       = time.Second
	)

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	clock := newManualClock(time.Date(2026, 11, 1, 9, 7, 0, 0, time.UTC))
	clock.SetJitterFunc(distinctJitter())
	limiter := newThrottleWindowClient(clock.Now, windowBudget, window)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = workers
	installManualClockForTestEngine(eng, clock)
	scopeKey := SKThrottleDrive(driveID)
	trace := &dispatchTrace{}
	eng.SetDebugEventHook(trace.record)
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	for i := range files {
		writeLocalFile(t, syncRoot, "file-"+string(rune('a'+i))+".txt", "content\n")
	}
	h.beginRound(t)

	// Pass 1 draws the 429 that blocks the drive.
	trace.beginPass()
	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	block, ok := getTestBlockScope(eng, scopeKey)
	require.True(t, ok, "the 429 activated the throttle scope")
	_, throttled, _ := limiter.counts()
	require.Positive(t, throttled)

	// Pass 2: the trial at the deadline succeeds and the ramp's batches are
	// admitted over their windows. The pass waits on its own timers in
	// virtual time, so the clock follows them.
	limiter.setBudget(1000)
	clock.Advance(block.NextTrialAt.Sub(clock.Now()))
	trialAt := clock.Now()
	clock.autoAdvanceTimers(t)
	trace.beginPass()
	_, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, files, remote.uploads, "the pass completes the backlog")
	_, ok = getTestBlockScope(eng, scopeKey)
	assert.False(t, ok, "the scope is released with its last batch")

	dispatches := trace.dispatchesInPass(2)
	require.GreaterOrEqual(t, len(dispatches), 1+firstReleaseAllowance(workers))
	assert.Equal(t, trialAt, dispatches[0], "the trial goes at the Retry-After deadline, unjittered")
	firstBatch := dispatches[1 : 1+firstReleaseAllowance(workers)]
	for i, at := range firstBatch {
		assert.True(t, at.After(trialAt), "member %d is admitted after the release", i)
		assert.LessOrEqual(t, at.Sub(trialAt), releaseBatchAdmissionWindow, "member %d is admitted within the window", i)
	}
	for i := 1; i < len(dispatches); i++ {
		assert.True(t, dispatches[i].After(dispatches[i-1]),
			"dispatch %d at %s follows dispatch %d at %s: batch members go at distinct instants",
			i, dispatches[i].Format(time.RFC3339Nano), i-1, dispatches[i-1].Format(time.RFC3339Nano))
	}

	clock.Advance(window)
	assertSyncInvariants(t, h)
}

// Validates: R-6.8.24, R-6.8.9
//
// The same pass with the frontier reversed on one serialized worker (D-090):
// the in-loop wait for a parked member must not depend on worker count or
// frontier order. A variant of the targeted test above, so it runs in the
// full profiles only.
func TestReleaseJitter_OneShotBatchUnderReverseDispatch(t *testing.T) {
	skipInShort(t)
	runUnderDispatchOrder(t, "reverse", TestReleaseJitter_OneShotBatchIsAdmittedOverAWindow)
}

// Validates: R-6.8.24
func TestWatchRuntime_ParkedBatchMembersWaitForTheirAdmitInstant(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	clock, ok := eng.clock.(*manualClock)
	require.True(t, ok)
	rt := testWatchRuntime(t, eng)
	// No plan is installed, so the slow start (R-6.8.25) does not gate; the
	// test is about admission instants alone.
	now := clock.Now()

	actions := make([]*trackedAction, 0, 4)
	for i := range 4 {
		ta := rt.sched.graph.Add(&Action{
			Type:    ActionUpload,
			Path:    "file-" + string(rune('a'+i)) + ".txt",
			DriveID: eng.driveID,
		}, int64(i+1), nil)
		require.NotNil(t, ta)
		actions = append(actions, ta)
	}
	actions[0].AdmitAt = now.Add(100 * time.Millisecond)
	actions[1].AdmitAt = now.Add(200 * time.Millisecond)
	actions[2].AdmitAt = now.Add(300 * time.Millisecond)
	rt.replaceOutbox(actions)

	ch, next := rt.dispatchChannelForOutbox()
	require.NotNil(t, ch)
	assert.Equal(t, 3, next, "the due entry behind the parked ones goes first")
	rt.handleWatchDispatch(next)

	ch, next = rt.dispatchChannelForOutbox()
	assert.Nil(t, ch)
	assert.Equal(t, -1, next)
	assert.True(t, rt.hasAdmitTimer())
	assert.True(t, clock.HasPendingTimerAt(now.Add(100*time.Millisecond)), "the loop wakes at the earliest admission instant")
	select {
	case <-rt.admitTimerChan():
		require.Fail(t, "nothing is due yet")
	default:
	}

	clock.Advance(100 * time.Millisecond)
	select {
	case <-rt.admitTimerChan():
	default:
		require.Fail(t, "the admit timer must wake the loop when the member is due")
	}
	ch, next = rt.dispatchChannelForOutbox()
	require.NotNil(t, ch)
	assert.Equal(t, actions[0], rt.outboxEntry(next))
	rt.handleWatchDispatch(next)
	assert.Zero(t, actions[0].AdmitAt, "dispatch clears the admission instant")

	ch, _ = rt.dispatchChannelForOutbox()
	assert.Nil(t, ch)
	assert.True(t, clock.HasPendingTimerAt(now.Add(200*time.Millisecond)))

	clock.Advance(200 * time.Millisecond)
	for range 2 {
		ch, next = rt.dispatchChannelForOutbox()
		require.NotNil(t, ch, "both remaining members are due")
		rt.handleWatchDispatch(next)
	}
	ch, _ = rt.dispatchChannelForOutbox()
	assert.Nil(t, ch)
	assert.Empty(t, rt.currentOutbox())
	assert.False(t, rt.hasAdmitTimer(), "an empty outbox needs no wake-up")
}
