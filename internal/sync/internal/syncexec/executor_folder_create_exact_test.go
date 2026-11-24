package syncexec

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// folderCreateRemoteMtime is the remote folder time a local folder create
// sets on the folder it makes.
func folderCreateRemoteMtime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// caseInsensitiveFolderCreateExecutor is an executor whose sync tree sits on
// the emulated case-insensitive volume, with the folder it sits on.
func caseInsensitiveFolderCreateExecutor(t *testing.T) (*Executor, string) {
	t.Helper()

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	e.syncTree = e.syncTree.Interpose(synctest.CaseInsensitive)

	return e, syncRoot
}

func localFolderCreate(path string) *syncdomain.Action {
	return &syncdomain.Action{
		Type:       syncdomain.ActionFolderCreate,
		Path:       path,
		CreateSide: syncdomain.CreateLocal,
		View: &syncdomain.PathView{
			Path:   path,
			Remote: &syncdomain.RemoteState{ItemID: "folder-" + path, ItemType: syncdomain.ItemTypeFolder, Mtime: folderCreateRemoteMtime().UnixNano()},
		},
	}
}

// A user's folder x2 appeared between the plan and the dispatch of a create
// of X2 (watch mode), on a volume that folds case. MkdirAll found x2 and
// called it done: the remote X2's baseline was bound to the user's x2, and
// X2's content would have been downloaded into it. The create adopts nothing
// it did not make: it is a stale precondition, so the plan re-observes, and
// x2 is left exactly as it was.
//
// Validates: R-2.12.10
func TestExecutor_CreateLocalFolder_CaseVariantOfAnotherFolderIsNotAdopted(t *testing.T) {
	t.Parallel()

	e, syncRoot := caseInsensitiveFolderCreateExecutor(t)
	userFolder := filepath.Join(syncRoot, "x2")
	require.NoError(t, os.Mkdir(userFolder, 0o700))
	before, err := os.Stat(userFolder)
	require.NoError(t, err)

	o := e.ExecuteFolderCreate(t.Context(), localFolderCreate("X2"))

	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	require.ErrorIs(t, o.Error, synctree.ErrTargetExists)
	assert.Equal(t, []string{"x2"}, syncfixture.LocalNames(t, syncRoot))
	after, err := os.Stat(userFolder)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "the user's folder gets no time of the remote one")
	entries, err := os.ReadDir(userFolder)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// A create that crashed after its mkdir and before its outcome was recorded
// left the folder under its exact name: the next run's create adopts it.
//
// Validates: R-2.5.1, R-2.12.10
func TestExecutor_CreateLocalFolder_AdoptsTheFolderACrashedCreateLeft(t *testing.T) {
	t.Parallel()

	e, syncRoot := caseInsensitiveFolderCreateExecutor(t)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "docs", "X2"), 0o700))

	o := e.ExecuteFolderCreate(t.Context(), localFolderCreate("docs/X2"))

	syncfixture.RequireOutcomeSuccess(t, &o)
	identity, err := e.syncTree.IdentityNoFollow(filepath.Join("docs", "X2"))
	require.NoError(t, err)
	assert.Equal(t, identity.Inode, o.LocalInode)
	info, err := os.Stat(filepath.Join(syncRoot, "docs", "X2"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(folderCreateRemoteMtime()), "the adopted folder takes the remote folder's time")
}

// A folder whose name on disk is the NFD spelling of the stored NFC name is
// the folder (R-2.13.2): the create adopts it under its spelling on disk and
// makes no second one.
//
// Validates: R-2.12.10, R-2.13.2
func TestExecutor_CreateLocalFolder_AdoptsTheNFDSpellingOfItsName(t *testing.T) {
	t.Parallel()

	e, syncRoot := caseInsensitiveFolderCreateExecutor(t)
	nfd := norm.NFD.String("Café")
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, nfd), 0o700))

	o := e.ExecuteFolderCreate(t.Context(), localFolderCreate(norm.NFC.String("Café")))

	syncfixture.RequireOutcomeSuccess(t, &o)
	assert.Equal(t, []string{nfd}, syncfixture.LocalNames(t, syncRoot))
	identity, err := e.syncTree.IdentityNoFollow(nfd)
	require.NoError(t, err)
	assert.Equal(t, identity.Inode, o.LocalInode)
}

// A file at the folder's name is not the folder: the create is a stale
// precondition and the file is kept as it is.
//
// Validates: R-2.12.10
func TestExecutor_CreateLocalFolder_FileAtTheNameIsRefused(t *testing.T) {
	t.Parallel()

	e, syncRoot := caseInsensitiveFolderCreateExecutor(t)
	syncfixture.WriteExecTestFile(t, syncRoot, "x2", "keep")

	o := e.ExecuteFolderCreate(t.Context(), localFolderCreate("X2"))

	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, syncdomain.ErrActionPreconditionChanged)
	requireExecFileContent(t, syncRoot, "x2", "keep")
	assert.Equal(t, []string{"x2"}, syncfixture.LocalNames(t, syncRoot))
}
