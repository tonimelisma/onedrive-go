package syncexec

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
)

// ---------------------------------------------------------------------------
// Worker pool mock types (prefixed to avoid collision with executor_test.go)
// ---------------------------------------------------------------------------

type workerMockItemClient = testMockItemClient

type workerMockDownloader struct {
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *workerMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

type workerMockUploader struct {
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *workerMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newWorkerTestSetup(t *testing.T) (
	*ExecutorConfig, *syncstore.SyncStore, string,
) {
	t.Helper()

	mgr := synctest.NewTestStore(t)

	syncRoot := t.TempDir()
	driveID := driveid.New("0000000000000001")
	logger := synctest.TestLogger(t)

	items := &workerMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			return &graph.Item{ID: "created-" + name, Name: name}, nil
		},
		deleteItemFn: func(_ context.Context, _ driveid.ID, _ string) error {
			return nil
		},
	}
	dl := &workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("file-content"))
			return int64(n), err
		},
	}
	ul := &workerMockUploader{}

	cfg := NewExecutorConfig(items, dl, ul, syncRoot, driveID, logger)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	cfg.SetNowFunc(func() time.Time { return time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC) })
	return cfg, mgr, syncRoot
}

// testDepGraphHelper wraps a syncdispatch.DepGraph for worker tests, providing
// the readyCh and doneCh channels that WorkerPool expects. Add sends ready
// actions to readyCh; drainAndComplete calls dg.Complete and sends newly-ready
// dependents to readyCh.
type testDepGraphHelper struct {
	dg      *syncdispatch.DepGraph
	readyCh chan *synctypes.TrackedAction
}

func newTestDepGraphHelper(t *testing.T) *testDepGraphHelper {
	t.Helper()
	return &testDepGraphHelper{
		dg:      syncdispatch.NewDepGraph(synctest.TestLogger(t)),
		readyCh: make(chan *synctypes.TrackedAction, 64),
	}
}

// Add wraps DepGraph.Add and sends ready actions to readyCh.
func (h *testDepGraphHelper) Add(action *synctypes.Action, id int64, deps []int64) {
	ta := h.dg.Add(action, id, deps)
	if ta != nil {
		h.readyCh <- ta
	}
}

// Ready returns the readyCh for WorkerPool.
func (h *testDepGraphHelper) Ready() <-chan *synctypes.TrackedAction { return h.readyCh }

// Done returns the DepGraph's Done channel.
func (h *testDepGraphHelper) Done() <-chan struct{} { return h.dg.Done() }

// drainAndComplete drains the worker result channel, calling dg.Complete
// for each result and sending newly-ready dependents to readyCh. Returns
// the collected results. This simulates the engine's drain goroutine.
func (h *testDepGraphHelper) drainAndComplete(results <-chan synctypes.WorkerResult) []synctypes.WorkerResult {
	var collected []synctypes.WorkerResult
	for r := range results {
		collected = append(collected, r)
		ready, _ := h.dg.Complete(r.ActionID)
		for _, ta := range ready {
			h.readyCh <- ta
		}
	}
	return collected
}

// runPoolWithDrain starts the pool, drains results in a goroutine (calling
// Complete on each), waits for all actions to finish, then stops the pool
// and returns the collected results.
func runPoolWithDrain(ctx context.Context, pool *WorkerPool, dgh *testDepGraphHelper) []synctypes.WorkerResult {
	pool.Start(ctx, 4)

	var results []synctypes.WorkerResult
	done := make(chan struct{})
	go func() {
		results = dgh.drainAndComplete(pool.Results())
		close(done)
	}()

	pool.Wait()
	pool.Stop()
	<-done
	return results
}

