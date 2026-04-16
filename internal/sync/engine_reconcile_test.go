package sync

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// changeEventsToObservedItems converter tests
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_RemoteOnly(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "r1", Path: "remote.txt", DriveID: driveid.New(testDriveID)},
		{Source: SourceLocal, Path: "local.txt"},
		{Source: SourceRemote, ItemID: "r2", Path: "remote2.txt", DriveID: driveid.New(testDriveID)},
	}

	items := changeEventsToObservedItems(slog.Default(), events)
	assert.Len(t, items, 2, "should only include remote events")
	assert.Equal(t, "r1", items[0].ItemID)
	assert.Equal(t, "r2", items[1].ItemID)
}

func TestChangeEventsToObservedItems_MapsAllFields(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	events := []ChangeEvent{
		{
			Source:    SourceRemote,
			ItemID:    "item1",
			ParentID:  "parent1",
			DriveID:   driveID,
			Path:      "docs/file.txt",
			ItemType:  ItemTypeFile,
			Hash:      "qxh1",
			Size:      1024,
			Mtime:     123456789,
			ETag:      "etag1",
			IsDeleted: false,
		},
		{
			Source:    SourceRemote,
			ItemID:    "item2",
			DriveID:   driveID,
			Path:      "docs/folder",
			ItemType:  ItemTypeFolder,
			IsDeleted: true,
		},
	}

	items := changeEventsToObservedItems(slog.Default(), events)
	require.Len(t, items, 2)

	assert.Equal(t, driveID, items[0].DriveID)
	assert.Equal(t, "item1", items[0].ItemID)
	assert.Equal(t, "parent1", items[0].ParentID)
	assert.Equal(t, "docs/file.txt", items[0].Path)
	assert.Equal(t, ItemTypeFile, items[0].ItemType)
	assert.Equal(t, "qxh1", items[0].Hash)
	assert.Equal(t, int64(1024), items[0].Size)
	assert.Equal(t, int64(123456789), items[0].Mtime)
	assert.Equal(t, "etag1", items[0].ETag)
	assert.False(t, items[0].IsDeleted)

	assert.Equal(t, ItemTypeFolder, items[1].ItemType)
	assert.True(t, items[1].IsDeleted)
}

// ---------------------------------------------------------------------------
// Zero-event guard tests (Step 1)
// ---------------------------------------------------------------------------

// Validates: R-6.7.19
func TestObserveAndCommitRemote_ZeroEvents_NoTokenAdvance(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "new-token-should-not-be-saved",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	// Seed a known delta token.
	saveObservationCursorForTest(t, e.baseline, ctx, driveID.String(), "old-token")

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemote with 0 events (only root, which is skipped).
	events, pendingToken, err := testEngineFlow(t, e).observeAndCommitRemote(ctx, bl)
	require.NoError(t, err)
	assert.Empty(t, events, "should return 0 events (root is skipped)")
	assert.Empty(t, pendingToken, "no pending token when 0 events")

	// Token should NOT have been advanced.
	savedToken := readObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Equal(t, "old-token", savedToken, "token should not advance when 0 events returned")
}

// Validates: R-2.15.1
func TestObserveAndCommitRemote_WithEvents_TokenDeferred(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "hello.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "new-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	// Seed a known delta token.
	saveObservationCursorForTest(t, e.baseline, ctx, driveID.String(), "old-token")

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemote with actual events.
	events, pendingToken, err := testEngineFlow(t, e).observeAndCommitRemote(ctx, bl)
	require.NoError(t, err)
	assert.Len(t, events, 1, "should return 1 event (root is skipped)")

	// Token should be returned as pending — NOT yet committed to DB.
	assert.Equal(t, "new-token", pendingToken, "pending token should be returned")

	savedToken := readObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Equal(t, "old-token", savedToken,
		"token should NOT be committed to DB by observeAndCommitRemote — it is deferred")
}

// ---------------------------------------------------------------------------
// Full reconciliation tests (Step 2)
// ---------------------------------------------------------------------------

func TestFindOrphans_DetectsDeletedItems(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
		{Path: "c.txt", DriveID: driveID, ItemID: "id-c", ItemType: ItemTypeFile},
	})

	// Seen set has 2 of 3 items — id-b is missing (orphan).
	seen := map[string]struct{}{
		"id-a": {},
		"id-c": {},
	}

	orphans := findBaselineOrphans(bl, seen, driveID, "")
	require.Len(t, orphans, 1, "should detect 1 orphan")
	assert.Equal(t, "b.txt", orphans[0].Path)
	assert.Equal(t, "id-b", orphans[0].ItemID)
	assert.Equal(t, ChangeDelete, orphans[0].Type)
	assert.Equal(t, SourceRemote, orphans[0].Source)
	assert.True(t, orphans[0].IsDeleted)
}

