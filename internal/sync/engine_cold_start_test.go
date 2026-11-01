package sync

import (
	"context"
	"fmt"
	"io"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// gatedUploadClient parks every upload until the test releases it and
// records, per arrival, how many uploads were in flight at that instant
// (itself included). The test is the only thing that lets an upload finish,
// so what the engine lets in flight is exactly what the test sees parked.
type gatedUploadClient struct {
	mu       stdsync.Mutex // guards the fields below
	inFlight int
	arrivals []int
	gates    []chan struct{}
	open     bool

	arrived chan struct{}
}

func newGatedUploadClient() *gatedUploadClient {
	return &gatedUploadClient{arrived: make(chan struct{}, 1024)}
}

// wrap gates every upload path of inner (creates, replacements and simple
// uploads); observation and folder creates stay free.
func (c *gatedUploadClient) wrap(inner *engineMockClient) *engineMockClient {
	out := *inner
	out.uploadFn = func(
		ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if err := c.admit(ctx); err != nil {
			return nil, err
		}
		defer c.leave()
		return inner.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}
	out.uploadCreatingFn = func(
		ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		if err := c.admit(ctx); err != nil {
			return nil, err
		}
		defer c.leave()
		return inner.uploadCreatingFn(ctx, parentID, name, content, size, onCreated)
	}
	out.uploadToItemFn = func(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if err := c.admit(ctx); err != nil {
			return nil, err
		}
		defer c.leave()
		return inner.uploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
	}

	return &out
}

func (c *gatedUploadClient) admit(ctx context.Context) error {
	c.mu.Lock()
	c.inFlight++
	c.arrivals = append(c.arrivals, c.inFlight)
	gate := make(chan struct{})
	if c.open {
		close(gate)
	}
	c.gates = append(c.gates, gate)
	c.mu.Unlock()
	c.arrived <- struct{}{}

	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gated upload: %w", ctx.Err())
	}
}

func (c *gatedUploadClient) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
}

// release lets arrival i finish.
func (c *gatedUploadClient) release(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.gates[i]:
	default:
		close(c.gates[i])
	}
}

// releaseAll lets every parked and every future upload finish at once.
func (c *gatedUploadClient) releaseAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open = true
	for _, gate := range c.gates {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}
}

// waitArrivals blocks until n uploads have arrived in total.
func (c *gatedUploadClient) waitArrivals(t *testing.T, n int) {
	t.Helper()

	for c.arrivalCount() < n {
		select {
		case <-c.arrived:
		case <-time.After(debugEventTimeout):
			require.Failf(t, "uploads did not arrive", "waited for %d arrivals, have %d", n, c.arrivalCount())
		}
	}
}

func (c *gatedUploadClient) arrivalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.arrivals)
}

func (c *gatedUploadClient) arrivalsSnapshot() []int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]int(nil), c.arrivals...)
}

// coldStartTrace records the slow-start debug events of a run, per pass.
type coldStartTrace struct {
	mu     stdsync.Mutex // guards the fields below
	pass   int
	events [][]string
	raised chan int
}

func newColdStartTrace() *coldStartTrace {
	return &coldStartTrace{raised: make(chan int, 64)}
}

//nolint:gocritic // Value semantics match SetDebugEventHook, so the hook cannot mutate engine-owned events.
func (tr *coldStartTrace) record(event DebugEvent) {
	var line string
	if event.Type == engineDebugEventColdStartAllowanceRaised {
		line = fmt.Sprintf("raised(%d)", event.Count)
		select {
		case tr.raised <- event.Count:
		default:
		}
	}
	if event.Type == engineDebugEventColdStartHandedOver {
		line = "handed-over"
	}
	if line == "" {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for len(tr.events) < tr.pass {
		tr.events = append(tr.events, nil)
	}
	tr.events[tr.pass-1] = append(tr.events[tr.pass-1], line)
}

func (tr *coldStartTrace) beginPass() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.pass++
	tr.events = append(tr.events, nil)
}

func (tr *coldStartTrace) passEvents() [][]string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return append([][]string(nil), tr.events...)
}

func (tr *coldStartTrace) waitRaised(t *testing.T, want int) {
	t.Helper()

	select {
	case got := <-tr.raised:
		require.Equal(t, want, got, "slow-start allowance")
	case <-time.After(debugEventTimeout):
		require.Failf(t, "allowance not raised", "waited for the allowance to reach %d", want)
	}
}

