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
	failedRows         []RemoteStateRow
	failedErr          error
	earliestRetry      time.Time
	earliestRetryErr   error
	listFailedCalls    int
	earliestRetryCalls int

	localIssueRows        []LocalIssueRow
	localIssueErr         error
	earliestLocalRetry    time.Time
	earliestLocalRetryErr error
}

func (m *mockStateReader) ListUnreconciled(_ context.Context) ([]RemoteStateRow, error) {
	return nil, nil
}

func (m *mockStateReader) ListFailedForRetry(_ context.Context, _ time.Time) ([]RemoteStateRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.listFailedCalls++

	return m.failedRows, m.failedErr
}

func (m *mockStateReader) EarliestRetryAt(_ context.Context, _ time.Time) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.earliestRetryCalls++

	return m.earliestRetry, m.earliestRetryErr
}

func (m *mockStateReader) ListLocalIssuesForRetry(_ context.Context, _ time.Time) ([]LocalIssueRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.localIssueRows, m.localIssueErr
}

func (m *mockStateReader) EarliestLocalIssueRetryAt(_ context.Context, _ time.Time) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.earliestLocalRetry, m.earliestLocalRetryErr
}

func (m *mockStateReader) FailureCount(_ context.Context) (int, error)            { return 0, nil }
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

// mockLocalIssueEscalator implements LocalIssueEscalator for failure retrier tests.
type mockLocalIssueEscalator struct {
	mu    stdsync.Mutex
	paths []string
	err   error
}

func (m *mockLocalIssueEscalator) MarkLocalIssuePermanent(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.paths = append(m.paths, path)

	return m.err
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

func testFailureRetrierWith(state *mockStateReader, esc *mockEscalator, localIss *mockLocalIssueEscalator, adder *mockEventAdder, checker *mockInFlightChecker) *FailureRetrier {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Convert nil *mockLocalIssueEscalator to nil LocalIssueEscalator interface.
	var lis LocalIssueEscalator
	if localIss != nil {
		lis = localIss
	}

	return NewFailureRetrier(DefaultFailureRetrierConfig(), state, esc, lis, adder, checker, logger)
}

func makeFailedRow(path, status string, failureCount int) RemoteStateRow {
	driveID := driveid.New("00000000000d0001")

	return RemoteStateRow{
		DriveID:      driveID,
		ItemID:       "item-" + path,
		Path:         path,
		ParentID:     "parent1",
		ItemType:     "file",
		Hash:         "hash-" + path,
		Size:         100,
		Mtime:        1000,
		ETag:         "etag-" + path,
		SyncStatus:   status,
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
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, 2),
		makeFailedRow("b.txt", statusDeleteFailed, 3),
	}
	state := &mockStateReader{failedRows: rows}
	esc := &mockEscalator{}
	adder := &mockEventAdder{}
	checker := newMockInFlightChecker()

	r := testFailureRetrier(state, esc, adder, checker)
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 2)

	// First: download_failed → ChangeModify
	assert.Equal(t, "a.txt", adder.events[0].Path)
	assert.Equal(t, ChangeModify, adder.events[0].Type)
	assert.Equal(t, SourceRemote, adder.events[0].Source)

	// Second: delete_failed → ChangeDelete
	assert.Equal(t, "b.txt", adder.events[1].Path)
	assert.Equal(t, ChangeDelete, adder.events[1].Type)
}

func TestReconcile_SkipInFlight(t *testing.T) {
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, 2),
		makeFailedRow("b.txt", statusDownloadFailed, 3),
	}
	state := &mockStateReader{failedRows: rows}
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
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, defaultEscalationThreshold),
		makeFailedRow("b.txt", statusDownloadFailed, defaultEscalationThreshold+5),
		makeFailedRow("c.txt", statusDownloadFailed, 2), // below threshold
	}
	state := &mockStateReader{failedRows: rows}
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
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, defaultEscalationThreshold),
	}
	state := &mockStateReader{failedRows: rows}
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

func TestSynthesizeEvent_DeleteStatuses(t *testing.T) {
	r := testFailureRetrier(&mockStateReader{}, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())

	tests := []struct {
		name     string
		status   string
		wantType ChangeType
	}{
		{"download_failed", statusDownloadFailed, ChangeModify},
		{"pending_download", statusPendingDownload, ChangeModify},
		{"synced", statusSynced, ChangeModify},
		{"delete_failed", statusDeleteFailed, ChangeDelete},
		{"pending_delete", statusPendingDelete, ChangeDelete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := makeFailedRow("test.txt", tt.status, 1)
			ev := r.synthesizeEvent(&row)

			assert.Equal(t, tt.wantType, ev.Type)
			assert.Equal(t, SourceRemote, ev.Source)
			assert.Equal(t, "test.txt", ev.Path)
			assert.Equal(t, row.ItemID, ev.ItemID)
			assert.Equal(t, row.DriveID, ev.DriveID)
			assert.Equal(t, row.Hash, ev.Hash)
			assert.Equal(t, row.Size, ev.Size)
			assert.Equal(t, row.Mtime, ev.Mtime)
			assert.Equal(t, row.ETag, ev.ETag)
			assert.Equal(t, tt.wantType == ChangeDelete, ev.IsDeleted)
		})
	}
}

func TestSynthesizeEvent_FolderItemType(t *testing.T) {
	r := testFailureRetrier(&mockStateReader{}, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())

	row := makeFailedRow("dir", statusDownloadFailed, 1)
	row.ItemType = "folder"

	ev := r.synthesizeEvent(&row)
	assert.Equal(t, ItemTypeFolder, ev.ItemType)
}

