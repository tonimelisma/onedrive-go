package driveops

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// crashedCreateRecord saves the session record a create leaves when the
// process dies mid-upload: keyed by the local path, naming the item it made.
func crashedCreateRecord(t *testing.T, store *SessionStore, driveStr, localPath, hash string, size int, itemID string) {
	t.Helper()

	require.NoError(t, store.Save(driveStr, localPath, &SessionRecord{
		SessionURL:  "https://upload.example.com/crashed-create",
		FileHash:    hash,
		FileSize:    int64(size),
		ItemID:      itemID,
		CreatedETag: `"created"`,
	}))
}

// Validates: R-5.2.4, R-5.6.15
//
// A create killed mid-upload leaves its session record under the local path.
// The next pass adopts the empty item and uploads to it by ID, under a key
// that names the item. That overwrite must resume the create's session, not
// open a fresh one and send every byte again, and it must clean the create's
// record up once it is done.
func TestUploadFileToItem_ResumesTheSessionACrashedCreateLeft(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	driveStr := driveid.New("d1").String()
	crashedCreateRecord(t, store, driveStr, localPath, hash, len(data), "item-1")

	resumed := false
	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(_ context.Context, session *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			resumed = true
			assert.Equal(t, graph.UploadURL("https://upload.example.com/crashed-create"), session.UploadURL)

			return tmUploadedItem(t, "item-1", content, size), nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	result, err := tm.UploadFileToItem(t.Context(), driveid.New("d1"), "item-1", localPath, UploadOpts{IfMatch: `"live"`})
	require.NoError(t, err)
	assert.True(t, resumed, "the overwrite must resume the create's session")
	assert.Empty(t, ul.ifMatches, "no fresh session may be opened while the create's session can be resumed")
	assert.Equal(t, "item-1", result.Item.ID)

	_, found, loadErr := store.Load(driveStr, localPath)
	require.NoError(t, loadErr)
	assert.False(t, found, "the create's record must be gone after the upload completes")
	_, found, loadErr = store.Load(driveStr, existingItemSessionKey(localPath, "item-1"))
	require.NoError(t, loadErr)
	assert.False(t, found, "the re-keyed record must be gone after the upload completes")
}

// Validates: R-5.2.4
//
// A create record under the same local path that names another item is not
// this overwrite's: it is left where it is and a fresh session is opened.
func TestUploadFileToItem_LeavesAnotherItemsCreateRecordAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	driveStr := driveid.New("d1").String()
	crashedCreateRecord(t, store, driveStr, localPath, hash, len(data), "item-other")

	resumed := false
	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			resumed = true
			return nil, assert.AnError
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item-1", Size: int64(len(data)), QuickXorHash: hash}, nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	_, err := tm.UploadFileToItem(t.Context(), driveid.New("d1"), "item-1", localPath, UploadOpts{IfMatch: `"live"`})
	require.NoError(t, err)
	assert.False(t, resumed, "another item's create session must not be resumed")
	assert.Equal(t, []string{`"live"`}, ul.ifMatches, "the overwrite opens its own session")
	assert.Empty(t, ul.canceledSessions, "another item's session is not this upload's to cancel")

	rec, found, loadErr := store.Load(driveStr, localPath)
	require.NoError(t, loadErr)
	require.True(t, found, "another item's create record stays for its own retry")
	assert.Equal(t, "item-other", rec.ItemID)
}

// Validates: R-5.2.4, R-5.6.13
//
// The create's record names this item but recorded other content: the file
// changed after the crash. The record is dropped and its server session
// canceled, like any record for different content, and a fresh session is
// opened.
func TestUploadFileToItem_DropsACreateRecordForOtherContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	driveStr := driveid.New("d1").String()
	crashedCreateRecord(t, store, driveStr, localPath, "other-content-hash", len(data), "item-1")

	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			assert.Fail(t, "a session for other content must not be resumed")
			return nil, assert.AnError
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item-1", Size: int64(len(data)), QuickXorHash: hash}, nil
		},
	}}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	_, err := tm.UploadFileToItem(t.Context(), driveid.New("d1"), "item-1", localPath, UploadOpts{IfMatch: `"live"`})
	require.NoError(t, err)
	assert.Equal(t, []string{`"live"`}, ul.ifMatches, "the overwrite opens its own session")
	assert.Equal(t, []graph.UploadURL{"https://upload.example.com/crashed-create"}, ul.canceledSessions,
		"the stale create session is canceled on the server when its record is dropped")

	_, found, loadErr := store.Load(driveStr, localPath)
	require.NoError(t, loadErr)
	assert.False(t, found, "a record for other content is dropped")
}
