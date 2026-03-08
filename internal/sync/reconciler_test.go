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

// mockEscalator implements ConflictEscalator for failure retrier tests.
type mockEscalator struct {
	mu    stdsync.Mutex
	calls []escalateCall
	err   error
}

type escalateCall struct {
	driveID driveid.ID
	itemID  string
	path    string
	reason  string
}

func (m *mockEscalator) EscalateToConflict(_ context.Context, driveID driveid.ID, itemID, path, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, escalateCall{driveID: driveID, itemID: itemID, path: path, reason: reason})

	return m.err
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

// mockSyncFailureRecorder implements SyncFailureRecorder for failure retrier tests.
type mockSyncFailureRecorder struct {
	mu              stdsync.Mutex
	permanentCalls  []permanentCall
	permanentErr    error
	recordCalls     int
	clearCalls      int
	clearResCalls   int
	listFailures    []SyncFailureRow
	listFailuresErr error
}

type permanentCall struct {
	path    string
	driveID driveid.ID
}

func (m *mockSyncFailureRecorder) RecordSyncFailure(_ context.Context, _ string, _ driveid.ID,
	_, _, _ string, _ int, _ int64, _, _ string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.recordCalls++

	return nil
}

func (m *mockSyncFailureRecorder) ListSyncFailures(_ context.Context) ([]SyncFailureRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.listFailures, m.listFailuresErr
}

func (m *mockSyncFailureRecorder) ClearSyncFailure(_ context.Context, _ string, _ driveid.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.clearCalls++

	return nil
}

func (m *mockSyncFailureRecorder) ClearResolvedSyncFailures(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.clearResCalls++

	return nil
}

func (m *mockSyncFailureRecorder) MarkSyncFailurePermanent(_ context.Context, path string, driveID driveid.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.permanentCalls = append(m.permanentCalls, permanentCall{path: path, driveID: driveID})

	return m.permanentErr
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

func testFailureRetrier(state *mockStateReader, esc *mockEscalator, adder *mockEventAdder, checker *mockInFlightChecker) *FailureRetrier {
	return testFailureRetrierWith(state, esc, nil, adder, checker)
}

func testFailureRetrierWith(state *mockStateReader, esc *mockEscalator, recorder *mockSyncFailureRecorder, adder *mockEventAdder, checker *mockInFlightChecker) *FailureRetrier {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Convert nil *mockSyncFailureRecorder to nil SyncFailureRecorder interface.
	var sfr SyncFailureRecorder
	if recorder != nil {
		sfr = recorder
	}

	return NewFailureRetrier(DefaultFailureRetrierConfig(), state, esc, sfr, adder, checker, logger)
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
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)

	require.NotNil(t, r)
	assert.NotNil(t, r.kickCh)
	assert.Equal(t, 1, cap(r.kickCh))
}

func TestKick_Coalescing(t *testing.T) {
	state := &mockStateReader{}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)

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

func TestReconcile_DispatchRetriableItems(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", "download", 2),
		makeFailedRow("b.txt", "delete", 3),
	}
	state := &mockStateReader{failureRows: rows}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
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
		makeFailedRow("a.txt", "download", 2),
		makeFailedRow("b.txt", "download", 3),
	}
	state := &mockStateReader{failureRows: rows}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()
	checker.paths["a.txt"] = true // a.txt is in-flight

	r := testFailureRetrier(state, esc, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	// Only b.txt dispatched; a.txt skipped because it's in-flight.
	require.Len(t, adder.events, 1)
	assert.Equal(t, "b.txt", adder.events[0].Path)
}

func TestReconcile_EscalationThreshold(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", "download", defaultEscalationThreshold),
		makeFailedRow("b.txt", "download", defaultEscalationThreshold+5),
		makeFailedRow("c.txt", "download", 2), // below threshold
	}
	state := &mockStateReader{failureRows: rows}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	// a.txt and b.txt escalated, c.txt dispatched.
	esc.mu.Lock()
	defer esc.mu.Unlock()

	require.Len(t, esc.calls, 2)
	assert.Equal(t, "a.txt", esc.calls[0].path)
	assert.Equal(t, "b.txt", esc.calls[1].path)

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 1)
	assert.Equal(t, "c.txt", adder.events[0].Path)
}

func TestReconcile_EscalationError(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", "download", defaultEscalationThreshold),
	}
	state := &mockStateReader{failureRows: rows}
	esc := &mockEscalator{err: errors.New("db error")}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// Should not panic; error is logged.
	r.reconcile(context.Background())

	esc.mu.Lock()
	require.Len(t, esc.calls, 1)
	esc.mu.Unlock()

	adder.mu.Lock()
	assert.Empty(t, adder.events, "escalated item should not also be dispatched")
	adder.mu.Unlock()
}

