package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"golang.org/x/sys/unix"
)

// rootSafetyPaths is one drive's on-disk layout, kept stable across engine
// restarts so a test can change the sync folder between them the way an
// unmount, a wiped tmpfs or a re-pointed symlink would.
type rootSafetyPaths struct {
	dbPath   string
	dataDir  string
	syncRoot string
}

func newRootSafetyPaths(t *testing.T) rootSafetyPaths {
	t.Helper()

	tmpDir := t.TempDir()
	paths := rootSafetyPaths{
		dbPath:   filepath.Join(tmpDir, "state.db"),
		dataDir:  filepath.Join(tmpDir, "data"),
		syncRoot: filepath.Join(tmpDir, "sync"),
	}
	require.NoError(t, os.MkdirAll(paths.dataDir, 0o750))
	require.NoError(t, os.MkdirAll(paths.syncRoot, 0o750))

	return paths
}

// openRootSafetyEngine opens a primary engine over paths. It returns the
// construction error instead of failing, because refusing to start is the
// behavior several tests pin.
func openRootSafetyEngine(t *testing.T, paths rootSafetyPaths, mock *engineMockClient) (*testEngine, error) {
	t.Helper()

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:          paths.dbPath,
		SyncRoot:        paths.syncRoot,
		DataDir:         paths.dataDir,
		DriveID:         driveid.New(engineTestDriveID),
		AccountEmail:    engineTestEmail,
		Fetcher:         mock,
		SocketIOFetcher: mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		PathConvergence: &enginePathConvergenceStub{},
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          testLogger(t),
	})
	if err != nil {
		return nil, err
	}
	eng.assertInvariants = true

	return newFlowBackedTestEngine(eng), nil
}

func closeRootSafetyEngine(t *testing.T, eng *testEngine) {
	t.Helper()
	require.NoError(t, eng.Close(context.WithoutCancel(t.Context())))
}

func seedRootSafetyRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "Docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "item-docs", "b.txt", "bravo\n")
	remote.addFile(t, "item-c", "root", "c.txt", "charlie\n")

	return remote
}

// Validates: R-6.2.12
func TestNewEngine_RecordsSyncRootIdentityWithFirstCommittedSnapshot(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)

	_, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	state, err := eng.baseline.ReadObservationState(t.Context())
	require.NoError(t, err)
	assert.Nil(t, state.SyncRootIdentity, "a dry run must not record the sync folder identity")

	runScenarioPass(t, eng)
	state, err = eng.baseline.ReadObservationState(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state.SyncRootIdentity, "the first committed local snapshot records the sync folder identity")

	actual, err := mustOpenSyncTree(t, paths.syncRoot).RootIdentity()
	require.NoError(t, err)
	assert.Equal(t, actual, *state.SyncRootIdentity)
	closeRootSafetyEngine(t, eng)

	reopened, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err, "the same folder must still be accepted")
	closeRootSafetyEngine(t, reopened)
}

// Validates: R-6.2.2, R-6.2.12
func TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(paths.syncRoot, "Docs", "a.txt"))
	closeRootSafetyEngine(t, eng)

	// The disk is unmounted: what is left at the sync path is the empty
	// mount-point directory, a different inode on a different filesystem.
	require.NoError(t, os.Rename(paths.syncRoot, paths.syncRoot+".mounted"))
	require.NoError(t, os.Mkdir(paths.syncRoot, 0o750))

	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMountRootUnavailable)
	assert.Contains(t, err.Error(), "drive rebind-root", "the refusal names the explicit way out")
	assert.Empty(t, remote.deletes, "an unmounted sync folder must not delete anything online")
}

// Validates: R-6.2.12
func TestEngine_SymlinkedSyncDirRepointedIsRefused(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	target := paths.syncRoot + ".target"
	require.NoError(t, os.Rename(paths.syncRoot, target))
	require.NoError(t, os.Symlink(target, paths.syncRoot))
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	assert.Equal(t, target, eng.syncTree.Path(), "a symlinked sync folder is opened on its target, resolved once")
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(target, "c.txt"))
	closeRootSafetyEngine(t, eng)

	empty := paths.syncRoot + ".empty"
	require.NoError(t, os.Mkdir(empty, 0o750))
	require.NoError(t, os.Remove(paths.syncRoot))
	require.NoError(t, os.Symlink(empty, paths.syncRoot))

	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.ErrorIs(t, err, ErrMountRootUnavailable)
	assert.Empty(t, remote.deletes)
}

// Validates: R-6.2.12
func TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	t.Cleanup(func() { closeRootSafetyEngine(t, eng) })
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(paths.syncRoot, paths.syncRoot+".old"))
	require.NoError(t, os.Mkdir(paths.syncRoot, 0o750))

	_, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.ErrorIs(t, err, ErrMountRootUnavailable)
	assert.Empty(t, remote.deletes)
}

