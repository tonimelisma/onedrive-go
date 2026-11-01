package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// throttleWindowClient wraps a scenario remote's client with the request
// budget a rate-limited OneDrive drive enforces: at most `budget` mutating
// requests per window of the engine's (virtual) clock; every request past
// that answers 429 with a Retry-After that reaches the end of the window.
// It also models Microsoft's penalty rule ("throttled requests count towards
// usage limits"; making calls while throttled "will extend the time it takes
// for your app to become unthrottled"): a request that arrives while a
// Retry-After is open is refused and pushes the deadline out by another
// window. It counts what it saw so a test can state how many requests the
// client wasted on 429s and how many it sent into an open penalty.
type throttleWindowClient struct {
	now    func() time.Time
	budget int
	window time.Duration

	mu           stdsync.Mutex // guards the fields below
	windowStart  time.Time
	inWindow     int
	penaltyUntil time.Time
	requests     int
	throttled    int
	penalized    int
}

func newThrottleWindowClient(now func() time.Time, budget int, window time.Duration) *throttleWindowClient {
	return &throttleWindowClient{now: now, budget: budget, window: window}
}

// admit charges one request to the window, or refuses it with a 429.
func (c *throttleWindowClient) admit() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.requests++
	if now.Before(c.penaltyUntil) {
		// Inside an open Retry-After: refused, and the penalty grows.
		c.penalized++
		c.throttled++
		c.penaltyUntil = c.penaltyUntil.Add(c.window)
		return c.throttledError(c.penaltyUntil.Sub(now))
	}
	if c.windowStart.IsZero() || !now.Before(c.windowStart.Add(c.window)) {
		c.windowStart = now
		c.inWindow = 0
	}
	c.inWindow++
	if c.inWindow <= c.budget {
		return nil
	}
	c.throttled++
	c.penaltyUntil = c.windowStart.Add(c.window)
	retryAfter := c.penaltyUntil.Sub(now)
	if retryAfter < time.Millisecond {
		retryAfter = time.Millisecond
	}

	return c.throttledError(retryAfter)
}

func (c *throttleWindowClient) throttledError(retryAfter time.Duration) error {
	return &graph.GraphError{
		StatusCode: http.StatusTooManyRequests,
		Code:       "activityLimitReached",
		Err:        graph.ErrThrottled,
		RetryAfter: retryAfter,
	}
}

// setBudget changes the per-window budget from the next window on.
func (c *throttleWindowClient) setBudget(budget int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.budget = budget
}

func (c *throttleWindowClient) counts() (requests, throttled, penalized int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.requests, c.throttled, c.penalized
}

// wrap puts the budget in front of every mutating call of inner. Observation
// (delta, lookups) stays free so the test is about execution.
func (c *throttleWindowClient) wrap(inner *engineMockClient) *engineMockClient {
	out := *inner
	out.uploadFn = func(
		ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if err := c.admit(); err != nil {
			return nil, err
		}
		return inner.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}
	out.uploadCreatingFn = func(
		ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		if err := c.admit(); err != nil {
			return nil, err
		}
		return inner.uploadCreatingFn(ctx, parentID, name, content, size, onCreated)
	}
	out.uploadToItemFn = func(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if err := c.admit(); err != nil {
			return nil, err
		}
		return inner.uploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)
	}
	out.createFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		if err := c.admit(); err != nil {
			return nil, err
		}
		return inner.createFolderFn(ctx, driveID, parentID, name)
	}

	return &out
}

// throttleScopeTrace is the story of one scope across the passes of a run,
// recorded from the engine's debug events: per pass, the sizes of the batches
// released (a release of the whole scope counts as its last batch), and the
// number of releases. The hook must never block (the recorder's channel
// would, with nobody reading it), so it only appends under a mutex.
type throttleScopeTrace struct {
	key ScopeKey

	mu       stdsync.Mutex // guards the fields below
	pass     int
	releases int
	lines    []string
	batches  [][]int
}

