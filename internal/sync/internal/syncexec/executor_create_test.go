package syncexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
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

// Validates: R-5.6.14
func TestExecutor_Upload_CreateConflictIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	// A create never replaces, so a 409 means OneDrive already holds the
	// name (here another device's file of the same spelling): replan from
	// fresh truth instead of retrying the same create.
	ul := &syncfixture.ExecutorMockUploader{
		UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
	}
	items := &syncfixture.ExecutorMockItemClient{
		ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "theirs", Name: "x.docx", Size: int64(len("theirs"))}}, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "x.docx", "mine")

	o := e.ExecuteUpload(t.Context(), &syncdomain.Action{
		Type: syncdomain.ActionUpload, Path: "x.docx", DriveID: driveid.New(synctest.TestDriveID), View: &syncdomain.PathView{Path: "x.docx"},
	})
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability, "OneDrive's answer proved the remote side moved on")
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteIsConditionalOnThePreflightETag(t *testing.T) {
	t.Parallel()

	var sentIfMatch []string
	ul := &syncfixture.ExecutorMockUploader{
		UploadToItemFn: func(_ context.Context, _ driveid.ID, _, ifMatch string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			sentIfMatch = append(sentIfMatch, ifMatch)
			return &graph.Item{ID: "known-item-id", ETag: "after"}, nil
		},
	}
	items := &syncfixture.ExecutorMockItemClient{
		GetItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			item := syncfixture.DefaultMockGetItem(driveID, itemID)
			item.ETag = "live-etag"
			return item, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &syncdomain.Action{
		Type: syncdomain.ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &syncdomain.PathView{Path: "known.txt"},
	})
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{"live-etag"}, sentIfMatch)
}

// Validates: R-5.6.2
func TestExecutor_Upload_OverwriteRaceWithARemoteEditIsAStalePrecondition(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		UploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusPreconditionFailed, Err: graph.ErrPreconditionFailed}
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "known.txt", "content")

	o := e.ExecuteUpload(t.Context(), &syncdomain.Action{
		Type: syncdomain.ActionUpload, Path: "known.txt", ItemID: "known-item-id",
		DriveID: driveid.New(synctest.TestDriveID), View: &syncdomain.PathView{Path: "known.txt"},
	})
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	assert.Equal(t, syncdomain.PermissionCapabilityRemoteWrite, o.FailureCapability,
		"a post-preflight 412 is a remote-write precondition, as for a remote delete or move")
}

// Validates: R-5.6.14
func TestExecutor_CreateRemoteFolder_ConflictAdoptsTheSameNamedFolder(t *testing.T) {
	t.Parallel()

	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{
				{ID: "file-photos", Name: "photo", IsFolder: false},
				{ID: "existing-folder", Name: "photos", IsFolder: true, ETag: "e1"},
			}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
		Type: syncdomain.ActionFolderCreate, Path: "photos", CreateSide: syncdomain.CreateRemote, View: &syncdomain.PathView{Path: "photos"},
	})
	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, "existing-folder", o.ItemID)
}

// Validates: R-5.6.14, R-2.12.2
func TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsADurableCollision(t *testing.T) {
	t.Parallel()

	// "Photos" is the same OneDrive name as "photos" but not this folder's
	// spelling: the executor does not merge them, and the user has to rename
	// one, as for an upload (RemoteNameTakenError).
	items := &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "other", Name: "Photos", IsFolder: true}}, nil
		},
	}
	cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())

	o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
		Type: syncdomain.ActionFolderCreate, Path: "photos", CreateSide: syncdomain.CreateRemote, View: &syncdomain.PathView{Path: "photos"},
	})
	syncfixture.RequireOutcomeFailure(t, &o)
	var taken *RemoteNameTakenError
	require.ErrorAs(t, o.Error, &taken)
	assert.NotErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged, "not a precondition the next plan can fix")
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

			items := &syncfixture.ExecutorMockItemClient{
				CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
					return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
				},
				ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
					return tc.children, nil
				},
			}
			cfg, _ := newTestExecutorConfig(t, items, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			e := NewExecution(cfg, syncfixture.EmptyBaseline())

			o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
				Type: syncdomain.ActionFolderCreate, Path: "K", CreateSide: syncdomain.CreateRemote, View: &syncdomain.PathView{Path: "K"},
			})
			syncfixture.RequireOutcomeFailure(t, &o)
			assert.NotEqual(t, "twin", o.ItemID)
			var taken *RemoteNameTakenError
			if !tc.taken {
				assert.NotErrorAs(t, o.Error, &taken)
				require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
				return
			}
			require.ErrorAs(t, o.Error, &taken)
			assert.Equal(t, "k", taken.remoteName)
		})
	}
}

