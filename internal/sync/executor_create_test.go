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
	// name (here another device's file of the same spelling): replan from
	// fresh truth instead of retrying the same create.
	ul := &executorMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
	}
	items := &executorMockItemClient{
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "theirs", Name: "x.docx", Size: int64(len("theirs"))}}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "x.docx", "mine")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "x.docx", DriveID: driveid.New(synctest.TestDriveID), View: &PathView{Path: "x.docx"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, ErrActionPreconditionChanged)
	assert.Equal(t, PermissionCapabilityRemoteWrite, o.FailureCapability, "OneDrive's answer proved the remote side moved on")
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
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &PathView{Path: "known.txt"},
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
	e := NewExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &PathView{Path: "known.txt"},
	})
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, ErrActionPreconditionChanged)
	assert.Equal(t, PermissionCapabilityRemoteWrite, o.FailureCapability,
		"a post-preflight 412 is a remote-write precondition, as for a remote delete or move")
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
	e := NewExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &PathView{Path: "photos"},
	})
	requireOutcomeSuccess(t, &o)
	assert.Equal(t, "existing-folder", o.ItemID)
}

// Validates: R-5.6.14, R-2.12.2
func TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsADurableCollision(t *testing.T) {
	t.Parallel()

	// "Photos" is the same OneDrive name as "photos" but not this folder's
	// spelling: the executor does not merge them, and the user has to rename
	// one, as for an upload (RemoteNameTakenError).
	items := &executorMockItemClient{
		createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "other", Name: "Photos", IsFolder: true}}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
	e := NewExecution(cfg, emptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &Action{
		Type: ActionFolderCreate, Path: "photos", CreateSide: CreateRemote, View: &PathView{Path: "photos"},
	})
	requireOutcomeFailure(t, &o)
	var taken *RemoteNameTakenError
	require.ErrorAs(t, o.Error, &taken)
	assert.NotErrorIs(t, o.Error, ErrActionPreconditionChanged, "not a precondition the next plan can fix")
}

// Validates: R-2.12.3, R-5.6.14
func TestClassifyCreateConflict_NeverAdoptsATwin(t *testing.T) {
	t.Parallel()

	const (
		nfc    = "caf\u00e9"  // NFC
		nfd    = "cafe\u0301" // NFD: the same OneDrive name, the same stored form
		upper  = "CAF\u00c9"  // the same OneDrive name, another stored form
		title  = "Caf\u00e9"  // another of those
		kelvin = "\u212a.txt" // KELVIN SIGN: stored as "K.txt", another OneDrive name
		plainK = "K.txt"      // the create's name in the twin rows
		lowerK = "k.txt"      // the same OneDrive name as "K.txt"
	)
	folder := func(id, name string) graph.Item { return graph.Item{ID: id, Name: name, IsFolder: true} }
	file := func(id, name string) graph.Item { return graph.Item{ID: id, Name: name} }

	tests := []struct {
		label    string
		children []graph.Item
		name     string
		wantID   string
		want     createConflictFound
	}{
		{label: "no candidate", children: []graph.Item{folder("x", "other")}, name: nfc, want: createConflictNothing},
		{label: "identical", children: []graph.Item{folder("id", nfc)}, name: nfc, wantID: "id", want: createConflictWanted},
		{label: "same stored form", children: []graph.Item{folder("id", nfd)}, name: nfc, wantID: "id", want: createConflictWanted},
		{label: "same name", children: []graph.Item{folder("id", upper)}, name: nfc, wantID: "id", want: createConflictRival},
		{label: "a twin alone", children: []graph.Item{folder("twin", kelvin)}, name: plainK, want: createConflictNothing},
		{
			label: "a twin beside a same name", children: []graph.Item{folder("twin", kelvin), folder("rival", lowerK)},
			name: plainK, wantID: "rival", want: createConflictRival,
		},
		{label: "a file of the wanted name", children: []graph.Item{file("id", nfc)}, name: nfc, wantID: "id", want: createConflictWanted},
		{
			label: "two same names name the first listed", children: []graph.Item{file("a", upper), file("b", title)},
			name: nfc, wantID: "a", want: createConflictRival,
		},
		{
			label: "the wanted name beside a same name", children: []graph.Item{folder("rival", upper), folder("id", nfc)},
			name: nfc, wantID: "id", want: createConflictWanted,
		},
		{
			label: "two of the wanted tier are nothing", children: []graph.Item{folder("a", nfd), folder("b", nfd)},
			name: nfc, want: createConflictNothing,
		},
		{label: "a child listed twice counts once", children: []graph.Item{folder("id", nfd), folder("id", nfd)}, name: nfc, wantID: "id", want: createConflictWanted},
		{label: "a child without an ID is no candidate", children: []graph.Item{folder("", nfc)}, name: nfc, want: createConflictNothing},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			t.Parallel()
			child, found := classifyCreateConflict(tt.children, tt.name)
			assert.Equal(t, tt.want, found)
			if tt.wantID == "" {
				assert.Nil(t, child)
				return
			}
			require.NotNil(t, child)
			assert.Equal(t, tt.wantID, child.ID)
		})
	}
}

// A folder whose stored form only matches another OneDrive name (the Kelvin
// sign for K) is not the folder the create wanted: beside a sibling of the
// create's own OneDrive name the name is taken, naming that sibling, and
// alone it is a stale precondition. Neither is adopted.
//
// Validates: R-2.12.3, R-5.6.14
func TestExecutor_CreateRemoteFolder_ConflictBesideATwinAdoptsNothing(t *testing.T) {
	t.Parallel()

	twin := graph.Item{ID: "twin", Name: "\u212a", IsFolder: true} // KELVIN SIGN, stored as "K"
	rival := graph.Item{ID: "rival", Name: "k", IsFolder: true}

	for _, tc := range []struct {
		label    string
		children []graph.Item
		taken    bool
	}{
		{label: "twin and key-equal sibling", children: []graph.Item{twin, rival}, taken: true},
		{label: "twin alone", children: []graph.Item{twin}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			items := &executorMockItemClient{
				createFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
					return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
				},
				listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
					return tc.children, nil
				},
			}
			cfg, _ := newTestExecutorConfig(t, items, &executorMockDownloader{}, &executorMockUploader{})
			e := NewExecution(cfg, emptyBaseline())

			o := e.ExecuteFolderCreate(t.Context(), &Action{
				Type: ActionFolderCreate, Path: "K", CreateSide: CreateRemote, View: &PathView{Path: "K"},
			})
			requireOutcomeFailure(t, &o)
			assert.NotEqual(t, "twin", o.ItemID)
			var taken *RemoteNameTakenError
			if !tc.taken {
				assert.NotErrorAs(t, o.Error, &taken)
				require.ErrorIs(t, o.Error, ErrActionPreconditionChanged)
				return
			}
			require.ErrorAs(t, o.Error, &taken)
			assert.Equal(t, "k", taken.remoteName)
		})
	}
}
