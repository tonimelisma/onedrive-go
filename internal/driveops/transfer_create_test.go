package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// tmCreatingUploader adds UploadCreating and RemoveEmptyItem to
// tmMockUploader, recording what a create did and which items the transfer
// manager asked to have removed while empty.
type tmCreatingUploader struct {
	tmMockUploader
	created       *graph.Item
	contentErr    error
	contentResult *graph.Item
	events        []string
	removedEmpty  []string
	// removeResults scripts the answers to the removal requests in order: an
	// error is a removal that failed and left the item, as one whose deletes
	// were all refused with 412 does (LI-20261110-01). Past the end of the
	// script, and for a nil entry, the item is removed.
	removeResults []error
}

func (m *tmCreatingUploader) UploadCreating(
	_ context.Context, _ driveid.ID, _, _ string,
	_ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	m.events = append(m.events, "create")
	if onCreated != nil {
		if err := onCreated(m.created); err != nil {
			return nil, &graph.CreatedItemError{Item: m.created, Err: err}
		}
	}
	m.events = append(m.events, "content")
	if m.contentErr != nil {
		return nil, &graph.CreatedItemError{Item: m.created, Err: m.contentErr}
	}

	return m.contentResult, nil
}

// RemoveEmptyItem records the request and answers it from removeResults: the
// read and the conditional delete behind it belong to the Graph client and
// are tested there.
func (m *tmCreatingUploader) RemoveEmptyItem(_ context.Context, _ driveid.ID, itemID string) (bool, error) {
	m.removedEmpty = append(m.removedEmpty, itemID)
	if n := len(m.removedEmpty); n <= len(m.removeResults) && m.removeResults[n-1] != nil {
		return false, m.removeResults[n-1]
	}

	return true, nil
}

var _ PlaceholderRemover = (*tmCreatingUploader)(nil)

func writeCreateTestFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "new.txt")
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}

// Validates: R-5.6.15
func TestUploadFile_RecordsTheCreatedItemBeforeAnyContent(t *testing.T) {
	t.Parallel()

	data := []byte("content")
	ul := &tmCreatingUploader{
		created:       &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentResult: &graph.Item{ID: "new-item", Size: int64(len(data)), QuickXorHash: tmHashBytes(data)},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, data), UploadOpts{
		OnCreated: func(itemID, createdETag string) error {
			ul.events = append(ul.events, "recorded "+itemID+" "+createdETag)
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"create", `recorded new-item "c,1"`, "content"}, ul.events)
}

// Validates: R-5.6.15
func TestUploadFile_UncertainFinalFragmentThatDidNotLandRemovesThePlaceholder(t *testing.T) {
	t.Parallel()

	data := []byte("content")
	var lookedUp []string
	ul := &tmCreatingUploader{
		created:    &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentErr: &graph.FinalFragmentError{Err: errors.New("connection reset")},
		tmMockUploader: tmMockUploader{
			getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
				lookedUp = append(lookedUp, itemID)
				// Still the empty placeholder: the content did not land.
				return &graph.Item{ID: itemID, ETag: `"c,1"`, QuickXorHash: tmHashBytes(nil)}, nil
			},
			listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
				return nil, errors.New("a create's outcome must be looked up by ID, not by name")
			},
		},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, data), UploadOpts{
		OnCreated: func(string, string) error { return nil },
	})
	require.Error(t, err)
	assert.Equal(t, []string{"new-item"}, lookedUp)
	assert.Equal(t, []string{"new-item"}, ul.removedEmpty)
}

// Validates: R-5.6.15
//
// Opening and canceling the session moves the empty item's eTag (live, e2e
// TestE2E_SyncWatch_FileModification). The delete conditional on the
// creation eTag then always failed with 412 and left the placeholder. The
// removal is asked for by item ID, so the eTag it is conditional on is read
// when it deletes, and it is asked for only while the lookup shows the item
// still empty.
func TestUploadFile_UncertainFinalFragmentRemovesThePlaceholderByItsCurrentETag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		current *graph.Item
		removed []string
	}{
		{
			name:    "still empty under a moved eTag",
			current: &graph.Item{ID: "new-item", ETag: `"c,3"`, QuickXorHash: tmHashBytes(nil)},
			removed: []string{"new-item"},
		},
		{
			name:    "someone else's content landed",
			current: &graph.Item{ID: "new-item", ETag: `"c,4"`, Size: 5, QuickXorHash: tmHashBytes([]byte("other"))},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ul := &tmCreatingUploader{
				created:    &graph.Item{ID: "new-item", ETag: `"c,1"`},
				contentErr: &graph.FinalFragmentError{Err: errors.New("connection reset")},
				tmMockUploader: tmMockUploader{
					getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
						return tc.current, nil
					},
				},
			}
			tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

			_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt",
				writeCreateTestFile(t, []byte("content")), UploadOpts{OnCreated: func(string, string) error { return nil }})
			require.Error(t, err)
			assert.Equal(t, tc.removed, ul.removedEmpty)
		})
	}
}