// Validates: R-6.8.25
func TestColdStart_AllowanceDoublesPerRoundToThePoolSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ceiling   int
		successes int
		want      []int
	}{
		{name: "eight workers: 2, 4, 8", ceiling: 8, successes: 14, want: []int{4, 8}},
		{name: "the ceiling bounds the last doubling", ceiling: 6, successes: 6, want: []int{4, 6}},
		{name: "a pool of one admits one and never grows", ceiling: 1, successes: 3, want: nil},
		{name: "a round needs as many successes as the allowance", ceiling: 8, successes: 5, want: []int{4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newActionScheduler()
			s.resetColdStart(tc.ceiling)
			ta := &trackedAction{Action: Action{Type: ActionUpload, Path: "a.txt", DriveID: driveid.New(engineTestDriveID)}}
			assert.Equal(t, min(2, tc.ceiling), s.coldStartRampFor(coldStartTarget(ta)).allowance)

			var raised []int
			for range tc.successes {
				if allowance := s.noteColdStartSuccess(ta); allowance > 0 {
					raised = append(raised, allowance)
				}
			}
			assert.Equal(t, tc.want, raised)
		})
	}
}

// Validates: R-6.8.25
func TestColdStart_HandOverOpensTheTargetToTheScope(t *testing.T) {
	t.Parallel()

	driveA := driveid.New(engineTestDriveID)
	driveB := driveid.New("other-drive")
	s := newActionScheduler()
	s.resetColdStart(8)
	a := &trackedAction{Action: Action{Type: ActionUpload, Path: "a.txt", DriveID: driveA}}
	b := &trackedAction{Action: Action{Type: ActionUpload, Path: "b.txt", DriveID: driveB}}
	local := &trackedAction{Action: Action{Type: ActionLocalDelete, Path: "c.txt"}}

	a2 := &trackedAction{ID: 2, Action: Action{Type: ActionUpload, Path: "a2.txt", DriveID: driveA}}
	s.markRunning(a)
	s.markRunning(a2)
	assert.False(t, s.coldStartAdmits(&trackedAction{ID: 3, Action: a.Action}), "two in flight is the opening allowance")
	assert.True(t, s.coldStartAdmits(b), "each target ramps on its own")
	assert.True(t, s.coldStartAdmits(local), "an action that sends nothing to a drive is never gated")

	assert.Equal(t, []string{a.Action.ThrottleTargetKey()}, s.handOverColdStart(SKThrottleDrive(driveA)))
	assert.True(t, s.coldStartAdmits(&trackedAction{ID: 3, Action: a.Action}), "the scope owns the drive's admission now")
	assert.Empty(t, s.handOverColdStart(SKThrottleDrive(driveA)), "said once")
	assert.Empty(t, s.handOverColdStart(SKQuotaOwn()), "a quota scope says nothing about request rate")
	assert.Zero(t, s.noteColdStartSuccess(a), "a handed-over target no longer ramps")

	assert.ElementsMatch(t, []string{b.Action.ThrottleTargetKey()}, s.handOverColdStart(SKService()))
	c := &trackedAction{Action: Action{Type: ActionUpload, Path: "d.txt", DriveID: driveid.New("third-drive")}}
	assert.True(t, s.coldStartRampFor(coldStartTarget(c)).handedOver, "a service scope takes every later target too")

	s.resetColdStart(8)
	assert.False(t, s.coldStartRampFor(coldStartTarget(a)).handedOver, "a fresh plan starts over")
}

// Validates: R-6.8.25
//
// A fresh plan with no active scope opened at full worker concurrency into
// its backlog; on a personal OneDrive eight workers of small uploads made 19
// requests a second and drew the first 429 within a minute. A fresh plan
// must open with two in flight per drive and double per round of successful
// completions up to the worker pool size, and the whole backlog must still
// complete.
func TestColdStart_OneShotRampsInFlightFromTwo(t *testing.T) {
	t.Parallel()

	const (
		files   = 12
		workers = 8
	)

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	gate := newGatedUploadClient()
	eng, syncRoot := newTestEngine(t, gate.wrap(remote.client(t)))
	eng.transferWorkers = workers
	trace := newColdStartTrace()
	eng.SetDebugEventHook(trace.record)
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("file-%02d.txt", i), fmt.Sprintf("content %d\n", i))
	}

	trace.beginPass()
	runErr := make(chan error, 1)
	go func() {
		_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		runErr <- err
	}()

	// Round one: two in flight. One success keeps the allowance at two (a
	// third upload takes the freed slot); the second success doubles it.
	gate.waitArrivals(t, 2)
	gate.release(0)
	gate.waitArrivals(t, 3)
	gate.release(1)
	trace.waitRaised(t, 4)
	// Round two: four in flight (the third upload plus three more).
	gate.waitArrivals(t, 6)
	for i := 2; i < 6; i++ {
		gate.release(i)
	}
	trace.waitRaised(t, workers)
	gate.releaseAll()

	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(debugEventTimeout):
		require.Fail(t, "the pass did not finish")
	}

	arrivals := gate.arrivalsSnapshot()
	require.Len(t, arrivals, files, "every upload reaches the drive exactly once")
	for i, inFlight := range arrivals {
		switch {
		case i < 3:
			assert.LessOrEqual(t, inFlight, 2, "arrival %d: the first round allows two in flight", i+1)
		case i < 6:
			assert.LessOrEqual(t, inFlight, 4, "arrival %d: the second round allows four", i+1)
		default:
			assert.LessOrEqual(t, inFlight, workers, "arrival %d: never more than the pool", i+1)
		}
	}
	assert.Equal(t, files, remote.uploads)
	assert.Equal(t, [][]string{{"raised(4)", "raised(8)"}}, trace.passEvents())
}

