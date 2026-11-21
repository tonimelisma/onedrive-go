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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// ---------------------------------------------------------------------------
// ProjectRemoteObservations projection tests
// ---------------------------------------------------------------------------

func TestChangeEventsToObservedItems_MapsAllFields(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	events := []syncdomain.RemoteItem{
		{ObservedItem: syncdomain.ObservedItem{
			ItemID:    "item1",
			ParentID:  "parent1",
			DriveID:   driveID,
			Path:      "docs/file.txt",
			ItemType:  syncdomain.ItemTypeFile,
			Hash:      "qxh1",
			Size:      1024,
			Mtime:     123456789,
			ETag:      "etag1",
			IsDeleted: false,
		}},
		{ObservedItem: syncdomain.ObservedItem{
			ItemID:    "item2",
			DriveID:   driveID,
			Path:      "docs/folder",
			ItemType:  syncdomain.ItemTypeFolder,
			IsDeleted: true,
		}},
	}

	items := syncobserve.ProjectRemoteObservations(slog.Default(), events).Observed
	require.Len(t, items, 2)

	assert.Equal(t, driveID, items[0].DriveID)
	assert.Equal(t, "item1", items[0].ItemID)
	assert.Equal(t, "docs/file.txt", items[0].Path)
	assert.Equal(t, syncdomain.ItemTypeFile, items[0].ItemType)
	assert.Equal(t, "qxh1", items[0].Hash)
	assert.Equal(t, int64(1024), items[0].Size)
	assert.Equal(t, int64(123456789), items[0].Mtime)
	assert.Equal(t, "etag1", items[0].ETag)
	assert.False(t, items[0].IsDeleted)

	assert.Equal(t, syncdomain.ItemTypeFolder, items[1].ItemType)
	assert.True(t, items[1].IsDeleted)
}

// ---------------------------------------------------------------------------
// Zero-event guard tests (Step 1)
// ---------------------------------------------------------------------------

// Validates: R-6.7.19
func TestObserveAndCommitRemote_ZeroEvents_NoTokenAdvance(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "new-token-should-not-be-saved",
			}, nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := syncfixture.TestLogger(t)

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
	syncstoretest.SaveObservationCursorForTest(t, e.baseline, ctx, driveID.String(), "old-token")

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemoteCurrentState with no change (only root, which is skipped).
	pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
	)
	require.NoError(t, err)
	require.Nil(t, pendingCursor, "no pending cursor when the window holds no change")

	// Token should NOT have been advanced.
	savedToken := syncstoretest.ReadObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Equal(t, "old-token", savedToken, "token should not advance when 0 events returned")
}

// Validates: R-2.15.1
func TestObserveAndCommitRemote_WithEvents_TokenDeferred(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	logger := syncfixture.TestLogger(t)

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
	syncstoretest.SaveObservationCursorForTest(t, e.baseline, ctx, driveID.String(), "old-token")

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	// observeAndCommitRemoteCurrentState with one change (root is skipped).
	pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
	)
	require.NoError(t, err)
	remoteRows, err := e.baseline.ListRemoteState(ctx)
	require.NoError(t, err)
	assert.Len(t, remoteRows, 1, "the one changed item is committed (root is skipped)")

	require.NotNil(t, pendingCursor)
	assert.Equal(t, "new-token", pendingCursor.CursorToken, "pending cursor should be returned")

	savedToken := syncstoretest.ReadObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Equal(t, "old-token", savedToken,
		"cursor should NOT be committed to DB by observeAndCommitRemoteCurrentState — it is deferred")
}

// ---------------------------------------------------------------------------
// Full reconciliation tests (Step 2)
// ---------------------------------------------------------------------------

// Validates: R-2.1.6, R-2.8.4
func TestFindOrphans_DetectsDeletedItems(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	bl := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: syncdomain.ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: syncdomain.ItemTypeFile},
		{Path: "c.txt", DriveID: driveID, ItemID: "id-c", ItemType: syncdomain.ItemTypeFile},
	})

	// Seen set has 2 of 3 items — id-b is missing (orphan).
	seen := map[string]struct{}{
		"id-a": {},
		"id-c": {},
	}

	orphans := syncstore.FindBaselineOrphans(bl, seen, driveID, "")
	require.Len(t, orphans, 1, "should detect 1 orphan")
	assert.Equal(t, "b.txt", orphans[0].Path)
	assert.Equal(t, "id-b", orphans[0].ItemID)
	assert.True(t, orphans[0].IsDeleted)
	assert.Empty(t, orphans[0].OldPath)
}

