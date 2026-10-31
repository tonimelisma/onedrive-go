package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// collisionSpellingFixture is a folder holding two case-colliding files, one
// of them with an NFD name on disk (a file copied from a Mac): "Résumé.txt"
// spelled NFD and "résumé.txt" spelled NFC. The stored path of either is NFC.
type collisionSpellingFixture struct {
	dir       string
	upperDisk string // NFD spelling on disk
	upperNFC  string // stored spelling of the same file
	lowerNFC  string // stored and on-disk spelling of the other file
	obs       *localObserver
	events    chan changeEvent
}

func newCollisionSpellingFixture(t *testing.T) *collisionSpellingFixture {
	t.Helper()

	f := &collisionSpellingFixture{
		upperDisk: norm.NFD.String("Résumé.txt"),
		upperNFC:  norm.NFC.String("Résumé.txt"),
		lowerNFC:  norm.NFC.String("résumé.txt"),
	}
	require.NotEqual(t, f.upperDisk, f.upperNFC)
	if !namesCoexist(t, f.upperDisk, f.lowerNFC) {
		t.Skip("the test filesystem folds case")
	}

	f.dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, f.upperDisk), []byte("upper\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, f.lowerNFC), []byte("lower\n"), 0o600))
	if _, err := os.Lstat(filepath.Join(f.dir, f.upperNFC)); err == nil {
		t.Skip("the test filesystem finds an NFD name by its NFC spelling")
	}

	f.obs = newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	f.events = make(chan changeEvent, 10)

	return f
}

// createLower delivers the create event for the NFC-named file, which the
// NFD-named one blocks as a case collision.
func (f *collisionSpellingFixture) createLower(t *testing.T) {
	t.Helper()

	tree := mustOpenSyncTree(t, f.dir)
	f.obs.handleCreate(t.Context(), tree, filepath.Join(f.dir, f.lowerNFC), f.lowerNFC, f.lowerNFC,
		newRecordingFsWatcher(), f.events)
	require.Empty(t, f.events, "a colliding create is held back")
}

func drainChangeEvents(events chan changeEvent) []changeEvent {
	var drained []changeEvent
	for {
		select {
		case ev := <-events:
			drained = append(drained, ev)
		default:
			return drained
		}
	}
}

// A collision peer is tracked and re-emitted under its stored path, never its
// spelling on disk: stored paths are NFC (R-2.13.1), and an event carrying the
// NFD spelling named an item the baseline and OneDrive do not know.
//
// Validates: R-2.12.2, R-2.13.2
func TestHandleDelete_ReemitsCollisionPeerUnderItsStoredPath(t *testing.T) {
	t.Parallel()

	f := newCollisionSpellingFixture(t)
	f.createLower(t)
	assert.Contains(t, f.obs.CollisionPeers[f.lowerNFC], f.upperNFC, "the peer is tracked under its stored path")

	// Resolving the collision by deleting the NFC-named file lets the
	// NFD-named survivor sync.
	require.NoError(t, os.Remove(filepath.Join(f.dir, f.lowerNFC)))
	f.obs.HandleDelete(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, f.dir),
		filepath.Join(f.dir, f.lowerNFC), f.lowerNFC, f.lowerNFC, f.events)

	var creates []changeEvent
	for _, ev := range drainChangeEvents(f.events) {
		if ev.Type == changeCreate {
			creates = append(creates, ev)
		}
	}
	require.Len(t, creates, 1, "the surviving peer is re-emitted")
	assert.Equal(t, f.upperNFC, creates[0].Path)
	assert.Equal(t, f.upperNFC, creates[0].Name)
	assert.Equal(t, int64(len("upper\n")), creates[0].Size, "the peer is stated at its spelling on disk")
}

// Validates: R-2.12.2, R-2.13.2
func TestHandleDelete_DeletingTheNFDNamedPeerReemitsTheOther(t *testing.T) {
	t.Parallel()

	f := newCollisionSpellingFixture(t)
	f.createLower(t)

	// The NFD-named file's delete event carries its stored path, as every
	// event does; the peer recorded for it must be found under that path.
	require.NoError(t, os.Remove(filepath.Join(f.dir, f.upperDisk)))
	f.obs.HandleDelete(t.Context(), newRecordingFsWatcher(), mustOpenSyncTree(t, f.dir),
		filepath.Join(f.dir, f.upperDisk), f.upperNFC, f.upperNFC, f.events)

	var creates []changeEvent
	for _, ev := range drainChangeEvents(f.events) {
		if ev.Type == changeCreate {
			creates = append(creates, ev)
		}
	}
	require.Len(t, creates, 1, "the surviving peer is re-emitted")
	assert.Equal(t, f.lowerNFC, creates[0].Path)
	assert.Empty(t, f.obs.CollisionPeers, "no peer is left tracked under a spelling no event carries")
}