// countResults counts succeeded and failed results.
func countResults(results []synctypes.WorkerResult) (succeeded, failed int) {
	for i := range results {
		r := &results[i]
		if r.Success {
			succeeded++
		} else {
			failed++
		}
	}
	return succeeded, failed
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestWorkerPool_FolderCreate(t *testing.T) {
	t.Parallel()

	cfg, mgr, syncRoot := newWorkerTestSetup(t)
	ctx := t.Context()

	actions := []synctypes.Action{
		{
			Type:       synctypes.ActionFolderCreate,
			Path:       "Documents",
			DriveID:    driveid.New("0000000000000001"),
			ItemID:     "folder-doc",
			CreateSide: synctypes.CreateLocal,
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:   "folder-doc",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "root",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	succeeded, failed := countResults(results)
	assert.Equal(t, 0, failed, "failed actions")
	assert.Equal(t, 1, succeeded)

	// Verify directory was created.
	info, statErr := os.Stat(filepath.Join(syncRoot, "Documents"))
	require.NoError(t, statErr, "stat Documents")
	assert.True(t, info.IsDir(), "Documents should be a directory")

	// Verify baseline was updated.
	bl, loadErr := mgr.Load(ctx)
	require.NoError(t, loadErr)

	_, ok := bl.GetByPath("Documents")
	assert.True(t, ok, "baseline entry not found for Documents")
}

// Validates: R-5.1
func TestWorkerPool_DependencyChain(t *testing.T) {
	t.Parallel()

	cfg, mgr, syncRoot := newWorkerTestSetup(t)
	ctx := t.Context()

	// Folder create → then download into that folder.
	actions := []synctypes.Action{
		{
			Type:       synctypes.ActionFolderCreate,
			Path:       "NewDir",
			DriveID:    driveid.New("0000000000000001"),
			CreateSide: synctypes.CreateLocal,
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:   "newdir-id",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "root",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
		{
			Type:    synctypes.ActionDownload,
			Path:    "NewDir/file.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "file-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:   "file-id",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "newdir-id",
					Size:     12,
					Hash:     "testhash",
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)
	dgh.Add(&actions[1], 1, []int64{0})

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	succeeded, failed := countResults(results)
	assert.Equal(t, 0, failed, "failed actions")
	assert.Equal(t, 2, succeeded)

	// Verify file was downloaded.
	content, readErr := trustedpath.ReadFile(filepath.Join(syncRoot, "NewDir", "file.txt"))
	require.NoError(t, readErr, "read file")
	assert.Equal(t, "file-content", string(content))
}

func TestWorkerPool_StopCancelsWork(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionDownload,
			Path:    "slow.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "slow-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:  "slow-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    100,
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	pool.Start(ctx, 4)

	// Give workers a moment to pick up the action.
	time.Sleep(50 * time.Millisecond)

	// Stop should not hang.
	done := make(chan struct{})
	go func() {
		pool.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(5 * time.Second):
		require.Fail(t, "Stop() did not return within timeout")
	}
}

// Validates: R-5.1
func TestWorkerPool_ResultChannel(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionLocalDelete,
			Path:    "result-test.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "del-id",
			View:    &synctypes.PathView{},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 42, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	// Verify result for the action.
	var found bool
	for _, r := range results {
		if r.Path == "result-test.txt" {
			assert.True(t, r.Success)
			assert.Equal(t, int64(42), r.ActionID)
			found = true
		}
	}
	require.True(t, found, "expected result for result-test.txt in channel")
}

// TestWorkerPool_FailedOutcome verifies that when an action execution fails,
// the worker reports the failure via the result channel.
func TestWorkerPool_FailedOutcome(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	// Configure a download mock that always fails.
	cfg.SetDownloads(&workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, fmt.Errorf("simulated download failure")
		},
	})
	cfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads(), cfg.Uploads(), nil, synctest.TestLogger(t)))

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionDownload,
			Path:    "fail-me.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "fail-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:  "fail-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    10,
					Hash:    "somehash",
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	succeeded, failed := countResults(results)
	assert.Equal(t, 0, succeeded)
	assert.GreaterOrEqual(t, failed, 1, "expected at least one failure")

	// Verify the failure details.
	var foundFailure bool
	for _, r := range results {
		if !r.Success && r.Path == "fail-me.txt" {
			foundFailure = true
			assert.NotEmpty(t, r.ErrMsg)
			require.Error(t, r.Err, "Err should carry the full error")
		}
	}
	assert.True(t, foundFailure, "expected failure result for fail-me.txt")
}

// ---------------------------------------------------------------------------
// Regression: B-090 — parent resolution via baseline (no createdFolders map)
// ---------------------------------------------------------------------------

// TestWorkerPool_FolderCreateThenUpload_ParentResolvedFromBaseline verifies
// that when action 0 creates a folder and action 1 uploads a file into that
// folder, the upload resolves its parentID from the baseline.
func TestWorkerPool_FolderCreateThenUpload_ParentResolvedFromBaseline(t *testing.T) {
	t.Parallel()

	cfg, mgr, syncRoot := newWorkerTestSetup(t)
	ctx := t.Context()

	var capturedParentID string

	// Override uploader to capture the parentID used.
	cfg.SetUploads(&workerMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			capturedParentID = parentID

			return &graph.Item{ID: "uploaded-into-folder", ETag: "e1"}, nil
		},
	})
	cfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads(), cfg.Uploads(), nil, synctest.TestLogger(t)))

	// Action 0: create folder "Uploads".
	// Action 1: upload file "Uploads/doc.txt" into that folder.
	actions := []synctypes.Action{
		{
			Type:       synctypes.ActionFolderCreate,
			Path:       "Uploads",
			DriveID:    driveid.New("0000000000000001"),
			CreateSide: synctypes.CreateLocal,
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:   "uploads-folder-id",
					DriveID:  driveid.New("0000000000000001"),
					ParentID: "root",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
		{
			Type:    synctypes.ActionUpload,
			Path:    "Uploads/doc.txt",
			DriveID: driveid.New("0000000000000001"),
			View:    &synctypes.PathView{Path: "Uploads/doc.txt"},
		},
	}

	// Write the local file that will be uploaded.
	absPath := filepath.Join(syncRoot, "Uploads", "doc.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o700))
	require.NoError(t, os.WriteFile(absPath, []byte("upload content"), 0o600))

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)
	dgh.Add(&actions[1], 1, []int64{0})

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	succeeded, failed := countResults(results)
	assert.Equal(t, 0, failed, "failed actions")
	assert.Equal(t, 2, succeeded)

	// The upload must have resolved its parent from the baseline entry committed
	// by the folder-create action. For CreateSide=CreateLocal, folderOutcome uses
	// action.View.Remote.ItemID ("uploads-folder-id") as the baseline ItemID.
	assert.Equal(t, "uploads-folder-id", capturedParentID,
		"upload parentID should be resolved from baseline after folder create")
}