// Validates: R-2.1.6
func TestFindOrphans_NoOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	bl := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: syncdomain.ItemTypeFile},
		{Path: "b.txt", DriveID: driveID, ItemID: "id-b", ItemType: syncdomain.ItemTypeFile},
	})

	// All baseline items are in the seen set.
	seen := map[string]struct{}{
		"id-a": {},
		"id-b": {},
	}

	orphans := syncstore.FindBaselineOrphans(bl, seen, driveID, "")
	assert.Empty(t, orphans, "should find no orphans when all items are in seen set")
}

// Validates: R-2.1.6
func TestFindOrphans_IgnoresOtherDrives(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	otherDrive := driveid.New("0000000000000002")

	bl := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "a.txt", DriveID: driveID, ItemID: "id-a", ItemType: syncdomain.ItemTypeFile},
		{Path: "other.txt", DriveID: otherDrive, ItemID: "id-other", ItemType: syncdomain.ItemTypeFile},
	})

	// Empty seen set — only driveID's items should be orphaned.
	seen := map[string]struct{}{}

	orphans := syncstore.FindBaselineOrphans(bl, seen, driveID, "")
	require.Len(t, orphans, 1, "should only detect orphans for the specified drive")
	assert.Equal(t, "a.txt", orphans[0].Path)
}

func TestFindOrphans_WithPathPrefix(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	bl := syncfixture.NewBaselineForTest([]*syncdomain.BaselineEntry{
		{Path: "SharedFolder/a.txt", DriveID: driveID, ItemID: "id-a", ItemType: syncdomain.ItemTypeFile},
		{Path: "SharedFolder/sub/b.txt", DriveID: driveID, ItemID: "id-b", ItemType: syncdomain.ItemTypeFile},
		{Path: "OtherFolder/c.txt", DriveID: driveID, ItemID: "id-c", ItemType: syncdomain.ItemTypeFile},
	})

	// Only id-a is in the seen set. id-b is an orphan under SharedFolder.
	// id-c is outside the prefix — should NOT be detected.
	seen := map[string]struct{}{
		"id-a": {},
	}

	orphans := syncstore.FindBaselineOrphans(bl, seen, driveID, "SharedFolder")
	require.Len(t, orphans, 1, "should detect only orphans under the prefix")
	assert.Equal(t, "SharedFolder/sub/b.txt", orphans[0].Path)
}

// Validates: R-2.1.6, R-2.8.4
func TestObserveRemoteFull_IntegratesOrphans(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	// Full delta returns 2 items (root + file1). Baseline has file1 + file2.
	// file2 should be detected as an orphan.
	mock := fullRefreshOneFileMock(driveID)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := syncfixture.TestLogger(t)

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
	bl := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: syncdomain.ItemTypeFile},
		&syncdomain.BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: syncdomain.ItemTypeFile},
	)

	full, err := testEngineFlow(t, e).observeRemoteFullWithShortcutTopology(ctx, bl, syncdomain.ResyncKindApplyDifferences)
	require.NoError(t, err)
	assert.Equal(t, "full-token", full.token)

	// Should hold file1's row (it exists in the baseline) and the orphan
	// delete of file2.
	var live, deletes int
	for _, ev := range full.events {
		if !ev.IsDeleted {
			live++
			assert.Equal(t, "file1.txt", ev.Path)
			continue
		}
		deletes++
		assert.Equal(t, "file2.txt", ev.Path, "orphan should be file2.txt")
		assert.Equal(t, "f2", ev.ItemID)
	}

	assert.Equal(t, 1, live, "should hold file1's row (file1 exists in baseline)")
	assert.Equal(t, 1, deletes, "should have 1 orphan delete")
}

// ---------------------------------------------------------------------------
// ProjectRemoteObservations — empty ItemID guard
// ---------------------------------------------------------------------------

