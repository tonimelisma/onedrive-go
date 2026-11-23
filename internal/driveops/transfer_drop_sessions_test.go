package driveops

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// failingCancelUploader is an uploader whose every session cancel fails, as
// one does when the service cannot be reached.
type failingCancelUploader struct {
	tmMockUploader
}

func (m *failingCancelUploader) CancelUploadSession(_ context.Context, session *graph.UploadSession) error {
	m.canceledSessions = append(m.canceledSessions, session.UploadURL)
	return errors.New("cancel the upload session: connection refused")
}

// seedDropSessionRecords saves the records an upload of givenUp holds under
// both of its keys (the create key and the overwrite key of item-1), and one
// of another upload, and returns the session URLs of the given-up upload.
func seedDropSessionRecords(t *testing.T, store *SessionStore, givenUp, other string) []graph.UploadURL {
	t.Helper()

	driveStr := driveid.New("d1").String()
	createURL := graph.UploadURL("https://upload.example.com/create")
	overwriteURL := graph.UploadURL("https://upload.example.com/overwrite")
	require.NoError(t, store.Save(driveStr, givenUp, &SessionRecord{SessionURL: string(createURL), FileSize: 1}))
	require.NoError(t, store.Save(driveStr, existingItemSessionKey(givenUp, "item-1"),
		&SessionRecord{SessionURL: string(overwriteURL), FileSize: 1}))
	require.NoError(t, store.Save(driveStr, other, &SessionRecord{SessionURL: "https://upload.example.com/other", FileSize: 1}))

	return []graph.UploadURL{createURL, overwriteURL}
}

// An upload sync gave up loses the record under either key it could hold, and
// each record's server session is canceled once; another upload's record
// stays, so its retry still resumes.
//
// Validates: R-5.6.13
func TestTransferManager_DropUploadSessions_DropsBothKeysAndCancelsEachOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	givenUp := filepath.Join(dir, "sync", "big.bin")
	other := filepath.Join(dir, "sync", "other.bin")
	urls := seedDropSessionRecords(t, store, givenUp, other)
	ul := &tmMockUploader{}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	dropped, err := tm.DropUploadSessions(t.Context(), []string{givenUp})
	require.NoError(t, err)

	assert.Equal(t, 2, dropped)
	assert.ElementsMatch(t, urls, ul.canceledSessions, "each session is canceled once")
	driveStr := driveid.New("d1").String()
	_, found, err := store.Load(driveStr, givenUp)
	require.NoError(t, err)
	assert.False(t, found, "the create key's record is gone")
	_, found, err = store.Load(driveStr, existingItemSessionKey(givenUp, "item-1"))
	require.NoError(t, err)
	assert.False(t, found, "the overwrite key's record is gone")
	_, found, err = store.Load(driveStr, other)
	require.NoError(t, err)
	assert.True(t, found, "an upload still wanted keeps its record")
}

// A cancel that fails still drops the record: the session expires on its own,
// and a record kept for it would only resume an upload sync gave up.
//
// Validates: R-5.6.13
func TestTransferManager_DropUploadSessions_AFailedCancelStillDropsTheRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	givenUp := filepath.Join(dir, "sync", "big.bin")
	urls := seedDropSessionRecords(t, store, givenUp, filepath.Join(dir, "sync", "other.bin"))
	ul := &failingCancelUploader{}
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	dropped, err := tm.DropUploadSessions(t.Context(), []string{givenUp})
	require.NoError(t, err)

	assert.Equal(t, 2, dropped)
	assert.ElementsMatch(t, urls, ul.canceledSessions)
	records, err := store.RecordsForLocalPaths(map[string]struct{}{givenUp: {}})
	require.NoError(t, err)
	assert.Empty(t, records)
}

// Nothing to drop is no work: no session store, or no path.
//
// Validates: R-5.6.13
func TestTransferManager_DropUploadSessions_NothingToDrop(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{}
	dropped, err := newTestTM(&tmSimpleDownloader{}, ul, nil).DropUploadSessions(t.Context(), []string{"/sync/a.bin"})
	require.NoError(t, err)
	assert.Zero(t, dropped)

	dropped, err = newTestTM(&tmSimpleDownloader{}, ul, NewSessionStore(t.TempDir(), slog.Default())).DropUploadSessions(t.Context(), nil)
	require.NoError(t, err)
	assert.Zero(t, dropped)
	assert.Empty(t, ul.canceledSessions)
}