func TestFindOrphans_NoOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
	})

	// All baseline items are in the seen set.
	seen := map[string]struct{}{
		"id-a": {},
		"id-b": {},
	}

	orphans := findBaselineOrphans(bl, seen, driveID, "")
	assert.Empty(t, orphans, "should find no orphans when all items are in seen set")
}

func TestFindOrphans_IgnoresOtherDrives(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	otherDrive := driveid.New("0000000000000002")

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "other.txt", DriveID: otherDrive, ItemID: "id-other", ItemType: ItemTypeFile},
	})

	// Empty seen set — only driveID's items should be orphaned.
	seen := map[string]struct{}{}

	orphans := findBaselineOrphans(bl, seen, driveID, "")
	require.Len(t, orphans, 1, "should only detect orphans for the specified drive")
	assert.Equal(t, "a.txt", orphans[0].Path)
}

func TestFindOrphans_WithPathPrefix(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "SharedFolder/a.txt", DriveID: driveID, ItemID: "id-a", ItemType: ItemTypeFile},
		{Path: "SharedFolder/sub/b.txt", DriveID: driveID, ItemID: "id-b", ItemType: ItemTypeFile},
		{Path: "OtherFolder/c.txt", DriveID: driveID, ItemID: "id-c", ItemType: ItemTypeFile},
	})

	// Only id-a is in the seen set. id-b is an orphan under SharedFolder.
	// id-c is outside the prefix — should NOT be detected.
	seen := map[string]struct{}{
		"id-a": {},
	}

	orphans := findBaselineOrphans(bl, seen, driveID, "SharedFolder")
	require.Len(t, orphans, 1, "should detect only orphans under the prefix")
	assert.Equal(t, "SharedFolder/sub/b.txt", orphans[0].Path)
}

func TestObserveRemoteFull_IntegratesOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	// Full delta returns 2 items (root + file1). Baseline has file1 + file2.
	// file2 should be detected as an orphan.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	// Seed baseline with 2 files (file1 + file2).
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})
	bl.Put(&BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: ItemTypeFile})

	events, token, err := testEngineFlow(t, e).observeRemoteFull(ctx, bl)
	require.NoError(t, err)
	assert.Equal(t, "full-token", token)

	// Should have 1 modify (file1 exists in baseline) + 1 orphan delete (file2).
	var modifies, deletes int
	for _, ev := range events {
		switch ev.Type {
		case ChangeModify:
			modifies++
		case ChangeDelete:
			deletes++
			assert.Equal(t, "file2.txt", ev.Path, "orphan should be file2.txt")
			assert.Equal(t, "f2", ev.ItemID)
			assert.True(t, ev.IsDeleted)
		case ChangeCreate, ChangeMove:
			// Not expected in this test.
		}
	}

	assert.Equal(t, 1, modifies, "should have 1 modify event (file1 exists in baseline)")
	assert.Equal(t, 1, deletes, "should have 1 orphan delete event")
}

// ---------------------------------------------------------------------------
// changeEventsToObservedItems — empty ItemID guard (Item 4)
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_SkipsEmptyItemID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "valid-1", Path: "a.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "", Path: "bad.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "valid-2", Path: "b.txt", DriveID: driveID},
	}

	items := changeEventsToObservedItems(slog.Default(), events)
	require.Len(t, items, 2, "empty ItemID event should be skipped")
	assert.Equal(t, "valid-1", items[0].ItemID)
	assert.Equal(t, "valid-2", items[1].ItemID)
}

func TestShouldRunFullRemoteReconcile_NoCursor(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})

	shouldRun, err := e.shouldRunFullRemoteReconcile(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

func TestShouldRunFullRemoteReconcile_Overdue(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(1_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteReconcile(
		t.Context(),
		e.driveID,
		clock.Now().Add(-fullRemoteReconcileInterval-time.Minute),
	))

	shouldRun, err := e.shouldRunFullRemoteReconcile(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

func TestShouldRunFullRemoteReconcile_WithinCadence(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(2_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteReconcile(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
	))

	shouldRun, err := e.shouldRunFullRemoteReconcile(t.Context(), false)
	require.NoError(t, err)
	assert.False(t, shouldRun)
}

func TestFullRemoteReconcileDelay_UsesPersistedTimestamp(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(3_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.MarkFullRemoteReconcile(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
	))

	delay, err := e.fullRemoteReconcileDelay(t.Context())
	require.NoError(t, err)
	assert.Equal(t, time.Hour, delay)
}

// observeAndCommitRemoteFull tests (Item 6)
// ---------------------------------------------------------------------------

func TestObserveAndCommitRemoteFull(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	// Seed baseline with file1 + file2 (file2 will become an orphan).
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})
	bl.Put(&BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: ItemTypeFile})

	events, pendingToken, err := testEngineFlow(t, e).observeAndCommitRemoteFull(ctx, bl)
	require.NoError(t, err)

	// Should have modify (file1) + orphan delete (file2).
	var modifies, deletes int
	for _, ev := range events {
		switch ev.Type {
		case ChangeModify:
			modifies++
		case ChangeDelete:
			deletes++
			assert.Equal(t, "file2.txt", ev.Path)
			assert.True(t, ev.IsDeleted)
		case ChangeCreate, ChangeMove:
			// not expected
		}
	}

	assert.Equal(t, 1, modifies)
	assert.Equal(t, 1, deletes)

	// Token should be returned as pending — NOT committed to DB.
	assert.Equal(t, "full-token", pendingToken, "pending token should be returned")

	savedToken := readObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Empty(t, savedToken, "token should NOT be committed to DB by observeAndCommitRemoteFull — it is deferred")
}