// conflictFolderPages is how many pages a refused create's folder lists in
// pagedConflictFolder; the name sits on conflictNamePage.
const (
	conflictFolderPages = 3
	conflictNamePage    = 2
)

// pagedConflictFolder is the folder a create was refused in, served to the
// executor as Graph serves it: the read of one name (read) and a listing in
// conflictFolderPages pages, the child of the name on conflictNamePage. It
// counts the reads and the pages asked for.
type pagedConflictFolder struct {
	read         func(name string) (*graph.Item, error)
	named        graph.Item
	reads, pages atomic.Int32
}

func (f *pagedConflictFolder) itemClient() *syncfixture.ExecutorMockItemClient {
	return &syncfixture.ExecutorMockItemClient{
		CreateFolderFn: func(_ context.Context, _ driveid.ID, _, _ string) (*graph.Item, error) {
			return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
		},
		GetChildByNameFn: func(_ context.Context, _ driveid.ID, _, name string) (*graph.Item, error) {
			f.reads.Add(1)
			return f.read(name)
		},
		ListChildrenPagesFn: func(_ context.Context, _ driveid.ID, _ string, visit graph.ChildrenPageFunc) error {
			for page := 1; page <= conflictFolderPages; page++ {
				f.pages.Add(1)
				child := graph.Item{ID: fmt.Sprintf("other-%d", page), Name: fmt.Sprintf("other-%d", page)}
				if page == conflictNamePage {
					child = f.named
				}
				if !visit([]graph.Item{child}) {
					return nil
				}
			}

			return nil
		},
		ListChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return nil, errors.New("a refused create lists no whole folder")
		},
	}
}

// answers returns a read that answers item, for any name.
func answers(item *graph.Item) func(string) (*graph.Item, error) {
	return func(string) (*graph.Item, error) { return item, nil }
}

// misses returns a read that fails with err, for any name.
func misses(err error) func(string) (*graph.Item, error) {
	return func(string) (*graph.Item, error) { return nil, err }
}

// A folder create refused with 409 reads the one name first, and lists the
// parent only when that read cannot settle it, stopping at the page that
// holds the name as spelled. Every outcome stays: a folder of the name is
// adopted, a differently spelled sibling is a name collision, a file of the
// name is no folder to adopt, and a read that fails otherwise is its own
// failure.
//
// Validates: R-5.6.14, R-6.7.37
func TestExecutor_CreateRemoteFolder_ConflictReadsTheNameBeforeListing(t *testing.T) {
	t.Parallel()

	folder := graph.Item{ID: "existing-folder", Name: "photos", IsFolder: true}
	notFound := fmt.Errorf("reading: %w", graph.ErrNotFound)
	for _, tc := range []struct {
		label     string
		read      func(string) (*graph.Item, error)
		wantPages int32
		wantItem  string
		wantTaken bool
		wantStale bool
	}{
		{label: "the read answers the folder", read: answers(&folder), wantItem: "existing-folder"},
		{
			label: "the read answers another spelling", read: answers(&graph.Item{ID: "other", Name: "Photos", IsFolder: true}),
			wantTaken: true,
		},
		{label: "the read answers a file", read: answers(&graph.Item{ID: "file", Name: "photos"}), wantStale: true},
		{label: "the read finds nothing yet", read: misses(notFound), wantPages: conflictNamePage, wantItem: "existing-folder"},
		{
			label: "the read answers 400", read: misses(&graph.GraphError{StatusCode: http.StatusBadRequest, Err: graph.ErrBadRequest}),
			wantPages: conflictNamePage, wantItem: "existing-folder",
		},
		{label: "the read fails otherwise", read: misses(&graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError})},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			paged := &pagedConflictFolder{read: tc.read, named: folder}
			cfg, _ := newTestExecutorConfig(t, paged.itemClient(), &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
			e := NewExecution(cfg, syncfixture.EmptyBaseline())

			o := e.ExecuteFolderCreate(t.Context(), &syncdomain.Action{
				Type: syncdomain.ActionFolderCreate, Path: "photos", CreateSide: syncdomain.CreateRemote, View: &syncdomain.PathView{Path: "photos"},
			})
			assert.Equal(t, int32(1), paged.reads.Load(), "one read of the name")
			assert.Equal(t, tc.wantPages, paged.pages.Load(), "the listing stops at the page that holds the name")

			if tc.wantItem != "" {
				syncfixture.RequireOutcomeSuccess(t, &o)
				assert.Equal(t, tc.wantItem, o.ItemID)
				return
			}
			syncfixture.RequireOutcomeFailure(t, &o)
			var taken *RemoteNameTakenError
			assert.Equal(t, tc.wantTaken, errors.As(o.Error, &taken))
			assert.Equal(t, tc.wantStale, errors.Is(o.Error, syncdomain.ErrActionPreconditionChanged))
			if !tc.wantTaken && !tc.wantStale {
				require.ErrorIs(t, o.Error, graph.ErrServerError, "the read's own failure, not the 409")
			}
		})
	}
}