//nolint:gocritic // Value semantics match SetDebugEventHook, so the hook cannot mutate engine-owned events.
func (tr *throttleScopeTrace) record(event DebugEvent) {
	if event.ScopeKey != tr.key {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for len(tr.batches) < tr.pass {
		tr.batches = append(tr.batches, nil)
	}
	line, isBatch := "", false
	if event.Type == engineDebugEventScopeReleased {
		tr.releases++
		line, isBatch = fmt.Sprintf("released(%d)", event.Count), true
	}
	if event.Type == engineDebugEventScopeBatchReleased {
		line, isBatch = fmt.Sprintf("batch(%d)", event.Count), true
	}
	if event.Type == engineDebugEventScopeActivated {
		line = "activated"
	}
	if event.Type == engineDebugEventAdmittedWorkRetired {
		line = fmt.Sprintf("retired(%d)", event.Count)
	}
	if line == "" {
		return
	}
	tr.lines = append(tr.lines, line)
	if isBatch {
		tr.batches[tr.pass-1] = append(tr.batches[tr.pass-1], event.Count)
	}
}

func (tr *throttleScopeTrace) beginPass() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.pass++
	tr.lines = append(tr.lines, fmt.Sprintf("| pass %d:", tr.pass))
}

// throttledPassRun drives one-shot passes against the throttled fake under
// virtual time: each pass runs until it settles, and the clock then moves to
// the scope's trial, which is the window's end.
type throttledPassRun struct {
	h        *syncHarness
	limiter  *throttleWindowClient
	clock    *manualClock
	trace    *throttleScopeTrace
	scopeKey ScopeKey
	window   time.Duration

	// checkedDeadline records that one pass was started inside the
	// Retry-After and was seen to send nothing.
	checkedDeadline bool
}

