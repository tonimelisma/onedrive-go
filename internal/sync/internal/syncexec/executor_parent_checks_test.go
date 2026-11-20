package syncexec

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func parentChecksFolderBaseline() *syncdomain.Baseline {
	return syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: syncdomain.ItemTypeFolder,
	})
}

func uploadIntoFolder(t *testing.T, cfg *ExecutorConfig, syncRoot, name string) syncdomain.ActionOutcome {
	t.Helper()

	syncfixture.WriteExecTestFile(t, syncRoot, "folder/"+name, "hello "+name)
	action := &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "folder/" + name,
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "folder/" + name},
	}

	return NewExecution(cfg, parentChecksFolderBaseline()).ExecuteUpload(t.Context(), action)
}

// Validates: R-2.8.10
//
// Sibling creates into one folder read the parent live once per plan: the
// first upload waits for the parent path and reads the parent item, the
// second does neither. A newly registered plan reads the parent again.
func TestExecutor_Upload_SiblingCreatesReadTheirParentOncePerPlan(t *testing.T) {
	t.Parallel()

	getItemCalls := 0
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded-" + name, ETag: "etag", QuickXorHash: "abc"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, ul, pathConvergence)

	first := uploadIntoFolder(t, cfg, syncRoot, "a.txt")
	syncfixture.RequireOutcomeSuccess(t, &first)
	second := uploadIntoFolder(t, cfg, syncRoot, "b.txt")
	syncfixture.RequireOutcomeSuccess(t, &second)
	assert.Equal(t, 1, getItemCalls, "the parent item is read once per plan")
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent path is probed once per plan")

	cfg.ResetRemoteParentChecks()
	third := uploadIntoFolder(t, cfg, syncRoot, "c.txt")
	syncfixture.RequireOutcomeSuccess(t, &third)
	assert.Equal(t, 2, getItemCalls, "a new plan reads the parent live again")
	assert.Equal(t, []string{"folder", "folder"}, pathConvergence.waitCalls)
}

// Validates: R-2.8.10
//
// Once a sibling's create has read the parent in this plan, a later create
// that Graph answers 404 is the same parent-gone fact the live read would
// have been: a stale precondition naming the parent, so the engine forgets
// the item and replans rather than retrying the upload against nothing.
func TestExecutor_Upload_CreateIntoAParentGoneMidPlanIsAParentGoneFact(t *testing.T) {
	t.Parallel()

	getItemCalls := 0
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
	}
	parentGone := false
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			if parentGone {
				return nil, graph.ErrNotFound
			}
			return &graph.Item{ID: "uploaded-" + name, ETag: "etag", QuickXorHash: "abc"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &syncfixture.ExecutorMockDownloader{}, ul, &executorPathConvergenceStub{})

	first := uploadIntoFolder(t, cfg, syncRoot, "a.txt")
	syncfixture.RequireOutcomeSuccess(t, &first)

	parentGone = true
	second := uploadIntoFolder(t, cfg, syncRoot, "b.txt")
	syncfixture.RequireOutcomeFailure(t, &second)
	require.ErrorIs(t, second.Error, syncdomain.ErrActionPreconditionChanged)
	var gone *RemoteItemGoneError
	require.ErrorAs(t, second.Error, &gone)
	assert.Equal(t, "parent-id", gone.ItemID)
	assert.Equal(t, "parent", gone.Role)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, second.FailureCapability)
	assert.Equal(t, 1, getItemCalls, "the gone parent was not read again before the create")
}

// Validates: R-2.8.10
//
// The same for a folder create: its own 404 names the parent it was
// addressed to.
func TestExecutor_CreateRemoteFolder_IntoAParentGoneMidPlanIsAParentGoneFact(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
		CreateFolderFn: func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
			return nil, graph.ErrNotFound
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, parentChecksFolderBaseline())

	action := &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       "folder/child",
		CreateSide: syncdomain.CreateRemote,
		DriveID:    driveID,
		View:       &syncdomain.PathView{Path: "folder/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	var gone *RemoteItemGoneError
	require.ErrorAs(t, o.Error, &gone)
	assert.Equal(t, "parent-id", gone.ItemID)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteRead, o.FailureCapability)
}