// TestWorkerPool_PanicRecovery verifies that a panic in action execution
// doesn't crash the process — the worker recovers and reports a failure
// result. The pool completes normally.
func TestWorkerPool_PanicRecovery(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	// Configure a download mock that panics.
	cfg.SetDownloads(&workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			panic("intentional panic for testing")
		},
	})
	cfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads(), cfg.Uploads(), nil, synctest.TestLogger(t)))

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionDownload,
			Path:    "panic-me.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "panic-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:  "panic-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    10,
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	results := runPoolWithDrain(ctx, pool, dgh)

	// If we got here, the panic was recovered — the process didn't crash.
	_, failed := countResults(results)
	assert.GreaterOrEqual(t, failed, 1, "panic should be recorded as failure")

	// Verify the error message contains "panic:".
	var foundPanicResult bool
	for _, r := range results {
		if !r.Success && r.Path == "panic-me.txt" {
			assert.Contains(t, r.ErrMsg, "panic:")
			foundPanicResult = true
		}
	}
	assert.True(t, foundPanicResult,
		"expected panic failure result for panic-me.txt in result channel")
}

// ---------------------------------------------------------------------------
// Worker→Engine ownership: worker never calls Complete (R-6.8.9)
// ---------------------------------------------------------------------------

// Validates: R-6.8.9
func TestWorker_NeverCallsComplete(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionLocalDelete,
			Path:    "test-no-complete.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "del-id",
			View:    &synctypes.PathView{},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	pool.Start(ctx, 4)

	// Read one result from the channel — worker must send a result.
	var result synctypes.WorkerResult
	select {
	case r := <-pool.Results():
		result = r
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for worker result")
	}

	// Worker sent the result but did NOT call Complete — dgh.Done should
	// NOT have fired (the action is still in-flight from DepGraph's perspective).
	select {
	case <-dgh.Done():
		require.Fail(t, "dgh.Done fired — worker must not call Complete")
	default:
		// Expected: Done not fired because Complete was not called.
	}

	assert.True(t, result.Success)
	assert.Equal(t, "test-no-complete.txt", result.Path)
	assert.Equal(t, int64(0), result.ActionID, "ActionID should match TrackedAction.ID")

	// Now WE call Complete (simulating the engine), which should fire Done.
	_, _ = dgh.dg.Complete(result.ActionID)

	select {
	case <-dgh.Done():
		// Expected: Done fires after engine calls Complete.
	case <-time.After(5 * time.Second):
		require.Fail(t, "dgh.Done did not fire after Complete")
	}

	pool.Stop()
}

