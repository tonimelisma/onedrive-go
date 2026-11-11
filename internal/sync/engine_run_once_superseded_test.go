package sync

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.1.7
//
// A move OneDrive refuses with a 412 (the folder changed under the plan) is
// superseded and replanned. A one-shot run used to end there, answering
// "2 will be retried on the next sync" for work a second pass finishes at
// once (fault round of 2026-11-03); the run now makes that pass itself.
func TestRunOnce_ReplansASupersededMoveInTheSameRun(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-a", "item-dir", "a.txt", "alpha\n")
	client := remote.client(t)
	move := client.moveItemOptsFn
	var refused atomic.Int32
	client.moveItemOptsFn = func(
		ctx context.Context, d driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error) {
		if refused.CompareAndSwap(0, 1) {
			return nil, &graph.GraphError{StatusCode: http.StatusPreconditionFailed, Code: "preconditionFailed", Err: graph.ErrPreconditionFailed}
		}

		return move(ctx, d, itemID, newParentID, newName, opts)
	}

	eng, syncRoot := newTestEngine(t, client)
	runScenarioPass(t, eng)
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "dir-moved")))

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, int32(1), refused.Load(), "the first move was refused")
	assert.Equal(t, map[string]string{"dir-moved": "/", "dir-moved/a.txt": "alpha\n"}, remote.tree(), "the second pass moved the folder")
	assert.Equal(t, 1, report.Superseded)
	assert.Zero(t, report.LeftForNextSync, "nothing is left for the next sync")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.1.7, R-2.8.7, R-5.6.2
//
// An overwrite is conditional on the eTag its preflight read, so a remote
// edit saved between the preflight and the upload answers 412 (D-022). That
// is a stale precondition although its error carries the 412: it is
// superseded and replanned in the same run, the replan sees both edits and
// keeps both, and no retry work is left. Classified by its 412 first, the
// attempt was recorded as transient_conflict retry work for a later pass.
func TestRunOnce_OverwriteRefusedWithA412IsReplannedInTheSameRun(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-doc", "root", "doc.txt", "first\n")
	client := remote.client(t)
	upload := client.uploadToItemFn
	var raced atomic.Int32
	client.uploadToItemFn = func(
		ctx context.Context, d driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64,
		mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if raced.CompareAndSwap(0, 1) {
			// Another device saves its edit after the preflight read the eTag.
			remote.putFileAt(t, "doc.txt", "edited online\n")
		}

		return upload(ctx, d, itemID, ifMatch, content, size, mtime, progress)
	}

	eng, syncRoot := newTestEngine(t, client)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("edited here\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, int32(1), raced.Load(), "the first overwrite met the remote edit")
	assert.Equal(t, 1, report.Superseded, "the 412 is a stale precondition")
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Zero(t, report.LeftForNextSync, "nothing is left for the next sync")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "no transient_conflict retry work")
	assert.NotContains(t, remote.retainedContents(), "edited online\n", "the remote edit was never replaced")
	remoteContents := modelContents(remote.tree())
	assert.Contains(t, remoteContents, "edited online\n")
	assert.Contains(t, remoteContents, "edited here\n")
	requireScenarioQuietPlan(t, eng)
}