// Validates: R-6.8.25, R-6.8.24
//
// A 429 during the slow start hands the drive to the throttle scope, which
// owns re-admission from there (its ramp, not the slow start, paces the next
// pass, which starts under the persisted scope), and a later fresh plan
// with no scope starts the slow start over.
func TestColdStart_OneShotHandsOverToTheThrottleScopeAndResetsPerPlan(t *testing.T) {
	t.Parallel()

	const (
		files        = 40
		workers      = 8
		windowBudget = 3
		window       = time.Second
	)

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	clock := newManualClock(time.Date(2026, 11, 1, 9, 7, 0, 0, time.UTC))
	limiter := newThrottleWindowClient(clock.Now, windowBudget, window)
	eng, syncRoot := newTestEngine(t, limiter.wrap(remote.client(t)))
	eng.transferWorkers = workers
	installManualClockForTestEngine(eng, clock)
	scopeKey := SKThrottleDrive(driveID)
	trace := newColdStartTrace()
	eng.SetDebugEventHook(trace.record)
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("file-%02d.txt", i), fmt.Sprintf("content %d\n", i))
	}
	h.beginRound(t)

	// Pass 1: two succeed, the allowance doubles, and the four that follow
	// find the window spent: 429s, bounded by the allowance of four.
	trace.beginPass()
	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	requests, throttled, _ := limiter.counts()
	assert.Equal(t, 6, requests, "two, then four: nothing more is sent after the 429")
	assert.Equal(t, 3, throttled)
	block, ok := getTestBlockScope(eng, scopeKey)
	require.True(t, ok, "the 429 activated the throttle scope")

	// Pass 2 starts under the persisted scope: the scope's ramp paces it,
	// the slow start does not, and the backlog completes.
	limiter.setBudget(1000)
	clock.Advance(block.NextTrialAt.Sub(clock.Now()))
	trace.beginPass()
	_, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, files, remote.uploads)
	_, ok = getTestBlockScope(eng, scopeKey)
	assert.False(t, ok, "the scope is released with its last batch")

	// Pass 3 is a fresh plan with no scope: the slow start begins again.
	for i := range 12 {
		writeLocalFile(t, syncRoot, fmt.Sprintf("more-%02d.txt", i), fmt.Sprintf("more %d\n", i))
	}
	h.beginRound(t)
	clock.Advance(window)
	trace.beginPass()
	_, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, files+12, remote.uploads)

	assert.Equal(t, [][]string{
		{"raised(4)", "handed-over"},
		{"handed-over"},
		{"raised(4)", "raised(8)"},
	}, trace.passEvents())

	clock.Advance(window)
	assertSyncInvariants(t, h)
}

// Validates: R-6.8.25
func TestWatchRuntime_DispatchIsGatedByTheColdStartAllowance(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.transferWorkers = 8
	rt := testWatchRuntime(t, eng)
	rt.sched.resetColdStart(eng.workerPoolSize())
	ctx := t.Context()

	uploads := make([]*trackedAction, 0, 7)
	for i := range 7 {
		ta := rt.sched.graph.Add(&Action{
			Type:    ActionUpload,
			Path:    fmt.Sprintf("file-%d.txt", i),
			DriveID: eng.driveID,
			View:    &pathView{Path: fmt.Sprintf("file-%d.txt", i)},
		}, int64(i+1), nil)
		require.NotNil(t, ta)
		uploads = append(uploads, ta)
	}
	rt.replaceOutbox(uploads)

	dispatch := func() *trackedAction {
		ch, next := rt.dispatchChannelForOutbox()
		if ch == nil {
			return nil
		}
		ta := rt.outboxEntry(next)
		rt.handleWatchDispatch(next)
		return ta
	}
	complete := func(ta *trackedAction) {
		t.Helper()
		ready, err := rt.applyRuntimeCompletionStage(ctx, trialSuccessForTest(ta), &Baseline{})
		require.NoError(t, err)
		assert.Empty(t, ready)
	}

	first, second := dispatch(), dispatch()
	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.Nil(t, dispatch(), "two in flight: the third waits for a completion, not for a worker")
	assert.Len(t, rt.currentOutbox(), 5)

	complete(first)
	third := dispatch()
	require.NotNil(t, third, "one success frees one slot; the allowance is still two")
	assert.Nil(t, dispatch())

	complete(second)
	for i := range 3 {
		require.NotNil(t, dispatch(), "after a round of two successes four may fly (%d)", i)
	}
	assert.Nil(t, dispatch(), "four in flight under an allowance of four")
	assert.Len(t, rt.currentOutbox(), 1)
	assert.Equal(t, 4, rt.sched.runningCount)
}