// ---------------------------------------------------------------------------
// WorkerResult populates from TrackedAction (R-2.10.16, R-6.8.12)
// ---------------------------------------------------------------------------

// Validates: R-2.10.16, R-6.8.12
func TestWorkerResult_PopulatesFromAction(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	actions := []synctypes.Action{
		{
			Type:              synctypes.ActionLocalDelete,
			Path:              "shortcut-action.txt",
			DriveID:           driveid.New("0000000000000001"),
			ItemID:            "del-id",
			View:              &synctypes.PathView{},
			TargetShortcutKey: "remoteDrive:remoteItem",
			TargetDriveID:     driveid.New("0000000000000002"),
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 77, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	pool.Start(ctx, 4)

	var result synctypes.WorkerResult
	select {
	case r := <-pool.Results():
		result = r
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for worker result")
	}

	assert.Equal(t, "shortcut-action.txt", result.Path)
	assert.Equal(t, driveid.New("0000000000000001"), result.DriveID)
	assert.Equal(t, driveid.New("0000000000000002"), result.TargetDriveID,
		"TargetDriveID should flow through from Action")
	assert.Equal(t, "remoteDrive:remoteItem", result.ShortcutKey,
		"ShortcutKey should flow through from Action")
	assert.False(t, result.IsTrial, "should not be a trial action")
	assert.Empty(t, result.TrialScopeKey, "no trial scope key")
	assert.Equal(t, int64(77), result.ActionID, "ActionID should match TrackedAction.ID")

	// Clean up.
	_, _ = dgh.dg.Complete(result.ActionID)
	pool.Stop()
}

// Validates: R-6.8.12
func TestWorkerResult_HTTPStatusAndRetryAfter(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	// Configure a download mock that returns a 429 with Retry-After.
	cfg.SetDownloads(&workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, &graph.GraphError{
				StatusCode: 429,
				Message:    "throttled",
				RetryAfter: 30 * time.Second,
			}
		},
	})
	cfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads(), cfg.Uploads(), nil, synctest.TestLogger(t)))

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionDownload,
			Path:    "throttled.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "throttled-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:  "throttled-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    10,
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)
	pool.Start(ctx, 4)

	var result synctypes.WorkerResult
	select {
	case r := <-pool.Results():
		result = r
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for worker result")
	}

	assert.False(t, result.Success)
	assert.Equal(t, 429, result.HTTPStatus)
	assert.Equal(t, 30*time.Second, result.RetryAfter,
		"RetryAfter should be extracted from GraphError")

	_, _ = dgh.dg.Complete(result.ActionID)
	pool.Stop()
}

func TestExtractHTTPStatus_GraphError(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 404, Message: "not found"}
	assert.Equal(t, 404, ExtractHTTPStatus(ge))
}

func TestExtractHTTPStatus_WrappedGraphError(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 429, Message: "throttled"}
	wrapped := fmt.Errorf("download failed: %w", ge)
	assert.Equal(t, 429, ExtractHTTPStatus(wrapped))
}