// Validates: R-5.6.15
//
// The removal is best effort. When it fails and leaves the empty item, as one
// whose conditional deletes were all refused with 412 did (LI-20261110-01),
// the caller gets the upload's own failure, never the cleanup's, and the
// transfer manager asks once: the bound on attempts is the Graph client's.
func TestUploadFile_FailedPlaceholderRemovalKeepsTheUploadsOwnError(t *testing.T) {
	t.Parallel()

	contentLost := errors.New("connection reset")
	removalRefused := fmt.Errorf("graph: removing empty item new-item: %w", graph.ErrPreconditionFailed)
	ul := &tmCreatingUploader{
		created:       &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentErr:    &graph.FinalFragmentError{Err: contentLost},
		removeResults: []error{removalRefused},
		tmMockUploader: tmMockUploader{
			getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
				// Still empty: the content did not land, so the item is removed.
				return &graph.Item{ID: itemID, ETag: `"c,3"`, QuickXorHash: tmHashBytes(nil)}, nil
			},
		},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, []byte("content")),
		UploadOpts{OnCreated: func(string, string) error { return nil }})

	require.ErrorIs(t, err, contentLost, "the upload's own failure")
	require.NotErrorIs(t, err, graph.ErrPreconditionFailed, "never the cleanup's")
	assert.Equal(t, []string{"new-item"}, ul.removedEmpty)
}

// Validates: R-5.6.15
//
// A large new file goes through a recorded session, which creates the empty
// item itself. When recording that item fails, nothing may be sent: the
// session is canceled and the item is handed to the bounded removal by its
// ID, which reads its current eTag itself.
func TestSessionUpload_FailedCreatedHookRemovesTheEmptyItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, _, _ := sessionTestFile(t, dir)

	const sessionURL = graph.UploadURL("https://upload.example.com/new")
	errRecord := errors.New("state database is read-only")
	ul := &tmCreatingUploader{tmMockUploader: tmMockUploader{
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: sessionURL, ItemID: "new-item", CreatedETag: `"c,1"`}, nil
		},
		uploadFromSessionFn: func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
			return nil, errors.New("no content may be sent for an item that was not recorded")
		},
		getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
			return nil, errors.New("the removal reads the item itself, the transfer manager does not")
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{
		OnCreated: func(string, string) error { return errRecord },
	})
	require.ErrorIs(t, err, errRecord)
	assert.Equal(t, []string{"new-item"}, ul.removedEmpty)
	assert.Equal(t, []graph.UploadURL{sessionURL}, ul.canceledSessions)
}

// Validates: R-5.6.15
func TestSessionUpload_RetryOfAnInterruptedCreateReusesItsItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	// A previous run created the item and recorded the session, then died;
	// the session is gone now.
	require.NoError(t, store.Save(driveid.New("d1").String(), localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/old", FileHash: hash, FileSize: int64(len(data)),
		ExpiresAt: time.Now().Add(-time.Hour), ItemID: "placeholder", CreatedETag: `"p,1"`,
	}))

	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadSessionExpired
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return &graph.Item{ID: itemID, ETag: `"p,1"`}, nil
		},
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return nil, errors.New("must not create the file again: it would 409 against its own placeholder")
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "placeholder", Size: int64(len(data)), QuickXorHash: hash}, nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	item, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "placeholder", item.Item.ID)
	assert.Equal(t, []string{`"p,1"`}, ul.ifMatches, "the fresh session goes to the existing item, conditionally")
}

