package sync

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// goroutineIDBufferSize holds the first line of a goroutine's stack, which
// names its ID.
const goroutineIDBufferSize = 64

// currentGoroutineID is the ID the runtime gives the calling goroutine, read
// from the first line of its stack ("goroutine 18 [running]:").
func currentGoroutineID(t *testing.T) uint64 {
	t.Helper()

	buf := make([]byte, goroutineIDBufferSize)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	id, err := strconv.ParseUint(string(buf[:bytes.IndexByte(buf, ' ')]), 10, 64)
	require.NoError(t, err, "parse the goroutine ID")

	return id
}

// newOwnerCheck is an owner check for the calling goroutine: the returned
// function calls violated when it runs on any other goroutine.
func newOwnerCheck(t *testing.T, violated func()) func() {
	t.Helper()

	owner := currentGoroutineID(t)

	return func() {
		if currentGoroutineID(t) != owner {
			violated()
		}
	}
}

// installOwnerCheck makes the calling goroutine the owner of rt's scope
// ledger, timers and dependency graph: an accessor of any of them called
// from another goroutine fails the test.
func installOwnerCheck(t *testing.T, rt *watchRuntime) {
	t.Helper()

	check := newOwnerCheck(t, func() {
		assert.Fail(t, "a run-owned structure was used off the run's goroutine")
	})
	rt.sched.SetOwnerCheck(check)
	rt.timers.ownerCheck = check
	rt.sched.SetOwnerCheck(check)
}

// assertOwnerCheckFires proves an owner check installed through install
// catches access, the call of one accessor, from a second goroutine, and not
// from the owner.
func assertOwnerCheckFires(t *testing.T, install func(func()), access func()) {
	t.Helper()

	violations := make(chan struct{}, 1)
	install(newOwnerCheck(t, func() { violations <- struct{}{} }))

	access()
	assert.Empty(t, violations, "the owner's own call")

	accessed := make(chan struct{})
	go func() {
		defer close(accessed)
		access()
	}()
	syncfixture.ReceiveWithin(t, accessed, "the second goroutine's call")
	assert.Len(t, violations, 1, "the second goroutine's call is caught")
}

// Validates: R-6.3.4
func TestOwnerCheck_DependencyGraphCatchesASecondGoroutine(t *testing.T) {
	t.Parallel()

	graph := syncsched.NewDepGraph(synctest.TestLogger(t))
	assertOwnerCheckFires(t,
		func(check func()) { graph.SetOwnerCheck(check) },
		func() { graph.InFlightCount() },
	)
}

// Validates: R-6.3.4
func TestOwnerCheck_SchedulerScopesCatchASecondGoroutine(t *testing.T) {
	t.Parallel()

	scheduler := syncsched.New(time.Now, synctest.TestLogger(t), nil)
	assertOwnerCheckFires(t,
		func(check func()) { scheduler.SetOwnerCheck(check) },
		func() { scheduler.HasScope(syncdomain.SKService()) },
	)
}

// Validates: R-6.3.4
func TestOwnerCheck_WatchTimersCatchASecondGoroutine(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	assertOwnerCheckFires(t,
		func(check func()) { rt.timers.ownerCheck = check },
		func() { rt.hasTrialTimer() },
	)
}

// Every watch timer's callback runs on a goroutine of its own and only
// signals the channel it was handed: the loop's goroutine arms the four
// timers, a second goroutine fires them through the manual clock, and none
// of the callbacks touches the ledger, the timers or the graph (the owner
// check would fail the test) while the loop's goroutine is parked.
//
// Validates: R-6.3.4
func TestWatchRuntime_TimerCallbacksTouchOnlyChannels(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	clock := syncfixture.NewManualClock(time.Now())
	installManualClockForTestEngine(eng, clock)
	rt := testWatchRuntime(t, eng)
	installOwnerCheck(t, rt)
	due := clock.Now().Add(time.Minute)

	rt.sched.PutScope(&syncdomain.ActiveScope{Key: syncdomain.SKService(), TrialInterval: time.Minute, NextTrialAt: due})
	rt.sched.Hold(&syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "scoped.txt"}},
		syncsched.HeldScope, syncdomain.SKService(), time.Time{})
	rt.armTrialTimer()
	rt.sched.Hold(&syncdomain.TrackedAction{Action: syncdomain.Action{Type: syncdomain.ActionUpload, Path: "held.txt"}},
		syncsched.HeldRetry, syncdomain.ScopeKey{}, due)
	rt.armRetryTimer()
	rt.armAdmitTimer(due)
	require.NoError(t, rt.armFullRefreshTimer(t.Context()))
	require.True(t, rt.hasTrialTimer())
	require.True(t, rt.hasRetryTimer())
	require.True(t, rt.hasAdmitTimer())

	fired := make(chan struct{})
	go func() {
		defer close(fired)
		clock.Advance(syncdomain.FullRemoteRefreshInterval + time.Minute)
	}()
	syncfixture.ReceiveWithin(t, fired, "the second goroutine to fire the timers")

	syncfixture.ReceiveWithin(t, rt.timers.trialCh, "the trial timer's signal")
	syncfixture.ReceiveWithin(t, rt.timers.retryTimerCh, "the retry timer's signal")
	syncfixture.ReceiveWithin(t, rt.timers.admitCh, "the admit timer's signal")
	syncfixture.ReceiveWithin(t, rt.resources.refreshCh, "the full refresh timer's signal")
}

