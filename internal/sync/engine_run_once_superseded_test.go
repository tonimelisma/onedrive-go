package sync

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