// Validates: R-6.7.28
func TestChangeEventsToObservedItems_SkipsEmptyItemID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	events := []syncdomain.RemoteItem{
		{ObservedItem: syncdomain.ObservedItem{ItemID: "valid-1", Path: "a.txt", DriveID: driveID}},
		{ObservedItem: syncdomain.ObservedItem{ItemID: "", Path: "bad.txt", DriveID: driveID}},
		{ObservedItem: syncdomain.ObservedItem{ItemID: "valid-2", Path: "b.txt", DriveID: driveID}},
	}

	items := syncobserve.ProjectRemoteObservations(slog.Default(), events).Observed
	require.Len(t, items, 2, "empty ItemID event should be skipped")
	assert.Equal(t, "valid-1", items[0].ItemID)
	assert.Equal(t, "valid-2", items[1].ItemID)
}

// Validates: R-2.8.4
func TestShouldRunFullRemoteRefresh_NoCursor(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &syncfixture.EngineMockClient{})

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

// Validates: R-2.8.4
func TestShouldRunFullRemoteRefresh_Overdue(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	clock := syncfixture.NewManualClock(time.Unix(1_000, 0))
	installManualClockForTestEngine(e, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-syncdomain.FullRemoteRefreshInterval-time.Minute),
		syncdomain.RemoteObservationModeDelta,
	))

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.True(t, shouldRun)
}

// Validates: R-2.8.4
func TestShouldRunFullRemoteRefresh_WithinCadence(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	clock := syncfixture.NewManualClock(time.Unix(2_000, 0))
	installManualClockForTestEngine(e, clock)
	require.NoError(t, e.baseline.CommitObservationCursor(t.Context(), e.driveID, "token-1"))
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
		syncdomain.RemoteObservationModeDelta,
	))

	shouldRun, err := e.shouldRunFullRemoteRefresh(t.Context(), false)
	require.NoError(t, err)
	assert.False(t, shouldRun)
}

// Validates: R-2.8.4
func TestFullRemoteRefreshDelay_UsesPersistedTimestamp(t *testing.T) {
	t.Parallel()

	e, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	clock := syncfixture.NewManualClock(time.Unix(3_000, 0))
	installManualClockForTestEngine(e, clock)
	require.NoError(t, e.baseline.MarkFullRemoteRefresh(
		t.Context(),
		e.driveID,
		clock.Now().Add(-23*time.Hour),
		syncdomain.RemoteObservationModeDelta,
	))

	delay, err := e.fullRemoteRefreshDelay(t.Context())
	require.NoError(t, err)
	assert.Equal(t, time.Hour, delay)
}

// observeAndCommitRemoteFull tests (Item 6)
// ---------------------------------------------------------------------------

// Validates: R-2.1.6, R-2.15.1
func TestObserveAndCommitRemoteFull(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := fullRefreshOneFileMock(driveID)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := syncfixture.TestLogger(t)

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
	bl := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: syncdomain.ItemTypeFile},
		&syncdomain.BaselineEntry{Path: "file2.txt", DriveID: driveID, ItemID: "f2", ItemType: syncdomain.ItemTypeFile},
	)

	require.NoError(t, e.baseline.CommitObservation(ctx, []syncdomain.ObservedItem{
		{DriveID: driveID, ItemID: "f1", Path: "file1.txt", ItemType: syncdomain.ItemTypeFile},
		{DriveID: driveID, ItemID: "f2", Path: "file2.txt", ItemType: syncdomain.ItemTypeFile},
	}, "", driveID))

	pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		true,
	)
	require.NoError(t, err)

	// file1 is still there; the orphan delete removed file2.
	remoteRows, err := e.baseline.ListRemoteState(ctx)
	require.NoError(t, err)
	remotePaths := make([]string, 0, len(remoteRows))
	for i := range remoteRows {
		remotePaths = append(remotePaths, remoteRows[i].Path)
	}
	assert.Equal(t, []string{"file1.txt"}, remotePaths)
	require.NotNil(t, pendingCursor)

	// Cursor should be returned as pending — NOT committed to DB.
	assert.Equal(t, "full-token", pendingCursor.CursorToken, "pending cursor should be returned")

	savedToken := syncstoretest.ReadObservationCursorForTest(t, e.baseline, ctx, driveID.String())
	assert.Empty(t, savedToken, "cursor should NOT be committed to DB by observeAndCommitRemoteCurrentState — it is deferred")
}

