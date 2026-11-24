package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// Validates: R-6.8.32
//
// An upload whose read of the local file hung and was left behind by the
// graph client (graph.ErrLocalCallAbandoned) is recorded in the store under
// local_call_abandoned, which `status`, another process, reads from the
// store: the file is listed under that condition, not as anonymous retrying
// work. It waits on the durable curve, and the upload that succeeds once the
// disk answers again clears the record.
func TestRunOnce_AnUploadWhoseLocalReadWasLeftBehindIsAConditionStatusReads(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	hung := true
	abandoned := fmt.Errorf("upload hung.bin: %w: %w: %w: no data moved for 2m0s (context canceled)",
		graph.ErrTransferStalled, graph.ErrLocalSideStalled, graph.ErrLocalCallAbandoned)
	upload, uploadCreating := client.UploadFn, client.UploadCreatingFn
	client.UploadFn = func(
		ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64,
		mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error) {
		if hung {
			return nil, abandoned
		}
		return upload(ctx, driveID, parentID, name, content, size, mtime, progress)
	}
	client.UploadCreatingFn = func(
		ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		if hung {
			return nil, abandoned
		}
		return uploadCreating(ctx, parentID, name, content, size, onCreated)
	}

	eng, syncRoot := newTestEngine(t, client)
	clock := syncfixture.NewManualClock(time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "hung.bin"), []byte("on a hung mount\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "hung.bin", rows[0].Path)
	assert.Equal(t, syncdomain.ActionUpload, rows[0].ActionType, "an upload: the read hung")
	assert.Equal(t, syncdomain.IssueLocalCallAbandoned, rows[0].ConditionType, "the store keeps what was left behind")
	assert.GreaterOrEqual(t, rows[0].NextRetryAt-clock.Now().UnixNano(), int64(durableRecheckDelay),
		"a hung disk is not tried again the next second")

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.Zero(t, snapshot.RetryingItems, "not counted as anonymous retrying work")
	groups := syncstore.ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1)
	assert.Equal(t, syncdomain.ConditionLocalCallAbandoned, groups[0].ConditionKey)
	assert.Equal(t, []string{"hung.bin"}, groups[0].Paths)

	hung = false
	clock.Advance(retryBackoffCeilingForTest)
	report, err = eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, "on a hung mount\n", remote.tree()["hung.bin"])
	rows, err = eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "the upload that succeeded clears the record")
	snapshot, err = eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, syncstore.ProjectStoredConditionGroups(&snapshot))
}
