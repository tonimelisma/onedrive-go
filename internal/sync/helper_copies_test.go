package sync

// Private copies of test helpers whose home (syncexec, syncobserve,
// syncplan) these tests cannot import, because it imports this package or
// is another package's tests.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// adoptTestPlaceholder records the placeholder of a create at doc.txt and
// has observation adopt it, still empty, as synced empty content.
func adoptTestPlaceholder(t *testing.T, store *syncstore.SyncStore) syncdomain.ObservedItem {
	t.Helper()

	const path = "doc.txt"
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: path, ParentID: syncexec.GraphRootID, CreatedETag: placeholderCreatedETag,
	}))
	placeholder := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: testPlaceholderID, ParentID: syncexec.GraphRootID, Path: path, ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: placeholderCreatedETag,
	}
	require.NoError(t, store.CommitObservation(t.Context(), []syncdomain.ObservedItem{placeholder}, "t-adopt", driveID))
	_, _, adopted := syncstoretest.ReadBaselineRow(t, store, testPlaceholderID)
	require.True(t, adopted, "the placeholder is adopted")

	return placeholder
}

const (
	testPlaceholderID      = "placeholder"
	placeholderCreatedETag = `"p,1"`
)

// newPendingCreateTestExecutor returns an executor that uploads through ul and
// records its pending creates in a real store, plus that store and the sync
// root.
func newPendingCreateTestExecutor(t *testing.T, ul driveops.Uploader, logger *slog.Logger) (*syncexec.Executor, *syncstore.SyncStore, string) {
	t.Helper()

	return newPendingCreateTestExecutorWithHooks(t, ul, logger, syncexec.Hooks{})
}

// newPendingCreateTestExecutorWithHooks is newPendingCreateTestExecutor with
// the executor's test seams.
func newPendingCreateTestExecutorWithHooks(
	t *testing.T, ul driveops.Uploader, logger *slog.Logger, hooks syncexec.Hooks,
) (*syncexec.Executor, *syncstore.SyncStore, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	dl := &syncfixture.ExecutorMockDownloader{}
	cfg := syncexec.NewExecutorConfig(&syncfixture.ExecutorMockItemClient{}, dl, ul, syncTree, driveid.New(syncfixture.TestDriveID), logger, nil, hooks)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	store := syncstoretest.NewTestStore(t)
	cfg.SetPendingCreates(store)

	return syncexec.NewExecution(cfg, syncfixture.EmptyBaseline()), store, syncRoot
}

func newTestExecutorConfig(t *testing.T, items *syncfixture.ExecutorMockItemClient, dl *syncfixture.ExecutorMockDownloader, ul *syncfixture.ExecutorMockUploader) (*syncexec.ExecutorConfig, string) {
	t.Helper()

	return newTestExecutorConfigWithPathConvergence(t, items, dl, ul, nil)
}

func newTestExecutorConfigWithPathConvergence(
	t *testing.T,
	items *syncfixture.ExecutorMockItemClient,
	dl *syncfixture.ExecutorMockDownloader,
	ul *syncfixture.ExecutorMockUploader,
	pathConvergence driveops.PathConvergence,
) (*syncexec.ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	driveID := driveid.New(synctest.TestDriveID)
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	cfg := syncexec.NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, pathConvergence, syncexec.Hooks{})
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) })

	return cfg, syncRoot
}

// forwardedSnapshot takes the snapshot a full scan sent, failing when it
// sent none. The scan sends before it returns, so the snapshot is already
// buffered.
func forwardedSnapshot(t *testing.T, batches <-chan syncobserve.LocalObservation) *syncobserve.LocalObservation {
	t.Helper()

	select {
	case batch := <-batches:
		require.Equal(t, syncobserve.LocalSnapshot, batch.Kind)
		return &batch
	default:
		require.Fail(t, "the full scan sent no snapshot")
		return &syncobserve.LocalObservation{}
	}
}

func planCurrentStateForStore(t *testing.T, store *syncstore.SyncStore) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForStoreWithMode(t, store, syncdomain.SyncBidirectional)
}

func planCurrentStateForStoreWithMode(t *testing.T, store *syncstore.SyncStore, mode syncdomain.SyncMode) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForStoreWithLogger(t, store, mode, syncfixture.TestLogger(t))
}

// planCurrentStateForStoreWithLogger plans what store holds, as the engine
// reads it at the start of a pass, logging to logger.
func planCurrentStateForStoreWithLogger(t *testing.T, store *syncstore.SyncStore, mode syncdomain.SyncMode, logger *slog.Logger) *syncdomain.ActionPlan {
	t.Helper()

	ctx := t.Context()
	bl, err := store.Load(ctx)
	require.NoError(t, err)

	tx, err := syncstore.BeginPerfTx(ctx, store.RawDBForTest())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	rawLocalRows, err := syncstore.ListLocalStateRows(ctx, tx)
	require.NoError(t, err)

	observationState, err := syncstore.ReadObservationStateFromTx(ctx, tx)
	require.NoError(t, err)
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = driveid.New(syncfixture.EngineTestDriveID)
	}

	rawRemoteRows, err := syncstore.QueryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT `+syncstore.SqlSelectRemoteStateCols+` FROM remote_state`,
		contentDriveID,
	)
	require.NoError(t, err)

	visibleRows, err := syncstore.ReplacePlannerVisibleStateTx(ctx, tx, rawLocalRows, rawRemoteRows, contentDriveID)
	require.NoError(t, err)

	comparisons, err := syncstore.QueryComparisonStateWithRunnerForTables(
		ctx,
		tx,
		syncstore.PlannerVisibleLocalStateTable,
		syncstore.PlannerVisibleRemoteStateTable,
	)
	require.NoError(t, err)
	reconciliations, err := syncstore.QueryReconciliationStateWithRunnerForTables(
		ctx,
		tx,
		syncstore.PlannerVisibleLocalStateTable,
		syncstore.PlannerVisibleRemoteStateTable,
	)
	require.NoError(t, err)
	observationIssues, err := syncstore.QueryObservationIssueRowsWithRunner(ctx, tx)
	require.NoError(t, err)

	planner := syncplan.NewPlanner(logger)
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		visibleRows.Local,
		visibleRows.Remote,
		observationIssues,
		bl,
		syncplan.PlannerMountContext{DriveID: driveid.New(syncfixture.EngineTestDriveID)},
		mode,
	)
	require.NoError(t, err)

	return plan
}

func newWorkerTestSetup(t *testing.T) (
	*syncexec.ExecutorConfig, *syncstore.SyncStore, string,
) {
	t.Helper()

	return newWorkerTestSetupWithHooks(t, syncexec.Hooks{})
}

// newWorkerTestSetupWithHooks is newWorkerTestSetup with the executor's and
// its pools' test seams.
func newWorkerTestSetupWithHooks(t *testing.T, hooks syncexec.Hooks) (
	*syncexec.ExecutorConfig, *syncstore.SyncStore, string,
) {
	t.Helper()

	mgr := syncstoretest.NewTestManager(t)

	syncRoot := t.TempDir()
	driveID := driveid.New("0000000000000001")
	logger := synctest.TestLogger(t)
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	items := &syncfixture.WorkerMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			return &graph.Item{ID: "created-" + name, Name: name}, nil
		},
		DeleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
			return nil
		},
	}
	dl := &syncfixture.WorkerMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("file-content"))
			return int64(n), err
		},
	}
	ul := &syncfixture.WorkerMockUploader{}

	cfg := syncexec.NewExecutorConfig(items, dl, ul, syncTree, driveID, logger, nil, hooks)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })
	return cfg, mgr, syncRoot
}