// Validates: R-2.10.4
func TestObserveAndCommitRemoteTruth_RemoteReadDeniedPersistsObservationFindings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, graph.ErrForbidden
		},
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	syncDir := t.TempDir()
	logger := syncfixture.TestLogger(t)

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

	pendingCursor, err := testEngineFlow(t, e).observeAndCommitRemoteCurrentState(
		ctx,
		bl,
		false,
	)
	require.NoError(t, err)
	assert.Nil(t, pendingCursor)

	issues, err := e.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "/", issues[0].Path)
	assert.Equal(t, syncdomain.IssueRemoteReadDenied, issues[0].IssueType)
	assert.Equal(t, syncdomain.SKPermRemoteRead(""), issues[0].ScopeKey)

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
	require.NotNil(t, rt.resources.refreshResults)

	select {
	case result := <-rt.resources.refreshResults:
		require.NoError(t, rt.applyRemoteRefreshResult(ctx, &result))
	case <-time.After(syncfixture.EventWait):
		require.Fail(t, "refresh result was not delivered")
	}

	assert.False(t, rt.resources.refreshActive, "refreshActive should be false after applying refresh result")
}

// Validates: R-2.1.6
func TestRunFullRemoteRefreshAsync_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	logger := syncfixture.TestLogger(t)

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
	bl := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "file1.txt", DriveID: driveID, ItemID: "f1", ItemType: syncdomain.ItemTypeFile},
	)

	ready := setupWatchEngine(t, e)
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

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

	batch := testWatchRuntime(t, e).resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch, "remote refresh should mark dirty work for the watch loop")
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.1.6
func TestRunFullRemoteRefreshAsync_DeltaError(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, errors.New("delta unavailable")
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

	// Should not panic — error is logged and function returns.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	batch := testWatchRuntime(t, e).resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.True(t, batch.FullRefresh)
}

// Validates: R-2.8.4
func TestRunFullReconciliationAsync_NonBlocking(t *testing.T) {
	t.Parallel()

	// deltaFn blocks on a channel — lets us verify the call returns
	// before delta completes.
	unblock := make(chan struct{})

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			<-unblock
			return &graph.DeltaPage{DeltaLink: "tok"}, nil
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

	// Call should return immediately — goroutine is blocked in deltaFn.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// refreshActive should be true while delta is blocked.
	assert.True(t, testWatchRuntime(t, e).resources.refreshActive, "refreshActive should be true while goroutine runs")

	// Unblock delta and wait for completion.
	close(unblock)
	waitForRefreshDone(t, t.Context(), e)
}

// Validates: R-2.8.4
func TestRunFullRemoteRefreshAsync_SkipsIfRunning(t *testing.T) {
	t.Parallel()

	deltaCalled := false

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

	// Pre-set refreshActive — simulates a full remote refresh already in progress.
	testWatchRuntime(t, e).resources.refreshActive = true

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// deltaFn should not have been invoked.
	assert.False(t, deltaCalled, "deltaFn should not be called when full remote refresh is already running")

	testWatchRuntime(t, e).resources.refreshActive = false
}

// Validates: R-2.8.3
func TestWatchPipelineCleanup_KeepsRefreshResultChannelUsable(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)

	pipe, err := rt.initWatchInfra(t.Context(), syncdomain.SyncDownloadOnly, WatchOptions{})
	require.NoError(t, err)
	pipe.cleanup()

	done := make(chan struct{})
	go func() {
		finishFullRemoteRefresh(context.Background(), rt.resources.refreshResults, &syncobserve.RemoteObservationBatch{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(syncfixture.EventWait):
		require.FailNow(t, "watch cleanup left refresh result senders blocked")
	}
}

// Validates: R-2.8.4
func TestRunFullRemoteRefreshAsync_FeedsBuffer(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	logger := syncfixture.TestLogger(t)

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
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

	// Baseline is empty — delta returns a new file and the watch loop gets
	// a coarse dirty signal back from the remote refresh.
	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, t.Context(), e)

	batch := testWatchRuntime(t, e).resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch, "dirty scheduler should contain a coarse dirty signal from reconciliation")
	assert.False(t, batch.FullRefresh)
}