// runOwnership counts, for an engine whose runs carry an owner check, the
// checks that ran by structure and the ones that ran off the run's goroutine.
type runOwnership struct {
	mu         stdsync.Mutex
	byOwner    map[string]int
	violations int
}

// ownerOfCheck names the structure whose accessor ran the check: the first
// frame above the check that is a method of one of the three (inlined frames
// included, which runtime.CallersFrames reports).
func ownerOfCheck() string {
	pcs := make([]uintptr, ownerCheckFrames)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(ownerCheckCallerSkip, pcs)])
	for {
		frame, more := frames.Next()
		for _, owner := range []string{"(*DepGraph)", "(*Scheduler)", "(*watchTimerState)"} {
			if strings.Contains(frame.Function, owner) {
				return owner
			}
		}
		if !more {
			return "no run-owned structure"
		}
	}
}

// ownerCheckCallerSkip skips runtime.Callers and ownerOfCheck;
// ownerCheckFrames is how far up the search looks.
const (
	ownerCheckCallerSkip = 2
	ownerCheckFrames     = 8
)

// requireRunOwnership makes every run of eng carry an owner check bound to
// the goroutine that starts the run.
func requireRunOwnership(t *testing.T, eng *testEngine) *runOwnership {
	t.Helper()

	ownership := &runOwnership{byOwner: map[string]int{}}
	eng.hooks.ownerCheckFactory = func() func() {
		owner := currentGoroutineID(t)
		return func() {
			onOwner := currentGoroutineID(t) == owner
			ownership.mu.Lock()
			defer ownership.mu.Unlock()
			ownership.byOwner[ownerOfCheck()]++
			if !onOwner {
				ownership.violations++
			}
		}
	}

	return ownership
}

// assertRunOwned requires checks of each named structure and no violation.
func (o *runOwnership) assertRunOwned(t *testing.T, owners ...string) {
	t.Helper()

	o.mu.Lock()
	defer o.mu.Unlock()
	assert.Zero(t, o.violations, "a run-owned structure was used off the run's goroutine")
	for _, owner := range owners {
		assert.Positive(t, o.byOwner[owner], "%s ran its owner check: %v", owner, o.byOwner)
	}
}

// A one-shot pass that downloads a file uses the scope ledger and the
// dependency graph its start stage builds from its own goroutine alone.
//
// Validates: R-6.3.4
func TestRunOnce_UsesRunStateOnlyFromItsGoroutine(t *testing.T) {
	t.Parallel()

	const content = "owned!"
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := newDownloadDeltaMock(driveID, &graph.Item{
		ID: "item-a", Name: "owned.txt", ParentID: "root",
		DriveID: driveID, Size: int64(len(content)), QuickXorHash: syncfixture.TestQuickXor(content),
	}, "token-owned", []byte(content))
	eng, _ := newTestEngine(t, mock)
	ownership := requireRunOwnership(t, eng)

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Downloads)
	ownership.assertRunOwned(t, "(*DepGraph)", "(*Scheduler)")
}

// A watch whose bootstrap downloads a file uses the scope ledger, its timers
// and both dependency graphs (the one set up before the bootstrap and the one
// the bootstrap's plan rebuilds) from the run's goroutine alone, while its
// observers, workers and timer callbacks run beside it.
//
// Validates: R-6.3.4
func TestRunWatch_UsesRunStateOnlyFromItsGoroutine(t *testing.T) {
	t.Parallel()

	const content = "watched!"
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := newDownloadDeltaMock(driveID, &graph.Item{
		ID: "item-a", Name: "watched.txt", ParentID: "root",
		DriveID: driveID, Size: int64(len(content)), QuickXorHash: syncfixture.TestQuickXor(content),
	}, "token-watched", []byte(content))
	eng, _ := newTestEngine(t, mock)
	ownership := requireRunOwnership(t, eng)
	recorder := attachDebugEventRecorder(eng)

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncBidirectional, quietWatchOptions())
	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	syncfixture.StopWatch(t, cancel, done)
	ownership.assertRunOwned(t, "(*DepGraph)", "(*Scheduler)", "(*watchTimerState)")
}
