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

	return newPendingCreateTestExecutorWith(t, ul, logger, nil)
}

// newPendingCreateTestExecutorWith is newPendingCreateTestExecutor with a last
// word on the executor's inputs, whose PendingCreates is the returned store.
func newPendingCreateTestExecutorWith(
	t *testing.T, ul driveops.Uploader, logger *slog.Logger, configure func(*syncexec.ExecutorInputs),
) (*syncexec.Executor, *syncstore.SyncStore, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	store := syncstoretest.NewTestStore(t)
	in := syncexec.ExecutorInputs{
		Items:          &syncfixture.ExecutorMockItemClient{},
		Downloads:      &syncfixture.ExecutorMockDownloader{},
		Uploads:        ul,
		SyncTree:       syncTree,
		DriveID:        driveid.New(syncfixture.TestDriveID),
		Logger:         logger,
		PendingCreates: store,
	}
	if configure != nil {
		configure(&in)
	}

	return syncexec.NewExecution(mustNewTestExecutorConfig(t, &in), syncfixture.EmptyBaseline()), store, syncRoot
}

// mustNewTestExecutorConfig builds an executor configuration from in,
// filling what it leaves zero of the transfer manager (over its downloader
// and uploader) and the clock (the wall clock).
func mustNewTestExecutorConfig(t *testing.T, in *syncexec.ExecutorInputs) *syncexec.ExecutorConfig {
	t.Helper()

	if in.Transfers == nil {
		in.Transfers = driveops.NewTransferManager(in.Downloads, in.Uploads, nil, in.Logger)
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	cfg, err := syncexec.NewExecutorConfig(in)
	require.NoError(t, err)

	return cfg
}

func newTestExecutorConfig(t *testing.T, items *syncfixture.ExecutorMockItemClient, dl *syncfixture.ExecutorMockDownloader, ul *syncfixture.ExecutorMockUploader) (*syncexec.ExecutorConfig, string) {
	t.Helper()

	return newTestExecutorConfigWith(t, items, dl, ul, nil)
}

// newTestExecutorConfigWith is newTestExecutorConfig with a last word on the
// executor's inputs.
func newTestExecutorConfigWith(
	t *testing.T,
	items *syncfixture.ExecutorMockItemClient,
	dl *syncfixture.ExecutorMockDownloader,
	ul *syncfixture.ExecutorMockUploader,
	configure func(*syncexec.ExecutorInputs),
) (*syncexec.ExecutorConfig, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	in := syncexec.ExecutorInputs{
		Items:     items,
		Downloads: dl,
		Uploads:   ul,
		SyncTree:  syncTree,
		DriveID:   driveid.New(synctest.TestDriveID),
		Logger:    synctest.TestLogger(t),
		Now:       func() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) },
	}
	if configure != nil {
		configure(&in)
	}

	return mustNewTestExecutorConfig(t, &in), syncRoot
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

	plan, err := syncplan.NewPlanner(logger).PlanSnapshot(
		syncstoretest.ReadPlanningSnapshot(t, store),
		syncdomain.Decide,
		syncplan.PlannerMountContext{DriveID: driveid.New(syncfixture.EngineTestDriveID)},
		mode,
	)
	require.NoError(t, err)

	return plan
}

// reconciliationRowsForStore decides what store holds as a pass's planner
// does: the reconciliation rows of the planning read's comparison rows.
func reconciliationRowsForStore(t *testing.T, store *syncstore.SyncStore) []syncdomain.SQLiteReconciliationRow {
	t.Helper()

	_, reconciliations := syncplan.DecideComparisons(syncstoretest.ReadPlanningSnapshot(t, store).Comparisons, syncdomain.Decide)

	return reconciliations
}

// newWorkerTestSetupWith builds a worker test's executor configuration, store
// and sync root, with a last word on the executor's inputs.
func newWorkerTestSetupWith(t *testing.T, configure func(*syncexec.ExecutorInputs)) (
	*syncexec.ExecutorConfig, *syncstore.SyncStore, string,
) {
	t.Helper()

	mgr := syncstoretest.NewTestManager(t)

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	in := syncexec.ExecutorInputs{
		Items: &syncfixture.WorkerMockItemClient{
			CreateFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
				return &graph.Item{ID: "created-" + name, Name: name}, nil
			},
			DeleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
				return nil
			},
		},
		Downloads: &syncfixture.WorkerMockDownloader{
			DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
				n, err := w.Write([]byte("file-content"))
				return int64(n), err
			},
		},
		Uploads:  &syncfixture.WorkerMockUploader{},
		SyncTree: syncTree,
		DriveID:  driveid.New("0000000000000001"),
		Logger:   synctest.TestLogger(t),
		Now:      func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) },
	}
	if configure != nil {
		configure(&in)
	}

	return mustNewTestExecutorConfig(t, &in), mgr, syncRoot
}

// reconciliationKindsByPath names each row's reconciliation kind by its path.
func reconciliationKindsByPath(rows []syncdomain.SQLiteReconciliationRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ReconciliationKind.String()
	}

	return out
}

// comparisonKindsByPath names each decided row's comparison kind by its path.
func comparisonKindsByPath(rows []syncplan.DecidedComparison) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ComparisonKind.String()
	}

	return out
}