// Validates: R-2.1.2
func TestWatchDirtyScheduling_PathFanoutStillProducesOneCoarseDirtySignal(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	require.NoError(t, rt.handleWatchLocalObservationBatch(t.Context(), &syncobserve.LocalObservation{Dirty: true}))
	rt.markDirtyFromRemoteBatch(&syncobserve.RemoteObservationBatch{
		Changes: []syncobserve.RemoteChange{
			{Path: "alpha.txt"},
			{OldPath: "beta.txt"},
			{Path: "gamma.txt", OldPath: "delta.txt"},
		},
	})

	batch := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
	assert.Nil(t, rt.resources.dirtyBuf.FlushImmediate())
}

// Validates: R-2.8.4
func TestWatchLoop_RefreshTick_RunsPeriodicFullRemoteRefreshThroughResultHandoff(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	refreshC := make(chan time.Time, 1)
	rt.resources.refreshCh = refreshC
	_, done := syncfixture.RunInBackground(t, ctx, func(loopCtx context.Context) error {
		return rt.runWatchLoop(loopCtx, &watchPipeline{
			runtime: rt,
			bl:      bl,
			mode:    syncdomain.SyncBidirectional,
		})
	})

	refreshC <- time.Now()

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventRemoteRefreshApplied
	}, "watch loop applied periodic full remote refresh result")

	batch := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)

	select {
	case ta := <-ready:
		require.Failf(t, "periodic full remote refresh dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	savedToken := syncstoretest.ReadObservationCursorForTest(t, eng.baseline, t.Context(), driveID.String())
	assert.Equal(t, "watch-refresh-token", savedToken)

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.8.3
func TestRunFullRemoteRefreshAsync_ShutdownAfterCommit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	logger := syncfixture.TestLogger(t)

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
	attachDebugEventRecorderWithHook(e, func(event syncdomain.EngineDebugEvent) {
		if event.Type == syncdomain.EngineDebugEventRemoteRefreshCommitted {
			cancel()
		}
	})

	bl, err := e.baseline.Load(t.Context())
	require.NoError(t, err)

	setupWatchEngine(t, e)
	testWatchRuntime(t, e).resources.dirtyBuf = syncobserve.NewDirtyBuffer(e.logger)

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)
	waitForRefreshDone(t, ctx, e)

	// Verify observations WERE committed to SQLite — proving we took
	// the post-commit shutdown path, not the commit-failed path.
	// CommitObservation saves the primary observation cursor for this drive DB.
	savedToken := syncstoretest.ReadObservationCursorForTest(t, e.baseline, t.Context(), driveID.String())
	assert.Equal(t, "shutdown-tok", savedToken,
		"delta token should be saved — CommitObservation must have succeeded")

	batch := testWatchRuntime(t, e).resources.dirtyBuf.FlushImmediate()
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

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	testWatchRuntime(t, e).resources.refreshActive = true

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	// The skip message should appear in the Info-level log buffer.
	// If it were still at Debug level, the Info-level handler would exclude it.
	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "full remote refresh skipped",
		"skip message should be logged at Info level (not Debug)")

	testWatchRuntime(t, e).resources.refreshActive = false
}

// Validates: R-6.6.14
func TestRunFullRemoteRefreshAsync_DurationInCompletionLog(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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

// Validates: R-6.6.14
func TestRunFullRemoteRefreshAsync_DurationInNoChangesLog(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	// Return only root item — FullDelta produces 0 events (root is skipped),
	// so the function takes the "no changes" path.
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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

// fullRefreshOneFileMock enumerates the root and file1 only; any other item
// is confirmed gone by a live lookup (D-008).
func fullRefreshOneFileMock(driveID driveid.ID) *syncfixture.EngineMockClient {
	return &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file1.txt", ParentID: "root", DriveID: driveID, Size: 100, QuickXorHash: "qxh1"},
				},
				DeltaLink: "full-token",
			}, nil
		},
		GetItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
			return nil, graph.ErrNotFound
		},
	}
}
