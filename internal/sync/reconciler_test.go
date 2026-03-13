package sync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Mock types for reconciler tests
// ---------------------------------------------------------------------------

// mockStateReader implements StateReader for failure retrier tests.
type mockStateReader struct {
	mu                 stdsync.Mutex
	failureRows        []SyncFailureRow
	failureErr         error
	earliestRetry      time.Time
	earliestRetryErr   error
	failureCount       int
	failureCountErr    error
	listFailureCalls   int
	earliestRetryCalls int
}

func (m *mockStateReader) ListUnreconciled(_ context.Context) ([]RemoteStateRow, error) {
	return nil, nil
}

func (m *mockStateReader) ListSyncFailuresForRetry(_ context.Context, _ time.Time) ([]SyncFailureRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listFailureCalls++

	return m.failureRows, m.failureErr
}

func (m *mockStateReader) EarliestSyncFailureRetryAt(_ context.Context, _ time.Time) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.earliestRetryCalls++

	return m.earliestRetry, m.earliestRetryErr
}

func (m *mockStateReader) SyncFailureCount(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.failureCount, m.failureCountErr
}

func (m *mockStateReader) BaselineEntryCount(_ context.Context) (int, error)      { return 0, nil }
func (m *mockStateReader) UnresolvedConflictCount(_ context.Context) (int, error) { return 0, nil }
func (m *mockStateReader) ReadSyncMetadata(_ context.Context) (map[string]string, error) {
	return nil, nil
}

// mockEventAdder implements EventAdder for failure retrier tests.
type mockEventAdder struct {
	mu     stdsync.Mutex
	events []*ChangeEvent
}

func (m *mockEventAdder) Add(ev *ChangeEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.events = append(m.events, ev)
}

// mockInFlightChecker implements InFlightChecker for failure retrier tests.
type mockInFlightChecker struct {
	mu    stdsync.Mutex
	paths map[string]bool
}

func newMockInFlightChecker() *mockInFlightChecker {
	return &mockInFlightChecker{paths: make(map[string]bool)}
}

func (m *mockInFlightChecker) HasInFlight(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.paths[path]
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testFailureRetrier(state *mockStateReader, adder *mockEventAdder, checker *mockInFlightChecker) *FailureRetrier {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	return NewFailureRetrier(state, adder, checker, logger)
}

func makeFailedRow(path, direction string, failureCount int) SyncFailureRow {
	driveID := driveid.New("00000000000d0001")

	return SyncFailureRow{
		DriveID:      driveID,
		ItemID:       "item-" + path,
		Path:         path,
		Direction:    direction,
		Category:     "transient",
		IssueType:    direction + "_failed",
		FailureCount: failureCount,
		LastError:    "some error",
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNewFailureRetrier(t *testing.T) {
	state := &mockStateReader{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)

	require.NotNil(t, r)
	assert.NotNil(t, r.kickCh)
	assert.Equal(t, 1, cap(r.kickCh))
}

func TestKick_Coalescing(t *testing.T) {
	state := &mockStateReader{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)

	// First kick should succeed (buffered channel has capacity 1).
	r.Kick()
	assert.Len(t, r.kickCh, 1)

	// Second kick should be coalesced (default branch).
	r.Kick()
	assert.Len(t, r.kickCh, 1, "second kick should coalesce")

	// Drain and verify only one signal.
	<-r.kickCh
	assert.Len(t, r.kickCh, 0)
}

// Validates: R-6.5.3
func TestReconcile_DispatchRetriableItems(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
		makeFailedRow("b.txt", strDelete, 3),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 2)

	// First: download → ChangeModify
	assert.Equal(t, "a.txt", adder.events[0].Path)
	assert.Equal(t, ChangeModify, adder.events[0].Type)
	assert.Equal(t, SourceRemote, adder.events[0].Source)

	// Second: delete → ChangeDelete
	assert.Equal(t, "b.txt", adder.events[1].Path)
	assert.Equal(t, ChangeDelete, adder.events[1].Type)
}

func TestReconcile_SkipInFlight(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
		makeFailedRow("b.txt", strDownload, 3),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()
	checker.paths["a.txt"] = true // a.txt is in-flight

	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	// Only b.txt dispatched; a.txt skipped because it's in-flight.
	require.Len(t, adder.events, 1)
	assert.Equal(t, "b.txt", adder.events[0].Path)
}

func TestReconcile_HighFailureCountStillRetries(t *testing.T) {
	// With no escalation threshold, even high failure counts are retried.
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 50),
		makeFailedRow("b.txt", strUpload, 100),
		makeFailedRow("c.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	// All three items should be dispatched — no escalation.
	require.Len(t, adder.events, 3)
	assert.Equal(t, "a.txt", adder.events[0].Path)
	assert.Equal(t, "b.txt", adder.events[1].Path)
	assert.Equal(t, "c.txt", adder.events[2].Path)
}

func TestSynthesizeFailureEvent_Directions(t *testing.T) {
	r := testFailureRetrier(&mockStateReader{}, &mockEventAdder{}, newMockInFlightChecker())

	tests := []struct {
		name       string
		direction  string
		wantType   ChangeType
		wantSource ChangeSource
	}{
		{strDownload, strDownload, ChangeModify, SourceRemote},
		{strUpload, strUpload, ChangeModify, SourceLocal},
		{strDelete, strDelete, ChangeDelete, SourceRemote},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := makeFailedRow("test.txt", tt.direction, 1)
			ev := r.synthesizeFailureEvent(&row)

			require.NotNil(t, ev)
			assert.Equal(t, tt.wantType, ev.Type)
			assert.Equal(t, tt.wantSource, ev.Source)
			assert.Equal(t, "test.txt", ev.Path)
			assert.Equal(t, tt.wantType == ChangeDelete, ev.IsDeleted)

			// Download and delete events carry item metadata from the row.
			if tt.direction != strUpload {
				assert.Equal(t, row.ItemID, ev.ItemID)
				assert.Equal(t, row.DriveID, ev.DriveID)
			}
		})
	}
}