func TestSynthesizeEvent_InvalidItemType(t *testing.T) {
	r := testFailureRetrier(&mockStateReader{}, &mockEscalator{}, &mockEventAdder{}, newMockInFlightChecker())

	row := makeFailedRow("bad.txt", statusDownloadFailed, 1)
	row.ItemType = "bogus"

	ev := r.synthesizeEvent(&row)
	assert.Nil(t, ev, "invalid item type should return nil")
}

func TestReconcile_SkipsInvalidItemType(t *testing.T) {
	rows := []RemoteStateRow{
		makeFailedRow("good.txt", statusDownloadFailed, 2),
		makeFailedRow("bad.txt", statusDownloadFailed, 2),
	}
	rows[1].ItemType = "bogus" // invalid item type

	state := &mockStateReader{failedRows: rows}
	adder := &mockEventAdder{}
	r := testFailureRetrier(state, &mockEscalator{}, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	// Only good.txt dispatched; bad.txt skipped due to invalid item type.
	require.Len(t, adder.events, 1)
	assert.Equal(t, "good.txt", adder.events[0].Path)
}

func TestReconcile_NoRows(t *testing.T) {
	state := &mockStateReader{failedRows: nil}
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
	state := &mockStateReader{failedErr: errors.New("query error")}
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
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, 2),
	}
	state := &mockStateReader{failedRows: rows}
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
	rows := []RemoteStateRow{
		makeFailedRow("a.txt", statusDownloadFailed, 3),
		makeFailedRow("b.txt", statusDownloadFailed, 2), // below threshold
	}
	state := &mockStateReader{failedRows: rows}
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
// Local issue retry tests
// ---------------------------------------------------------------------------

func makeLocalIssueRow(path string, failureCount int) LocalIssueRow {
	return LocalIssueRow{
		Path:         path,
		IssueType:    "upload_failed",
		SyncStatus:   "upload_failed",
		FailureCount: failureCount,
		LastError:    "some error",
	}
}

func TestReconcile_DispatchLocalIssues(t *testing.T) {
	localRows := []LocalIssueRow{
		makeLocalIssueRow("a.txt", 2),
		makeLocalIssueRow("b.txt", 3),
	}
	state := &mockStateReader{localIssueRows: localRows}
	adder := &mockEventAdder{}
	localIss := &mockLocalIssueEscalator{}

	r := testFailureRetrierWith(state, &mockEscalator{}, localIss, adder, newMockInFlightChecker())
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

func TestReconcile_EscalateLocalIssue(t *testing.T) {
	localRows := []LocalIssueRow{
		makeLocalIssueRow("bad.txt", defaultEscalationThreshold),
		makeLocalIssueRow("ok.txt", 2),
	}
	state := &mockStateReader{localIssueRows: localRows}
	adder := &mockEventAdder{}
	localIss := &mockLocalIssueEscalator{}

	r := testFailureRetrierWith(state, &mockEscalator{}, localIss, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	localIss.mu.Lock()
	require.Len(t, localIss.paths, 1)
	assert.Equal(t, "bad.txt", localIss.paths[0])
	localIss.mu.Unlock()

	adder.mu.Lock()
	require.Len(t, adder.events, 1)
	assert.Equal(t, "ok.txt", adder.events[0].Path)
	adder.mu.Unlock()
}

func TestReconcile_MixedRemoteAndLocal(t *testing.T) {
	remoteRows := []RemoteStateRow{
		makeFailedRow("remote.txt", statusDownloadFailed, 2),
	}
	localRows := []LocalIssueRow{
		makeLocalIssueRow("local.txt", 1),
	}
	state := &mockStateReader{failedRows: remoteRows, localIssueRows: localRows}
	adder := &mockEventAdder{}
	localIss := &mockLocalIssueEscalator{}

	r := testFailureRetrierWith(state, &mockEscalator{}, localIss, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	r.reconcile(context.Background())

	adder.mu.Lock()
	defer adder.mu.Unlock()

	require.Len(t, adder.events, 2)

	// First event from remote (SourceRemote), second from local (SourceLocal).
	assert.Equal(t, SourceRemote, adder.events[0].Source)
	assert.Equal(t, "remote.txt", adder.events[0].Path)
	assert.Equal(t, SourceLocal, adder.events[1].Source)
	assert.Equal(t, "local.txt", adder.events[1].Path)
}

func TestArmTimer_ConsidersLocalIssues(t *testing.T) {
	now := time.Unix(1000, 0)
	remoteFuture := now.Add(5 * time.Minute)
	localFuture := now.Add(2 * time.Minute) // earlier than remote

	state := &mockStateReader{
		earliestRetry:      remoteFuture,
		earliestLocalRetry: localFuture,
	}
	adder := &mockEventAdder{}
	localIss := &mockLocalIssueEscalator{}

	r := testFailureRetrierWith(state, &mockEscalator{}, localIss, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	r.armTimer(context.Background(), now)

	r.mu.Lock()
	require.NotNil(t, r.timer, "timer should be armed")
	r.timer.Stop()
	r.mu.Unlock()
}

func TestArmTimer_OnlyLocalIssues(t *testing.T) {
	now := time.Unix(1000, 0)
	localFuture := now.Add(30 * time.Second)

	state := &mockStateReader{
		earliestRetry:      time.Time{}, // no remote retries
		earliestLocalRetry: localFuture,
	}
	adder := &mockEventAdder{}
	localIss := &mockLocalIssueEscalator{}

	r := testFailureRetrierWith(state, &mockEscalator{}, localIss, adder, newMockInFlightChecker())
	r.nowFunc = func() time.Time { return now }

	r.armTimer(context.Background(), now)

	r.mu.Lock()
	require.NotNil(t, r.timer, "timer should be armed for local issues even with no remote retries")
	r.timer.Stop()
	r.mu.Unlock()
}