// syncUntilConverged runs passes until the trees match or maxPasses is
// reached and returns the 429 count of each pass.
func (run *throttledPassRun) syncUntilConverged(t *testing.T, maxPasses int) []int {
	t.Helper()

	ctx := t.Context()
	throttledPerPass := make([]int, 0, maxPasses)
	for len(throttledPerPass) < maxPasses {
		_, throttledBefore, _ := run.limiter.counts()
		run.trace.beginPass()
		_, err := run.h.eng.RunOnce(ctx, SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		_, throttledAfter, _ := run.limiter.counts()
		throttledPerPass = append(throttledPerPass, throttledAfter-throttledBefore)

		if len(run.h.remote.tree()) == len(harnessLocalTree(t, run.h.syncRoot)) {
			break
		}
		block, ok := getTestBlockScope(run.h.eng, run.scopeKey)
		if !ok {
			run.clock.Advance(run.window)
			continue
		}
		if !run.checkedDeadline {
			run.requireSilentBeforeDeadline(t, block.NextTrialAt)
		}
		run.clock.Advance(block.NextTrialAt.Sub(run.clock.Now()))
	}

	return throttledPerPass
}

// requireSilentBeforeDeadline starts a pass halfway to the deadline: the
// persisted scope holds the whole plan until its trial is due, so nothing
// reaches the drive (R-6.8.24).
func (run *throttledPassRun) requireSilentBeforeDeadline(t *testing.T, deadline time.Time) {
	t.Helper()

	run.checkedDeadline = true
	requestsBefore, _, _ := run.limiter.counts()
	run.clock.Advance(deadline.Sub(run.clock.Now()) / 2)
	_, err := run.h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	requestsAfter, _, _ := run.limiter.counts()
	assert.Equal(t, requestsBefore, requestsAfter, "nothing may be sent to the drive before the Retry-After deadline")
}

// Validates: R-6.8.1, R-6.8.4, R-6.8.24, R-6.6.30
//
// Measured on a live account on 2026-11-01: with 10,000 small uploads planned
// against a rate-limited drive, every successful trial released the whole
// backlog at once, the workers drained it through 429 after 429, and the
// next trial repeated the flood: 22-28 requests a second, of which thousands
// per five minutes were 429s, while about ten files were uploaded and the
// scope was "released" 191 times in 15 minutes. Re-admission must ramp from
// a bounded batch, doubling only up to half the batch that last drew a 429
// and by one from there, a 429 inside the ramp must re-block, nothing may be
// sent to the drive before the Retry-After deadline (a run started before it
// sends nothing), and the admitted work a (re)activation blocks must be
// taken back before the workers reach it, so the 429s a throttling episode
// costs stay bounded by the workers in flight when it hits.
func TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding(t *testing.T) {
	t.Parallel()

	const (
		files         = 300
		workers       = 4
		windowBudget  = 50
		window        = time.Second
		maxPasses     = 40
		maxThrottled  = files / 10
		throttledNote = "429s must stay a small fraction of the requests"
	)

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	clock := newManualClock(time.Date(2026, 11, 1, 9, 7, 0, 0, time.UTC))
	limiter := newThrottleWindowClient(clock.Now, windowBudget, window)
	logger, logs := newCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, limiter.wrap(remote.client(t)), logger)
	eng.transferWorkers = workers
	installManualClockForTestEngine(eng, clock)
	scopeKey := SKThrottleDrive(driveID)
	trace := &throttleScopeTrace{key: scopeKey}
	eng.SetDebugEventHook(trace.record)
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}

	for i := range files {
		writeLocalFile(t, syncRoot, fmt.Sprintf("bulk/file-%03d.txt", i), fmt.Sprintf("content %d\n", i))
	}
	h.beginRound(t)

	run := throttledPassRun{h: h, limiter: limiter, clock: clock, trace: trace, scopeKey: scopeKey, window: window}
	throttledPerPass := run.syncUntilConverged(t, maxPasses)
	requests, throttled, penalized := limiter.counts()
	t.Logf("passes=%d requests=%d throttled=%d penalized=%d per-pass=%v",
		len(throttledPerPass), requests, throttled, penalized, throttledPerPass)

	require.Less(t, len(throttledPerPass), maxPasses, "the backlog never finished")
	require.True(t, run.checkedDeadline, "the scenario must have throttled at least once")
	assert.Equal(t, files, remote.uploads, "every upload completes exactly once")
	assert.Less(t, throttled, maxThrottled, throttledNote)
	throttledPasses := 0
	for pass, n := range throttledPerPass {
		assert.LessOrEqual(t, n, workers, "pass %d: a 429 may only reach the workers already in flight", pass+1)
		if n > 0 {
			throttledPasses++
		}
	}
	assert.LessOrEqual(t, penalized, (workers-1)*throttledPasses,
		"after a 429 only the transfers already in flight may still reach the drive; nothing new is sent before the deadline")

	// Once the window refills, the converged state must be quiet and every
	// invariant must hold; the extra pass of assertSyncInvariants needs a
	// fresh window too.
	clock.Advance(window)
	assertSyncInvariants(t, h)

	trace.mu.Lock()
	defer trace.mu.Unlock()
	releases, batches := trace.releases, trace.batches
	t.Logf("scope trace: %s", strings.Join(trace.lines, " "))
	output := logs.String()

	// The ramp: the first throttled pass retires the flood and ramps
	// nothing; the next pass ramps 4, 8, 16, 32 (one trial plus 29 fit the
	// window of 50, the 429 lands in the batch of 32); the pass after
	// remembers half of 32 and doubles only up to 16, then grows by one.
	require.GreaterOrEqual(t, len(batches), 3, "trace: %v", batches)
	assert.Empty(t, batches[0], "the first 429 interrupts no ramp")
	assert.Equal(t, []int{4, 8, 16, 32}, batches[1], "the first ramp doubles from the first batch")
	assert.Equal(t, []int{4, 8, 16, 17, 18}, batches[2], "the ramp after a 429 at 32 doubles up to 16 and then grows by one")
	assert.Equal(t, releases, strings.Count(output, "Resumed syncing"),
		"\"Resumed syncing\" is said once per actual release, not per batch")
	assert.LessOrEqual(t, releases, 2, "the scope is removed when its last blocked row is released, not per trial")
	assert.LessOrEqual(t, strings.Count(output, "OneDrive is limiting requests"), releases+1,
		"one announcement per throttling episode (R-6.6.30)")
}