func TestReconcile_NoRows(t *testing.T) {
	state := &mockStateReader{failureRows: nil}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	assert.Empty(t, adder.events)
	adder.mu.Unlock()
}

func TestReconcile_ListFailedError(t *testing.T) {
	state := &mockStateReader{failureErr: errors.New("query error")}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// Should not panic; error is logged, no events dispatched.
	r.reconcile(context.Background())

	adder.mu.Lock()
	assert.Empty(t, adder.events)
	adder.mu.Unlock()
}

func TestArmTimer_FutureRetry(t *testing.T) {
	now := time.Unix(1000, 0)
	future := now.Add(30 * time.Second)

	state := &mockStateReader{earliestRetry: future}
	r := testFailureRetrier(state, &mockEventAdder{}, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	r.armTimer(context.Background(), now)

	r.mu.Lock()
	require.NotNil(t, r.timer, "timer should be armed for future retry")
	r.timer.Stop()
	r.mu.Unlock()
}

func TestArmTimer_NoRetry(t *testing.T) {
	state := &mockStateReader{earliestRetry: time.Time{}} // zero = no pending retries
	r := testFailureRetrier(state, &mockEventAdder{}, newMockInFlightChecker())

	r.armTimer(context.Background(), time.Unix(1000, 0))

	r.mu.Lock()
	assert.Nil(t, r.timer, "timer should not be armed when no pending retries")
	r.mu.Unlock()
}

func TestArmTimer_PastRetry_Kicks(t *testing.T) {
	now := time.Unix(1000, 0)
	past := now.Add(-5 * time.Second)

	state := &mockStateReader{earliestRetry: past}
	r := testFailureRetrier(state, &mockEventAdder{}, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	r.armTimer(context.Background(), now)

	// Past retry should trigger an immediate kick.
	select {
	case <-r.kickCh:
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("expected immediate kick for past retry")
	}
}

func TestRun_ShutdownOnCancel(t *testing.T) {
	state := &mockStateReader{} // no rows, no timer
	r := testFailureRetrier(state, &mockEventAdder{}, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Give Run time to start and do the bootstrap reconcile.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// Run exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestRun_KickTriggersReconcile(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Bootstrap reconcile dispatches the row. Clear events and simulate
	// re-failure (new NextRetryAt) so the kick dispatches the updated row.
	time.Sleep(50 * time.Millisecond)

	adder.mu.Lock()
	adder.events = nil
	adder.mu.Unlock()

	state.mu.Lock()
	state.failureRows[0].NextRetryAt = 2000000000 // simulate re-failure with new backoff
	state.mu.Unlock()

	// Kick triggers another reconcile — row has new NextRetryAt, so it's dispatched.
	r.Kick()
	time.Sleep(100 * time.Millisecond)

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	assert.Equal(t, "a.txt", adder.events[0].Path)
	adder.mu.Unlock()

	cancel()
	<-done
}

func TestArmTimer_StopsExistingTimer(t *testing.T) {
	now := time.Unix(1000, 0)
	future := now.Add(10 * time.Minute)

	state := &mockStateReader{earliestRetry: future}
	r := testFailureRetrier(state, &mockEventAdder{}, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	// Arm once.
	r.armTimer(context.Background(), now)
	r.mu.Lock()
	require.NotNil(t, r.timer)
	firstTimer := r.timer
	r.mu.Unlock()

	// Arm again — should stop the first timer and create a new one.
	r.armTimer(context.Background(), now)
	r.mu.Lock()
	require.NotNil(t, r.timer)
	assert.NotSame(t, firstTimer, r.timer, "armTimer should create a new timer")
	r.timer.Stop()
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Upload failure retry tests
// ---------------------------------------------------------------------------

func TestReconcile_DispatchUploadFailures(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strUpload, 2),
		makeFailedRow("b.txt", strUpload, 3),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}

	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 2)
	assert.Equal(t, "a.txt", adder.events[0].Path)
	assert.Equal(t, SourceLocal, adder.events[0].Source)
	assert.Equal(t, ChangeModify, adder.events[0].Type)
	assert.Equal(t, "b.txt", adder.events[1].Path)
}

func TestReconcile_PreventsDoubleDispatch(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// First reconcile dispatches the row.
	r.reconcile(context.Background())

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	adder.mu.Unlock()

	// Second reconcile with same row (same NextRetryAt) — should not
	// re-dispatch. This is the bootstrap-vs-kick race guard.
	r.reconcile(context.Background())

	adder.mu.Lock()
	assert.Len(t, adder.events, 1, "same row should not be dispatched twice")
	adder.mu.Unlock()
}

func TestReconcile_ReDispatchesAfterRetryAtChange(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// First reconcile dispatches the row.
	r.reconcile(context.Background())

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	adder.mu.Unlock()

	// Simulate re-failure: RecordFailure sets a new next_retry_at.
	state.mu.Lock()
	state.failureRows[0].NextRetryAt = 2000000000
	state.mu.Unlock()

	// Second reconcile should dispatch (different NextRetryAt).
	r.reconcile(context.Background())

	adder.mu.Lock()
	assert.Len(t, adder.events, 2, "row with new NextRetryAt should be re-dispatched")
	adder.mu.Unlock()
}

func TestReconcile_ClearsDispatchedOnInFlight(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()
	r := testFailureRetrier(state, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// First reconcile dispatches.
	r.reconcile(context.Background())

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	adder.mu.Unlock()
	require.Contains(t, r.dispatchedRetryAt, "a.txt")

	// Path goes in-flight (pipeline consumed it).
	checker.mu.Lock()
	checker.paths["a.txt"] = true
	checker.mu.Unlock()

	r.reconcile(context.Background())
	assert.NotContains(t, r.dispatchedRetryAt, "a.txt",
		"in-flight path should be cleared from dispatch tracking")
}

func TestReconcile_PrunesStaleDispatchedEntries(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
		makeFailedRow("b.txt", strUpload, 1),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// First reconcile dispatches both.
	r.reconcile(context.Background())
	require.Len(t, r.dispatchedRetryAt, 2)

	// Simulate: a.txt succeeded (row cleared from sync_failures).
	state.mu.Lock()
	state.failureRows = []SyncFailureRow{rows[1]}
	state.mu.Unlock()

	r.reconcile(context.Background())
	assert.NotContains(t, r.dispatchedRetryAt, "a.txt",
		"resolved path should be pruned from dispatch tracking")
	assert.Contains(t, r.dispatchedRetryAt, "b.txt",
		"active path should remain in dispatch tracking")
}

func TestReconcile_PrunesAllWhenNoRows(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", strDownload, 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// First reconcile dispatches.
	r.reconcile(context.Background())
	require.Len(t, r.dispatchedRetryAt, 1)

	// All failures resolved.
	state.mu.Lock()
	state.failureRows = nil
	state.mu.Unlock()

	r.reconcile(context.Background())
	assert.Empty(t, r.dispatchedRetryAt,
		"all entries should be pruned when no rows remain")
}

func TestRun_BootstrapKickRace_NoDuplicateDispatch(t *testing.T) {
	// Reproduces the bootstrap-vs-kick race: a failure is recorded and
	// Kick() is called before Run() starts. Both bootstrap reconcile and
	// the pending kick see the same row. The dispatchedRetryAt guard
	// ensures only one event is buffered.
	rows := []SyncFailureRow{
		makeFailedRow("race.txt", strUpload, 1),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// Pre-load kick signal before Run starts — simulates the race where
	// processWorkerResult calls Kick() before the retrier goroutine executes.
	r.Kick()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Wait for both bootstrap and kick reconcile to complete.
	time.Sleep(100 * time.Millisecond)

	adder.mu.Lock()
	assert.Len(t, adder.events, 1,
		"bootstrap + kick should produce exactly one dispatch, not two")
	adder.mu.Unlock()

	cancel()
	<-done
}

func TestReconcile_MixedDirections(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("remote.txt", strDownload, 2),
		makeFailedRow("local.txt", strUpload, 1),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}

	r := testFailureRetrier(state, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 2)

	// First event from download (SourceRemote), second from upload (SourceLocal).
	assert.Equal(t, SourceRemote, adder.events[0].Source)
	assert.Equal(t, "remote.txt", adder.events[0].Path)
	assert.Equal(t, SourceLocal, adder.events[1].Source)
	assert.Equal(t, "local.txt", adder.events[1].Path)
}
