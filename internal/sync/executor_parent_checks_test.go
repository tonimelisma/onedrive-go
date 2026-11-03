package sync

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func parentChecksFolderBaseline() *Baseline {
	return baselineWith(&BaselineEntry{
		Path:     "folder",
		ItemID:   "parent-id",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemType: ItemTypeFolder,
	})
}

func uploadIntoFolder(t *testing.T, cfg *executorConfig, syncRoot, name string) actionOutcome {
	t.Helper()

	writeExecTestFile(t, syncRoot, "folder/"+name, "hello "+name)
	action := &Action{
		Type:    ActionUpload,
		Path:    "folder/" + name,
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Path: "folder/" + name},
	}

	return newExecution(cfg, parentChecksFolderBaseline()).ExecuteUpload(t.Context(), action)
}

// Validates: R-2.8.10
//
// Sibling creates into one folder read the parent live once per plan: the
// first upload waits for the parent path and reads the parent item, the
// second does neither. A newly registered plan reads the parent again.
func TestExecutor_Upload_SiblingCreatesReadTheirParentOncePerPlan(t *testing.T) {
	t.Parallel()

	getItemCalls := 0
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
	}
	pathConvergence := &executorPathConvergenceStub{}
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "uploaded-" + name, ETag: "etag", QuickXorHash: "abc"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, ul, pathConvergence)

	first := uploadIntoFolder(t, cfg, syncRoot, "a.txt")
	requireOutcomeSuccess(t, &first)
	second := uploadIntoFolder(t, cfg, syncRoot, "b.txt")
	requireOutcomeSuccess(t, &second)
	assert.Equal(t, 1, getItemCalls, "the parent item is read once per plan")
	assert.Equal(t, []string{"folder"}, pathConvergence.waitCalls, "the parent path is probed once per plan")

	cfg.resetRemoteParentChecks()
	third := uploadIntoFolder(t, cfg, syncRoot, "c.txt")
	requireOutcomeSuccess(t, &third)
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
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			getItemCalls++
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
	}
	parentGone := false
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			if parentGone {
				return nil, graph.ErrNotFound
			}
			return &graph.Item{ID: "uploaded-" + name, ETag: "etag", QuickXorHash: "abc"}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfigWithPathConvergence(t, items, &executorMockDownloader{}, ul, &executorPathConvergenceStub{})

	first := uploadIntoFolder(t, cfg, syncRoot, "a.txt")
	requireOutcomeSuccess(t, &first)

	parentGone = true
	second := uploadIntoFolder(t, cfg, syncRoot, "b.txt")
	requireOutcomeFailure(t, &second)
	require.ErrorIs(t, second.Error, errActionPreconditionChanged)
	var gone *remoteItemGoneError
	require.ErrorAs(t, second.Error, &gone)
	assert.Equal(t, "parent-id", gone.itemID)
	assert.Equal(t, "parent", gone.role)
	assert.Equal(t, permissionCapabilityRemoteRead, second.FailureCapability)
	assert.Equal(t, 1, getItemCalls, "the gone parent was not read again before the create")
}

// Validates: R-2.8.10
//
// The same for a folder create: its own 404 names the parent it was
// addressed to.
func TestExecutor_CreateRemoteFolder_IntoAParentGoneMidPlanIsAParentGoneFact(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, DriveID: driveID, IsFolder: true}, nil
		},
		createFolderFn: func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
			return nil, graph.ErrNotFound
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, parentChecksFolderBaseline())

	action := &Action{
		Type:       ActionFolderCreate,
		Path:       "folder/child",
		CreateSide: CreateRemote,
		DriveID:    driveID,
		View:       &pathView{Path: "folder/child"},
	}

	o := e.ExecuteFolderCreate(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	var gone *remoteItemGoneError
	require.ErrorAs(t, o.Error, &gone)
	assert.Equal(t, "parent-id", gone.itemID)
	assert.Equal(t, permissionCapabilityRemoteRead, o.FailureCapability)
}