// Validates: R-5.6.15
//
// A large new file is created and given its session in one Graph call. When
// the session request fails after the empty item exists (a shutdown cancels
// it: live, e2e TestE2E_SyncWatch_PauseResume), Graph names the item in a
// CreatedItemError and tries to remove it. The record hook ran only once a
// session was open, so the item went unrecorded and sync cleared its intent:
// when the removal failed too, the empty file read on the next pass as
// someone else's. The hook now runs for the item the error names, once, and
// the upload still fails with the session's error.
func TestSessionUpload_FailedSessionRecordsTheItemTheCreateMade(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, _, _ := sessionTestFile(t, dir)

	ul := &tmCreatingUploader{tmMockUploader: tmMockUploader{
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return nil, &graph.CreatedItemError{Item: &graph.Item{ID: "new-item", ETag: `"c,1"`}, Err: context.Canceled}
		},
		uploadFromSessionFn: func(context.Context, *graph.UploadSession, io.ReaderAt, int64, graph.ProgressFunc) (*graph.Item, error) {
			return nil, errors.New("there is no session to send content through")
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	var recorded []string
	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{
		OnCreated: func(itemID, createdETag string) error {
			recorded = append(recorded, itemID+" "+createdETag)
			return nil
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{`new-item "c,1"`}, recorded)
	assert.Empty(t, ul.removedEmpty, "Graph already tried to remove the item; the transfer manager does not ask again")
}

// requireOneCreatedItemError returns the one *graph.CreatedItemError in err's
// chain and fails the test when another is wrapped inside it: the fact it
// carries is read from the outermost one, and a second one would contradict it.
func requireOneCreatedItemError(t *testing.T, err error) *graph.CreatedItemError {
	t.Helper()

	var created *graph.CreatedItemError
	require.ErrorAs(t, err, &created)
	var inner *graph.CreatedItemError
	require.NotErrorAs(t, created.Err, &inner, "one created-item failure, not one wrapped in another")

	return created
}

// Validates: R-5.6.15
//
// When Graph's removal of a failed session's empty item worked, the failure
// says so, and the item is not recorded: a record naming an item Graph just
// deleted is a record no upload can ever use.
func TestRecordItemOfFailedSession_SkipsARemovedItem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		removed  bool
		recorded []string
	}{
		{name: "removed", removed: true},
		{name: "left in place", recorded: []string{`new-item "c,1"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			localPath, _, _ := sessionTestFile(t, dir)
			ul := &tmCreatingUploader{tmMockUploader: tmMockUploader{
				createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
					return nil, &graph.CreatedItemError{
						Item: &graph.Item{ID: "new-item", ETag: `"c,1"`}, Err: context.Canceled, Removed: tc.removed,
					}
				},
			}}
			tm := newTestTM(&tmSimpleDownloader{}, ul, NewSessionStore(dir, slog.Default()))

			var recorded []string
			_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{
				OnCreated: func(itemID, createdETag string) error {
					recorded = append(recorded, itemID+" "+createdETag)
					return nil
				},
			})

			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, tc.recorded, recorded)
			assert.Equal(t, tc.removed, requireOneCreatedItemError(t, err).Removed)
		})
	}
}

// Validates: R-5.6.15
//
// Both places where the transfer manager removes an empty item say whether
// the removal worked, through the one created-item failure the caller gets:
// a failed record of a session's item (the session path) and a final
// fragment that did not land (settleUncertainFinalFragment). A removal that
// failed leaves the item, which is then not reported removed.
func TestUploadFile_RemovedPlaceholderIsReportedRemoved(t *testing.T) {
	t.Parallel()

	removalRefused := fmt.Errorf("graph: removing empty item new-item: %w", graph.ErrPreconditionFailed)
	for _, tc := range []struct {
		name   string
		upload func(t *testing.T, removeResults []error) error
		refuse bool
	}{
		{name: "session item whose record failed", upload: sessionUploadWhoseRecordFails},
		{name: "session item whose record failed, removal refused", upload: sessionUploadWhoseRecordFails, refuse: true},
		{name: "final fragment that did not land", upload: uploadWhoseFinalFragmentDidNotLand},
		{name: "final fragment that did not land, removal refused", upload: uploadWhoseFinalFragmentDidNotLand, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var removeResults []error
			if tc.refuse {
				removeResults = []error{removalRefused}
			}

			err := tc.upload(t, removeResults)

			assert.Equal(t, !tc.refuse, requireOneCreatedItemError(t, err).Removed)
		})
	}
}

// sessionUploadWhoseRecordFails runs a large create whose session opens and
// whose record of the created item then fails, with the removal answering
// from removeResults, and returns the upload's error.
func sessionUploadWhoseRecordFails(t *testing.T, removeResults []error) error {
	t.Helper()

	dir := t.TempDir()
	localPath, _, _ := sessionTestFile(t, dir)
	errRecord := errors.New("state database is read-only")
	ul := &tmCreatingUploader{removeResults: removeResults, tmMockUploader: tmMockUploader{
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/new", ItemID: "new-item", CreatedETag: `"c,1"`}, nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, NewSessionStore(dir, slog.Default()))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", filepath.Base(localPath), localPath, UploadOpts{
		OnCreated: func(string, string) error { return errRecord },
	})
	require.ErrorIs(t, err, errRecord)
	assert.Equal(t, []string{"new-item"}, ul.removedEmpty)

	return err
}

// uploadWhoseFinalFragmentDidNotLand runs a create whose final fragment
// failed while the lookup shows the item still empty, with the removal
// answering from removeResults, and returns the upload's error.
func uploadWhoseFinalFragmentDidNotLand(t *testing.T, removeResults []error) error {
	t.Helper()

	contentLost := errors.New("connection reset")
	ul := &tmCreatingUploader{
		created:       &graph.Item{ID: "new-item", ETag: `"c,1"`},
		contentErr:    &graph.FinalFragmentError{Err: contentLost},
		removeResults: removeResults,
		tmMockUploader: tmMockUploader{
			getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
				return &graph.Item{ID: itemID, ETag: `"c,3"`, QuickXorHash: tmHashBytes(nil)}, nil
			},
		},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt", writeCreateTestFile(t, []byte("content")),
		UploadOpts{OnCreated: func(string, string) error { return nil }})
	require.ErrorIs(t, err, contentLost, "the upload's own failure")
	assert.Equal(t, []string{"new-item"}, ul.removedEmpty)

	return err
}

// Validates: R-5.6.15, R-5.5.2
//
// An upload that completed without returning its item is looked up by the ID
// the create made. When that lookup fails, the outcome is unknown, and the
// failure still names the created item: without the mark, a 404 there read
// in sync as "the parent folder is gone", and the folder and its subtree were
// forgotten from remote truth. The lookup's own failure stays in the chain,
// so it is classified by its own answer; without a create there is no mark,
// and the outcome is unknown all the same.
func TestSettleUploadedItem_LookupFailureKeepsTheCreateMark(t *testing.T) {
	t.Parallel()

	completed := fmt.Errorf("graph: uploading chunk at offset 0: %w", graph.ErrUploadCompletedWithoutItem)
	lookup := func(context.Context, driveid.ID, string) (*graph.Item, error) {
		return nil, graph.ErrNotFound
	}

	t.Run("created", func(t *testing.T) {
		t.Parallel()

		ul := &tmCreatingUploader{
			created:        &graph.Item{ID: "new-item", ETag: `"c,1"`},
			contentErr:     completed,
			tmMockUploader: tmMockUploader{getItemFn: lookup},
		}
		_, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt",
			writeCreateTestFile(t, []byte("content")), UploadOpts{OnCreated: func(string, string) error { return nil }})

		created := requireOneCreatedItemError(t, err)
		assert.Equal(t, "new-item", created.Item.ID)
		var unknown *uploadOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		require.ErrorIs(t, err, graph.ErrNotFound, "the lookup's failure is classified by its own answer")
		assert.NotErrorIs(t, err, graph.ErrUploadCompletedWithoutItem, "the answer is kept as text only")
	})

	t.Run("no mark", func(t *testing.T) {
		t.Parallel()

		ul := &tmMockUploader{
			uploadFn: func(context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc) (*graph.Item, error) {
				return nil, completed
			},
			listChildrenFn: func(context.Context, driveid.ID, string) ([]graph.Item, error) {
				return nil, nil
			},
		}
		_, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(t.Context(), driveid.New("d1"), "parent", "new.txt",
			writeCreateTestFile(t, []byte("content")), UploadOpts{})

		var created *graph.CreatedItemError
		require.NotErrorAs(t, err, &created)
		var unknown *uploadOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		require.ErrorIs(t, err, graph.ErrNotFound)
	})
}
