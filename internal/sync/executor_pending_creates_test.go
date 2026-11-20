package sync

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// newPendingCreateTestExecutor returns an executor that uploads through ul and
// records its pending creates in a real store, plus that store and the sync
// root.
func newPendingCreateTestExecutor(t *testing.T, ul driveops.Uploader, logger *slog.Logger) (*Executor, *SyncStore, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	dl := &executorMockDownloader{}
	cfg := NewExecutorConfig(&executorMockItemClient{}, dl, ul, syncTree, driveid.New(testDriveID), logger, nil)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	store := newTestStore(t)
	cfg.SetPendingCreates(store)

	return NewExecution(cfg, emptyBaseline()), store, syncRoot
}

// Validates: R-5.6.15
//
// The item exists on OneDrive from the moment the create answers, whatever
// happens to the action after. A shutdown landing between the answer and the
// record (live, 2026-11-10, e2e TestE2E_SyncWatch_PauseResume) failed the
// write on the canceled action context, so only the intent was left.
func TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled(t *testing.T) {
	t.Parallel()

	actionCtx, cancelAction := context.WithCancel(t.Context())
	defer cancelAction()
	created := &graph.Item{ID: "new-item", ETag: `"c,1"`}
	ul := &creatingTestUploader{create: func(uploadCtx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
		cancelAction() // the shutdown, right after the create answered
		if hookErr := onCreated(created); hookErr != nil {
			return nil, &graph.CreatedItemError{Item: created, Err: hookErr}
		}
		return nil, &graph.CreatedItemError{Item: created, Err: uploadCtx.Err()}
	}}
	_, logger := newDebugLogBuffer()
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	_, uploadErr := e.UploadNewFileForTest(actionCtx, &Action{Type: ActionUpload, Path: "doc.txt"},
		driveid.New(testDriveID), GraphRootID, "doc.txt", local)
	require.ErrorIs(t, uploadErr, context.Canceled)

	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new-item", pending.ItemID, "the item is recorded, not only the intent")
	assert.Equal(t, `"c,1"`, pending.CreatedETag)
}

// Validates: R-5.6.15, R-6.6.21
//
// A create that a shutdown canceled before it answered can still land on
// OneDrive after the client is gone, which is what the intent is for.
// Clearing it on the canceled context failed and warned at a clean stop; the
// intent is kept instead, quietly, and expires on its own.
func TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly(t *testing.T) {
	t.Parallel()

	actionCtx, cancelAction := context.WithCancel(t.Context())
	defer cancelAction()
	ul := &creatingTestUploader{create: func(uploadCtx context.Context, _ graph.CreatedFunc) (*graph.Item, error) {
		cancelAction() // the shutdown, while the create is in flight
		return nil, fmt.Errorf("graph: creating file: %w", uploadCtx.Err())
	}}
	logs, logger := newDebugLogBuffer()
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	_, uploadErr := e.UploadNewFileForTest(actionCtx, &Action{Type: ActionUpload, Path: "doc.txt"},
		driveid.New(testDriveID), GraphRootID, "doc.txt", local)
	require.ErrorIs(t, uploadErr, context.Canceled)
	assert.NotContains(t, logs.String(), "level=WARN", "a clean shutdown warns about nothing")

	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the intent stays for a create that may still land")
	assert.Empty(t, pending.ItemID)
}