// Validates: R-6.2.12
func TestClearSyncRootIdentity_RebindAcceptsReplacedFolder(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)

	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	require.NoError(t, eng.baseline.ClearSyncRootIdentity(t.Context()))
	closeRootSafetyEngine(t, eng)

	// The user restored the folder from a backup on purpose: same content,
	// new directory.
	require.NoError(t, os.Rename(paths.syncRoot, paths.syncRoot+".old"))
	require.NoError(t, os.MkdirAll(filepath.Join(paths.syncRoot, "Docs"), 0o750))
	for _, rel := range []string{"Docs/a.txt", "Docs/b.txt", "c.txt"} {
		data, readErr := os.ReadFile(filepath.Join(paths.syncRoot+".old", rel)) //nolint:gosec // test reads its own temp tree
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(paths.syncRoot, rel), data, 0o600)) //nolint:gosec // test writes its own temp tree
	}

	rebound, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err, "a cleared binding accepts the folder that is there now")
	closeRootSafetyEngine(t, rebound)
}

// Validates: R-6.2.13
func TestFullScan_RootReadDirEIOReturnsError(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	tree := mustOpenSyncTree(t, syncRoot)
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	var (
		events  []changeEvent
		jobs    []hashJob
		skipped []skippedItem
	)
	walkFn := obs.makeWalkFunc(
		t.Context(), tree, map[string]bool{}, map[string]localStateRow{},
		&events, &jobs, &skipped, new(atomic.Int64), 0, map[string]struct{}{},
	)

	err := walkFn(syncRoot, nil, &fs.PathError{Op: "readdirent", Path: syncRoot, Err: errors.New("input/output error")})
	require.ErrorIs(t, err, ErrMountRootUnavailable, "an unreadable sync folder stops the scan")
	assert.Empty(t, skipped, "no read boundary at \".\" that would match no path")
}

// Validates: R-6.2.13
func TestScopePathMatches_RootBoundaryCoversEverything(t *testing.T) {
	t.Parallel()

	assert.True(t, scopePathMatches("Docs/a.txt", "."))
	assert.True(t, scopePathMatches("c.txt", ""))
	assert.False(t, scopePathMatches("Docs2/a.txt", "Docs"))
}

// Validates: R-6.2.12
func TestNewEngine_RootRecreatedAtSamePathIsRefusedEvenIfInodeIsReused(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)
	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	closeRootSafetyEngine(t, eng)

	// rm -rf and mkdir: the inode may well come back, the token does not.
	require.NoError(t, os.RemoveAll(paths.syncRoot))
	require.NoError(t, os.Mkdir(paths.syncRoot, 0o750))

	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.ErrorIs(t, err, ErrMountRootUnavailable)
	assert.Empty(t, remote.deletes)
}

// Validates: R-6.2.12
func TestNewEngine_RootTokenDecidesOverInode(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)
	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	closeRootSafetyEngine(t, eng)

	token := make([]byte, 256)
	n, err := unix.Getxattr(paths.syncRoot, synctree.RootMarkAttribute, token)
	require.NoError(t, err, "binding writes the token on the folder")
	token = token[:n]

	// The same folder brought back by a copy that kept its extended
	// attributes (or a FUSE/SMB mount whose inodes churn): new inode, same
	// token, accepted.
	require.NoError(t, os.Rename(paths.syncRoot, paths.syncRoot+".old"))
	require.NoError(t, os.Mkdir(paths.syncRoot, 0o750))
	require.NoError(t, unix.Setxattr(paths.syncRoot, synctree.RootMarkAttribute, token, 0))
	reopened, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err, "a matching token is the same sync folder")
	closeRootSafetyEngine(t, reopened)

	// A folder carrying some other folder's token is refused.
	require.NoError(t, unix.Setxattr(paths.syncRoot, synctree.RootMarkAttribute, []byte("another-folder"), 0))
	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.ErrorIs(t, err, ErrMountRootUnavailable)
}

// Validates: R-6.2.12
func TestNewEngine_UnboundStateWithSyncedItemsIsRefusedUntilRebind(t *testing.T) {
	t.Parallel()

	paths := newRootSafetyPaths(t)
	remote := seedRootSafetyRemote(t)
	eng, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err)
	runScenarioPass(t, eng)
	// Simulate a state that holds synced items but never recorded its folder.
	_, err = eng.baseline.rawDB().ExecContext(t.Context(), `UPDATE observation_state SET sync_root_bound = 0`)
	require.NoError(t, err)
	closeRootSafetyEngine(t, eng)

	_, err = openRootSafetyEngine(t, paths, remote.client(t))
	require.ErrorIs(t, err, ErrMountRootUnavailable, "binding silently would accept an unmounted folder")
	assert.Contains(t, err.Error(), "drive rebind-root")

	require.NoError(t, RebindSyncRoot(t.Context(), paths.dbPath, testLogger(t)))
	rebound, err := openRootSafetyEngine(t, paths, remote.client(t))
	require.NoError(t, err, "an explicit rebind allows binding the folder that is there")
	runScenarioPass(t, rebound)
	state, err := rebound.baseline.ReadObservationState(t.Context())
	require.NoError(t, err)
	require.NotNil(t, state.SyncRootIdentity)
	assert.False(t, state.SyncRootRebind, "binding consumes the rebind")
	closeRootSafetyEngine(t, rebound)
}