func TestSynthesizeFailureEvent_Directions(t *testing.T) {
	r := testFailureRetrier(&mockStateReader{}, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())

	tests := []struct {
		name       string
		direction  string
		wantType   ChangeType
		wantSource ChangeSource
	}{
		{"download", "download", ChangeModify, SourceRemote},
		{"upload", "upload", ChangeModify, SourceLocal},
		{"delete", "delete", ChangeDelete, SourceRemote},
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
			if tt.direction != "upload" {
				assert.Equal(t, row.ItemID, ev.ItemID)
				assert.Equal(t, row.DriveID, ev.DriveID)
			}
		})
	}
}

func TestReconcile_NoRows(t *testing.T) {
	state := &mockStateReader{failureRows: nil}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	assert.Empty(t, adder.events)
	adder.mu.Unlock()

	esc.mu.Lock()
	assert.Empty(t, esc.calls)
	esc.mu.Unlock()
}

func TestReconcile_ListFailedError(t *testing.T) {
	state := &mockStateReader{failureErr: errors.New("query error")}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
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
	r := testFailureRetrier(state, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	r.armTimer(context.Background(), now)

	r.mu.Lock()
	require.NotNil(t, r.timer, "timer should be armed for future retry")
	r.timer.Stop()
	r.mu.Unlock()
}

func TestArmTimer_NoRetry(t *testing.T) {
	state := &mockStateReader{earliestRetry: time.Time{}} // zero = no pending retries
	r := testFailureRetrier(state, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())

	r.armTimer(context.Background(), time.Unix(1000, 0))

	r.mu.Lock()
	assert.Nil(t, r.timer, "timer should not be armed when no pending retries")
	r.mu.Unlock()
}

func TestArmTimer_PastRetry_Kicks(t *testing.T) {
	now := time.Unix(1000, 0)
	past := now.Add(-5 * time.Second)

	state := &mockStateReader{earliestRetry: past}
	r := testFailureRetrier(state, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())
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
	r := testFailureRetrier(state, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())
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
		makeFailedRow("a.txt", "download", 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, &mockEscalator{}, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	// Bootstrap reconcile dispatches the row. Clear it so the kick dispatches
	// fresh rows.
	time.Sleep(50 * time.Millisecond)

	adder.mu.Lock()
	adder.events = nil
	adder.mu.Unlock()

	// Kick triggers another reconcile.
	r.Kick()
	time.Sleep(100 * time.Millisecond)

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	assert.Equal(t, "a.txt", adder.events[0].Path)
	adder.mu.Unlock()

	cancel()
	<-done
}

func TestDefaultFailureRetrierConfig(t *testing.T) {
	cfg := DefaultFailureRetrierConfig()
	assert.Equal(t, 10, cfg.EscalationThreshold)
}

func TestFailureRetrier_CustomEscalationThreshold(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", "download", 3),
		makeFailedRow("b.txt", "download", 2), // below threshold
	}
	state := &mockStateReader{failureRows: rows}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := FailureRetrierConfig{EscalationThreshold: 3}
	r := NewFailureRetrier(cfg, state, esc, nil, adder, checker, logger)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	esc.mu.Lock()
	require.Len(t, esc.calls, 1, "a.txt should be escalated at threshold=3")
	assert.Equal(t, "a.txt", esc.calls[0].path)
	esc.mu.Unlock()

	adder.mu.Lock()
	require.Len(t, adder.events, 1, "b.txt should be dispatched")
	assert.Equal(t, "b.txt", adder.events[0].Path)
	adder.mu.Unlock()
}

func TestArmTimer_StopsExistingTimer(t *testing.T) {
	now := time.Unix(1000, 0)
	future := now.Add(10 * time.Minute)

	state := &mockStateReader{earliestRetry: future}
	r := testFailureRetrier(state, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())
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
// Upload failure (local issue) retry tests
// ---------------------------------------------------------------------------

func TestReconcile_DispatchUploadFailures(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("a.txt", "upload", 2),
		makeFailedRow("b.txt", "upload", 3),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	recorder := &mockSyncFailureRecorder{}

	r := testFailureRetrierWith(state, &mockEscalator{}, recorder, adder, newMockInFlightChecker())
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

func TestReconcile_EscalateUploadFailure(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("bad.txt", "upload", defaultEscalationThreshold),
		makeFailedRow("ok.txt", "upload", 2),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	recorder := &mockSyncFailureRecorder{}

	r := testFailureRetrierWith(state, &mockEscalator{}, recorder, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	recorder.mu.Lock()
	require.Len(t, recorder.permanentCalls, 1)
	assert.Equal(t, "bad.txt", recorder.permanentCalls[0].path)
	assert.Equal(t, driveid.New("00000000000d0001"), recorder.permanentCalls[0].driveID)
	recorder.mu.Unlock()

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	assert.Equal(t, "ok.txt", adder.events[0].Path)
	adder.mu.Unlock()
}

func TestReconcile_MixedDirections(t *testing.T) {
	rows := []SyncFailureRow{
		makeFailedRow("remote.txt", "download", 2),
		makeFailedRow("local.txt", "upload", 1),
	}
	state := &mockStateReader{failureRows: rows}
	adder := &mockEventAdder{}
	recorder := &mockSyncFailureRecorder{}

	r := testFailureRetrierWith(state, &mockEscalator{}, recorder, adder, newMockInFlightChecker())
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