// A file create refused with 409 reads the one name first, and lists the
// folder only when that read cannot settle it, stopping at the page that
// holds the name as spelled. Every outcome stays: the same spelling is a
// stale plan, a differently spelled sibling is a name collision, a folder of
// the name is no file to claim, and a read that fails otherwise is its own
// failure.
//
// Validates: R-5.6.14, R-6.7.37
func TestExecutor_Upload_CreateConflictReadsTheNameBeforeListing(t *testing.T) {
	t.Parallel()

	theirs := graph.Item{ID: "theirs", Name: "x.docx", Size: int64(len("theirs"))}
	notFound := fmt.Errorf("reading: %w", graph.ErrNotFound)
	for _, tc := range []struct {
		label     string
		read      func(string) (*graph.Item, error)
		wantPages int32
		wantTaken bool
		wantStale bool
	}{
		{label: "the read answers the same spelling", read: answers(&theirs), wantStale: true},
		{label: "the read answers another spelling", read: answers(&graph.Item{ID: "other", Name: "X.DOCX"}), wantTaken: true},
		{label: "the read answers a folder", read: answers(&graph.Item{ID: "dir", Name: "x.docx", IsFolder: true}), wantStale: true},
		{label: "the read finds nothing yet", read: misses(notFound), wantPages: conflictNamePage, wantStale: true},
		{label: "the read fails otherwise", read: misses(&graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError})},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			ul := &syncfixture.ExecutorMockUploader{
				UploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
					return nil, &graph.GraphError{StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict}
				},
				UploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
					return nil, errors.New("a refused create writes into no item it did not make")
				},
			}
			paged := &pagedConflictFolder{read: tc.read, named: theirs}
			cfg, syncRoot := newTestExecutorConfig(t, paged.itemClient(), &syncfixture.ExecutorMockDownloader{}, ul)
			e := NewExecution(cfg, syncfixture.EmptyBaseline())
			syncfixture.WriteExecTestFile(t, syncRoot, "x.docx", "mine")

			o := e.ExecuteUpload(t.Context(), &syncdomain.Action{
				Type: syncdomain.ActionUpload, Path: "x.docx", DriveID: driveid.New(synctest.TestDriveID), View: &syncdomain.PathView{Path: "x.docx"},
			})
			syncfixture.RequireOutcomeFailure(t, &o)
			assert.Equal(t, int32(1), paged.reads.Load(), "one read of the name")
			assert.Equal(t, tc.wantPages, paged.pages.Load(), "the listing stops at the page that holds the name")

			var taken *RemoteNameTakenError
			assert.Equal(t, tc.wantTaken, errors.As(o.Error, &taken))
			assert.Equal(t, tc.wantStale, errors.Is(o.Error, syncdomain.ErrActionPreconditionChanged))
			if !tc.wantTaken && !tc.wantStale {
				require.ErrorIs(t, o.Error, graph.ErrServerError, "the read's own failure, not the 409")
			}
		})
	}
}
