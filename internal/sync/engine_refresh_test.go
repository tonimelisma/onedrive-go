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
// projectObservedItems projection tests
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_RemoteOnly(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "r1", Path: "remote.txt", DriveID: driveid.New(testDriveID)},
		{Source: SourceLocal, Path: "local.txt"},
		{Source: SourceRemote, ItemID: "r2", Path: "remote2.txt", DriveID: driveid.New(testDriveID)},
	}

	items := projectObservedItems(slog.Default(), events)
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

	items := projectObservedItems(slog.Default(), events)
	require.Len(t, items, 2)

	assert.Equal(t, driveID, items[0].DriveID)
	assert.Equal(t, "item1", items[0].ItemID)
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

	// observeAndCommitRemoteCurrentState with 0 events (only root, which is skipped).
	events, pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
		testEngineFlow(t, e).buildPrimaryRootObservationPlan(false),
	)
	require.NoError(t, err)
	assert.Empty(t, events, "should return 0 events (root is skipped)")
	require.Nil(t, pendingCursor, "no pending cursor when 0 events")

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

	// observeAndCommitRemoteCurrentState with actual events.
	events, pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
		testEngineFlow(t, e).buildPrimaryRootObservationPlan(false),
	)
	require.NoError(t, err)
	assert.Len(t, events, 1, "should return 1 event (root is skipped)")

	require.NotNil(t, pendingCursor)
	assert.Equal(t, "new-token", pendingCursor.token, "pending cursor should be returned")

	savedToken := readObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Equal(t, "old-token", savedToken,
		"cursor should NOT be committed to DB by observeAndCommitRemoteCurrentState — it is deferred")
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
// projectObservedItems — empty ItemID guard (Item 4)
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_SkipsEmptyItemID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	events := []ChangeEvent{
		{Source: SourceRemote, ItemID: "valid-1", Path: "a.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "", Path: "bad.txt", DriveID: driveID},
		{Source: SourceRemote, ItemID: "valid-2", Path: "b.txt", DriveID: driveID},
	}

	items := projectObservedItems(slog.Default(), events)
	require.Len(t, items, 2, "empty ItemID event should be skipped")
	assert.Equal(t, "valid-1", items[0].ItemID)
	assert.Equal(t, "valid-2", items[1].ItemID)
}

func TestShouldRunFullRemoteRefresh_NoCursor(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

func TestShouldRunFullRemoteRefresh_Overdue(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(1_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-fullRemoteRefreshInterval-time.Minute),
		remoteObservationModeDelta,
	))

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

func TestShouldRunFullRemoteRefresh_WithinCadence(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(2_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
		remoteObservationModeDelta,
	))

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.False(t, shouldRun)
}

func TestFullRemoteRefreshDelay_UsesPersistedTimestamp(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &engineMockClient{})
	clock := newManualClock(time.Unix(3_000, 0))
	installManualClock(e.Engine, clock)
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
		remoteObservationModeDelta,
	))

	delay, err := e.fullRemoteRefreshDelay(t.Context())
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

	events, pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
		testEngineFlow(t, e).buildPrimaryRootObservationPlan(true),
	)
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
	require.NotNil(t, pendingCursor)

	// Cursor should be returned as pending — NOT committed to DB.
	assert.Equal(t, "full-token", pendingCursor.token, "pending cursor should be returned")

	savedToken := readObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Empty(t, savedToken, "cursor should NOT be committed to DB by observeAndCommitRemoteCurrentState — it is deferred")
}

// Validates: R-2.10.4
func TestObserveAndCommitRemoteTruth_RemoteReadDeniedPersistsObservationFindings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, graph.ErrForbidden
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

	events, pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
		testEngineFlow(t, e).buildPrimaryRootObservationPlan(false),
	)
	require.NoError(t, err)
	assert.Empty(t, events)
	assert.Nil(t, pendingCursor)

	issues, err := e.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "/", issues[0].Path)
	assert.Equal(t, IssueRemoteReadDenied, issues[0].IssueType)
	assert.Equal(t, SKPermRemoteRead(""), issues[0].ScopeKey)

	scopes, err := e.baseline.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, scopes, "remote read-denied observation is tracked as an issue row, not a block scope")
}

