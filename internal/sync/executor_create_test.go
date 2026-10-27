package sync

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-5.6.14
func TestExecutor_Upload_CreateConflictIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	// A create never replaces, so a 409 means OneDrive already holds the
	// name: replan from fresh truth instead of retrying the same create.
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "x.docx", "mine")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "x.docx", DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "x.docx"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteIsConditionalOnThePreflightETag(t *testing.T) {
	t.Parallel()

	var sentIfMatch []string
	ul := &executorMockUploader{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, ifMatch string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			sentIfMatch = append(sentIfMatch, ifMatch)
			return &graph.Item{ID: "known-item-id", ETag: "after"}, nil
		},
	}
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			item := defaultMockGetItem(driveID, itemID)
			item.ETag = "live-etag"
			return item, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "known.txt"},
	})
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"live-etag"}, sentIfMatch)
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteRaceWithARemoteEditIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	ul := &executorMockUploader{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusPreconditionFailed, Err: graph.ErrPreconditionFailed}
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "known.txt"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}

// Validates: R-5.6.14
func TestExecutor_CreateRemoteFolder_ConflictAdoptsTheSameNamedFolder(t *testing.T) {
	t.Parallel()

	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{
				{ID: "file-photos", Name: "photo", IsFolder: false},
				{ID: "existing-folder", Name: "photos", IsFolder: true, ETag: "e1"},
			}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &pathView{Path: "photos"},
	})
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "existing-folder", o.ItemID)
}

// Validates: R-5.6.14
func TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsStale(t *testing.T) {
	t.Parallel()

	// "Photos" is the same OneDrive name as "photos" but not this folder's
	// spelling: the planner decides, the executor does not merge them.
	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "other", Name: "Photos", IsFolder: true}}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &pathView{Path: "photos"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}