// ---------------------------------------------------------------------------
// runFullReconciliationAsync tests
// ---------------------------------------------------------------------------

// waitForReconcileDone applies the next reconcile result the same way the
// watch loop would and waits for reconcileActive to clear.
func waitForReconcileDone(t *testing.T, eng *testEngine) {
	t.Helper()

	rt := testWatchRuntime(t, eng)
	require.NotNil(t, rt.reconcileResults)

	select {
	case result := <-rt.reconcileResults:
		rt.applyReconcileResult(result)
	case <-time.After(10 * time.Second):
		require.Fail(t, "reconcile result was not delivered within 10s")
	}

	assert.False(t, rt.reconcileActive, "reconcileActive should be false after applying reconcile result")
}

// Validates: R-2.1.6
func TestRunFullReconciliationAsync_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	// Seed baseline matching delta exactly — no orphans.
	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	bl.Put(&BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: ItemTypeFile})

	ready := setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Full reconciliation always hands observed changes back through the watch
	// buffer, even when the later planner pass reduces them to a no-op. The
	// important boundary here is "no direct dispatch from the goroutine."
	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	select {
	case ta := <-ready:
		require.Failf(t, "no-change reconciliation dispatched work", "unexpected path %s", ta.Action.Path)
	default:
	}

	batch := testWatchRuntime(t, e).buf.FlushImmediate()
	require.Len(t, batch, 1, "reconciliation should hand observed state back through the watch buffer")
	assert.Equal(t, "file1.txt", batch[0].Path)
}

// Validates: R-2.1.6
func TestRunFullReconciliationAsync_DeltaError(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, errors.New("delta unavailable")
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Should not panic — error is logged and function returns.
	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	// Buffer should be empty — no events were produced.
	batch := testWatchRuntime(t, e).buf.FlushImmediate()
	assert.Empty(t, batch)
}

func TestRunFullReconciliationAsync_NonBlocking(t *testing.T) {
	t.Parallel()

	// deltaFn blocks on a channel — lets us verify the call returns
	// before delta completes.
	unblock := make(chan struct{})

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			<-unblock
			return &graph.DeltaPage{DeltaLink: "tok"}, nil
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Call should return immediately — goroutine is blocked in deltaFn.
	runFullReconciliationAsyncForTest(t, e, ctx, bl)

	// reconcileActive should be true while delta is blocked.
	assert.True(t, testWatchRuntime(t, e).reconcileActive, "reconcileActive should be true while goroutine runs")

	// Unblock delta and wait for completion.
	close(unblock)
	waitForReconcileDone(t, e)
}

func TestRunFullReconciliationAsync_SkipsIfRunning(t *testing.T) {
	t.Parallel()

	deltaCalled := false

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalled = true
			require.Fail(t, "deltaFn should not be called when reconciliation is already running")
			return nil, assert.AnError
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Pre-set reconcileActive — simulates a reconciliation already in progress.
	testWatchRuntime(t, e).reconcileActive = true

	runFullReconciliationAsyncForTest(t, e, ctx, bl)

	// deltaFn should not have been invoked.
	assert.False(t, deltaCalled, "deltaFn should not be called when reconciliation is already running")

	testWatchRuntime(t, e).reconcileActive = false
}

func TestRunFullReconciliationAsync_FeedsBuffer(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "newfile.txt", ParentID: "root", DriveID: driveID, Size: 42, QuickXorHash: "qxh"},
				},
				DeltaLink: "tok",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Baseline is empty — delta returns a new file → orphan detection
	// produces a download event that gets fed into the buffer.
	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	batch := testWatchRuntime(t, e).buf.FlushImmediate()
	assert.NotEmpty(t, batch, "buffer should contain events from reconciliation")
}