// ---------------------------------------------------------------------------
// runFullRemoteRefreshAsync tests
// ---------------------------------------------------------------------------

// waitForRefreshDone applies the next refresh result the same way the
// watch loop would and waits for refreshActive to clear.
func waitForRefreshDone(t *testing.T, ctx context.Context, eng *testEngine) {
	t.Helper()

	rt := testWatchRuntime(t, eng)
	require.NotNil(t, rt.refreshResults)

	select {
	case result := <-rt.refreshResults:
		require.NoError(t, rt.applyRemoteRefreshResult(ctx, &result))
	case <-time.After(10 * time.Second):
		require.Fail(t, "refresh result was not delivered within 10s")
	}

	assert.False(t, rt.refreshActive, "refreshActive should be false after applying refresh result")
}

// Validates: R-2.1.6
func TestRunFullRemoteRefreshAsync_NoChanges(t *testing.T) {
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
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	// Full reconciliation always hands observed changes back through the watch
	// dirty scheduler, even when the later planner pass reduces them to a no-op. The
	// important boundary here is "no direct dispatch from the goroutine."
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	select {
	case ta := <-ready:
		require.Failf(t, "no-change remote refresh dispatched work", "unexpected path %s", ta.Action.Path)
	default:
	}

	batch := testWatchRuntime(t, e).dirtyBuf.FlushImmediate()
	require.NotNil(t, batch, "remote refresh should mark dirty work for the watch loop")
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.1.6
func TestRunFullRemoteRefreshAsync_DeltaError(t *testing.T) {
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
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	// Should not panic — error is logged and function returns.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	batch := testWatchRuntime(t, e).dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.True(t, batch.FullRefresh)
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
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	// Call should return immediately — goroutine is blocked in deltaFn.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// refreshActive should be true while delta is blocked.
	assert.True(t, testWatchRuntime(t, e).refreshActive, "refreshActive should be true while goroutine runs")

	// Unblock delta and wait for completion.
	close(unblock)
	waitForRefreshDone(t, t.Context(), e)
}

func TestRunFullRemoteRefreshAsync_SkipsIfRunning(t *testing.T) {
	t.Parallel()

	deltaCalled := false

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalled = true
			require.Fail(t, "deltaFn should not be called when full remote refresh is already running")
			return nil, assert.AnError
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	// Pre-set refreshActive — simulates a full remote refresh already in progress.
	testWatchRuntime(t, e).refreshActive = true

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// deltaFn should not have been invoked.
	assert.False(t, deltaCalled, "deltaFn should not be called when full remote refresh is already running")

	testWatchRuntime(t, e).refreshActive = false
}

func TestWatchPipelineCleanup_KeepsRefreshResultChannelUsable(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)

	pipe, err := rt.initWatchInfra(t.Context(), SyncDownloadOnly, WatchOptions{})
	require.NoError(t, err)
	pipe.cleanup()

	done := make(chan struct{})
	go func() {
		rt.finishFullRemoteRefresh(context.Background(), &remoteRefreshResult{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "watch cleanup left refresh result senders blocked")
	}
}

func TestRunFullRemoteRefreshAsync_FeedsBuffer(t *testing.T) {
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
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	// Baseline is empty — delta returns a new file and the watch loop gets
	// a coarse dirty signal back from the remote refresh.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	batch := testWatchRuntime(t, e).dirtyBuf.FlushImmediate()
	require.NotNil(t, batch, "dirty scheduler should contain a coarse dirty signal from reconciliation")
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.1.2
func TestWatchDirtyScheduling_PathFanoutStillProducesOneCoarseDirtySignal(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	rt.handleWatchLocalChange(&ChangeEvent{
		Path:    "new-name.txt",
		OldPath: "old-name.txt",
	})
	rt.markDirtyFromRemoteBatch(&remoteObservationBatch{
		emitted: []ChangeEvent{
			{Path: "alpha.txt"},
			{OldPath: "beta.txt"},
			{Path: "gamma.txt", OldPath: "delta.txt"},
		},
	})

	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
	assert.Nil(t, rt.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.4
func TestWatchLoop_RefreshTick_RunsPeriodicFullRemoteRefreshThroughResultHandoff(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "refresh.txt", ParentID: "root", DriveID: driveID, Size: 42, QuickXorHash: "qxh"},
				},
				DeltaLink: "watch-refresh-token",
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
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	refreshC := make(chan time.Time, 1)
	rt.refreshCh = refreshC
	done := make(chan error, 1)
	go func() {
		done <- rt.runWatchLoop(ctx, &watchPipeline{
			runtime: rt,
			bl:      bl,
			mode:    SyncBidirectional,
		})
	}()

	refreshC <- time.Now()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventRemoteRefreshApplied
	}, "watch loop applied periodic full remote refresh result")

	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)

	select {
	case ta := <-ready:
		require.Failf(t, "periodic full remote refresh dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	savedToken := readObservationCursorForTest(t, eng.baseline, t.Context(), driveID.String())
	assert.Equal(t, "watch-refresh-token", savedToken)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "watch loop did not stop after cancellation")
	}
}