func TestExtractHTTPStatus_NonGraphError(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, ExtractHTTPStatus(fmt.Errorf("network timeout")))
}

func TestExtractHTTPStatus_Nil(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, ExtractHTTPStatus(nil))
}

func TestExtractRetryAfter_GraphError(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 429, RetryAfter: 30 * time.Second}
	assert.Equal(t, 30*time.Second, ExtractRetryAfter(ge))
}

func TestExtractRetryAfter_Nil(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Duration(0), ExtractRetryAfter(nil))
}

// ---------------------------------------------------------------------------
// Action drive identity methods (R-6.8.13)
// ---------------------------------------------------------------------------

// Validates: R-6.8.13, R-2.10.16, R-2.10.17
func TestAction_TargetsOwnDrive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		targetShortcutKey string
		wantOwnDrive      bool
		wantShortcutKey   string
	}{
		{
			name:            "own-drive action",
			wantOwnDrive:    true,
			wantShortcutKey: "",
		},
		{
			name:              "shortcut action",
			targetShortcutKey: "remoteDrive:remoteItem",
			wantOwnDrive:      false,
			wantShortcutKey:   "remoteDrive:remoteItem",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := synctypes.Action{TargetShortcutKey: tt.targetShortcutKey}
			assert.Equal(t, tt.wantOwnDrive, a.TargetsOwnDrive())
			assert.Equal(t, tt.wantShortcutKey, a.ShortcutKey())
		})
	}
}

// ---------------------------------------------------------------------------
// Engine-owned counters (R-6.8.9)
// ---------------------------------------------------------------------------

func TestEngineOwnsCounters(t *testing.T) {
	t.Parallel()

	cfg, mgr, _ := newWorkerTestSetup(t)
	ctx := t.Context()

	// 2 actions: one succeeds, one fails.
	cfg.SetDownloads(&workerMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			if itemID == "fail-id" {
				return 0, fmt.Errorf("simulated failure")
			}
			n, err := w.Write([]byte("ok"))
			return int64(n), err
		},
	})
	cfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads(), cfg.Uploads(), nil, synctest.TestLogger(t)))

	actions := []synctypes.Action{
		{
			Type:    synctypes.ActionLocalDelete,
			Path:    "ok.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "ok-id",
			View:    &synctypes.PathView{},
		},
		{
			Type:    synctypes.ActionDownload,
			Path:    "fail.txt",
			DriveID: driveid.New("0000000000000001"),
			ItemID:  "fail-id",
			View: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemID:  "fail-id",
					DriveID: driveid.New("0000000000000001"),
					Size:    10,
				},
			},
		},
	}

	dgh := newTestDepGraphHelper(t)
	dgh.Add(&actions[0], 0, nil)
	dgh.Add(&actions[1], 1, nil)

	pool := NewWorkerPool(cfg, dgh.Ready(), dgh.Done(), mgr, synctest.TestLogger(t), 10)

	// Simulate engine-owned counters.
	var succeeded, failed atomic.Int32
	pool.Start(ctx, 4)

	done := make(chan struct{})
	go func() {
		for r := range pool.Results() {
			if r.Success {
				succeeded.Add(1)
			} else {
				failed.Add(1)
			}
			_, _ = dgh.dg.Complete(r.ActionID)
		}
		close(done)
	}()

	pool.Wait()
	pool.Stop()
	<-done

	assert.Equal(t, int32(1), succeeded.Load(), "engine should count 1 success")
	assert.Equal(t, int32(1), failed.Load(), "engine should count 1 failure")
}

// TestExtractRetryAfter_Wrapped verifies that ExtractRetryAfter works with wrapped errors.
func TestExtractRetryAfter_Wrapped(t *testing.T) {
	t.Parallel()

	ge := &graph.GraphError{StatusCode: 503, RetryAfter: 120 * time.Second}
	wrapped := fmt.Errorf("request failed: %w", ge)
	assert.Equal(t, 120*time.Second, ExtractRetryAfter(wrapped))
}