// Validates: R-2.8.4
func TestWatchLoop_ReconcileTick_RunsPeriodicFullReconciliationThroughResultHandoff(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "reconcile.txt", ParentID: "root", DriveID: driveID, Size: 42, QuickXorHash: "qxh"},
				},
				DeltaLink: "watch-reconcile-token",
			}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	ready := setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.buf = NewBuffer(eng.logger)

	reconcileC := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		done <- rt.runWatchLoop(ctx, &watchPipeline{
			runtime:          rt,
			bl:               bl,
			safety:           DefaultSafetyConfig(),
			mode:             SyncBidirectional,
			reconcileC:       reconcileC,
			reconcileResults: rt.reconcileResults,
		})
	}()

	reconcileC <- time.Now()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventReconcileApplied
	}, "watch loop applied periodic reconciliation result")

	batch := rt.buf.FlushImmediate()

	require.Len(t, batch, 1)
	assert.Equal(t, "reconcile.txt", batch[0].Path)

	select {
	case ta := <-ready:
		require.Failf(t, "periodic reconciliation dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	savedToken := readObservationCursorForTest(t, eng.baseline, t.Context(), driveID.String())
	assert.Equal(t, "watch-reconcile-token", savedToken)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "watch loop did not stop after cancellation")
	}
}

func TestRunFullReconciliationAsync_ShutdownAfterCommit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "newfile.txt", ParentID: "root", DriveID: driveID, Size: 42, QuickXorHash: "qxh"},
				},
				DeltaLink: "shutdown-tok",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := testLogger(t)

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	// Context with manual cancel — cancel is triggered by the
	// afterReconcileCommit hook at the exact point between
	// CommitObservation succeeding and the ctx.Err() check.
	ctx, cancel := context.WithCancel(t.Context())

	bl, err := e.baseline.Load(t.Context())
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Hook: cancel context immediately after CommitObservation succeeds.
	// This guarantees we test the exact shutdown-after-commit code path,
	// not the commit-failed path or the normal completion path.
	testWatchRuntime(t, e).afterReconcileCommit = func() {
		cancel()
	}

	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	// Verify observations WERE committed to SQLite — proving we took
	// the post-commit shutdown path, not the commit-failed path.
	// CommitObservation saves the primary observation cursor for this drive DB.
	savedToken := readObservationCursorForTest(t, e.baseline, t.Context(), driveID.String())
	assert.Equal(t, "shutdown-tok", savedToken,
		"delta token should be saved — CommitObservation must have succeeded")

	// Buffer should be empty — events were committed to SQLite but
	// not fed to the buffer because shutdown was detected after commit.
	batch := testWatchRuntime(t, e).buf.FlushImmediate()
	assert.Empty(t, batch, "buffer should be empty after shutdown-aware early exit")
}

func TestRunFullReconciliationAsync_SkipLogPromotedToInfo(t *testing.T) {
	t.Parallel()

	// Capture logs at Info level — if the skip message were still Debug,
	// it would NOT appear in the output.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			require.Fail(t, "deltaFn should not be called when reconciliation is already running")
			return nil, assert.AnError
		},
	}

	e, _ := newTestEngineWithLogger(t, mock, logger)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	// Pre-set reconcileActive — simulates a reconciliation already in progress.
	testWatchRuntime(t, e).reconcileActive = true

	runFullReconciliationAsyncForTest(t, e, ctx, bl)

	// The skip message should appear in the Info-level log buffer.
	// If it were still at Debug level, the Info-level handler would exclude it.
	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "full reconciliation skipped",
		"skip message should be logged at Info level (not Debug)")

	testWatchRuntime(t, e).reconcileActive = false
}

func TestRunFullReconciliationAsync_DurationInCompletionLog(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "newfile.txt", ParentID: "root", DriveID: driveID, Size: 42, QuickXorHash: "qxh"},
				},
				DeltaLink: "dur-tok",
			}, nil
		},
	}

	// Capture logs to verify duration field in completion message.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "periodic full reconciliation complete",
		"should have completion message")
	assert.Contains(t, logOutput, "duration=",
		"completion log must include duration field")
}

func TestRunFullReconciliationAsync_DurationInNoChangesLog(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	// Return only root item — FullDelta produces 0 events (root is skipped),
	// so the function takes the "no changes" path.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
				},
				DeltaLink: "no-change-tok",
			}, nil
		},
	}

	// Capture logs to verify duration in the "no changes" completion path.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()

	rawEngine, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	e := newFlowBackedTestEngine(rawEngine)
	defer e.Close(t.Context())

	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).buf = NewBuffer(e.logger)

	runFullReconciliationAsyncForTest(t, e, ctx, bl)
	waitForReconcileDone(t, e)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "no changes",
		"should have no-changes completion message")
	assert.Contains(t, logOutput, "duration=",
		"no-changes completion log must also include duration field")
}