func TestRunFullRemoteRefreshAsync_ShutdownAfterCommit(t *testing.T) {
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
	// remote_refresh_committed event at the exact point between durable
	// refresh apply and the ctx.Err() check.
	ctx, cancel := context.WithCancel(t.Context())
	attachDebugEventRecorderWithHook(e, func(event engineDebugEvent) {
		if event.Type == engineDebugEventRemoteRefreshCommitted {
			cancel()
		}
	})

	bl, err := e.baseline.Load(t.Context())
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).dirtyBuf = NewDirtyBuffer(e.logger)

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, ctx, e)

	// Verify observations WERE committed to SQLite — proving we took
	// the post-commit shutdown path, not the commit-failed path.
	// CommitObservation saves the primary observation cursor for this drive DB.
	savedToken := readObservationCursorForTest(t, e.baseline, t.Context(), driveID.String())
	assert.Equal(t, "shutdown-tok", savedToken,
		"delta token should be saved — CommitObservation must have succeeded")

	batch := testWatchRuntime(t, e).dirtyBuf.FlushImmediate()
	require.NotNil(t, batch, "shutdown-aware early exit should request a fresh replan")
	assert.True(t, batch.FullRefresh)
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
			require.Fail(t, "deltaFn should not be called when full remote refresh is already running")
			return nil, assert.AnError
		},
	}

	e, _ := newTestEngineWithLogger(t, mock, logger)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)

	// Pre-set refreshActive — simulates a full remote refresh already in progress.
	testWatchRuntime(t, e).refreshActive = true

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// The skip message should appear in the Info-level log buffer.
	// If it were still at Debug level, the Info-level handler would exclude it.
	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "full remote refresh skipped",
		"skip message should be logged at Info level (not Debug)")

	testWatchRuntime(t, e).refreshActive = false
}

func TestRunFullRemoteRefreshAsync_DurationInCompletionLog(t *testing.T) {
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

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "periodic full remote refresh complete",
		"should have completion message")
	assert.Contains(t, logOutput, "duration=",
		"completion log must include duration field")
}

func TestRunFullRemoteRefreshAsync_DurationInNoChangesLog(t *testing.T) {
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

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "no changes",
		"should have no-changes completion message")
	assert.Contains(t, logOutput, "duration=",
		"no-changes completion log must also include duration field")
}
