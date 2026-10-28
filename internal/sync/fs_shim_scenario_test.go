package sync

import (
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Scenarios on emulated filesystems (D-094): the engine runs over the sync
// tree with a shim from internal/synctest interposed, and must classify what
// the filesystem says the way the requirements do, then converge once the
// filesystem heals.

const fsShimFileCount = 3

// shimScenarioRemote is a fake OneDrive holding fsShimFileCount files in
// folder.
func shimScenarioRemote(t *testing.T, folder string) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-folder", "root", folder)
	for i := range fsShimFileCount {
		remote.addFile(t, fmt.Sprintf("item-%d", i), "item-folder", fmt.Sprintf("f%d.txt", i), fmt.Sprintf("content %d\n", i))
	}

	return remote
}

// Validates: R-2.10.60, R-2.10.62
//
// Each errno a download meets under one folder: a full disk is one scope with
// every download blocked under it, never one generic failure per file;
// per-item conditions stay per item. Once the filesystem heals, sync
// converges. EROFS is not here: the read-only scope is confirmed by statfs
// (permissions_rdonly_*.go), which reads the real filesystem beside the
// sync tree's seam, so the shim cannot make a folder read-only.
func TestErrnoShim_DownloadFailuresAreClassifiedThenHeal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		errno syscall.Errno
		// scope is the one block scope every download must wait under; zero
		// means the failures stay per item.
		scope ScopeKey
	}{
		{name: "ENOSPC", errno: syscall.ENOSPC, scope: SKDiskLocal()},
		{name: "EFBIG", errno: syscall.EFBIG},
		{name: "EIO", errno: syscall.EIO},
		{name: "ENAMETOOLONG", errno: syscall.ENAMETOOLONG},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := shimScenarioRemote(t, "dl")
			shim := synctest.NewErrnoShim(synctest.ErrnoRule{Op: synctest.FSOpWrite, Path: "dl", Errno: tc.errno})
			eng, syncRoot := newTestEngineOnFS(t, remote.client(t), shim.Wrap)
			clock := &modelClock{}
			eng.clock = clock
			h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
			h.beginRound(t)

			report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			require.Positive(t, shim.Hits(), "the downloads met the filesystem")
			assert.Equal(t, fsShimFileCount, report.Failed, "errors: %v", report.Errors)

			rows := listRetryWorkForTest(t, eng.baseline, t.Context())
			require.Len(t, rows, fsShimFileCount)
			scopes := blockScopesByKeyForTest(t, eng)
			if tc.scope.IsZero() {
				assert.Empty(t, scopes, "a per-item condition blocks nothing else")
				for _, row := range rows {
					assert.False(t, row.Blocked, "%s", row.Path)
				}
			} else {
				assert.Contains(t, scopes, tc.scope)
				assert.Len(t, scopes, 1, "one scope, not one per file")
				for _, row := range rows {
					assert.True(t, row.Blocked, "%s waits for the scope", row.Path)
					assert.Equal(t, tc.scope, row.ScopeKey, "%s", row.Path)
				}
			}

			shim.SetRules()
			for range 3 {
				clock.advance(modelPassInterval)
				_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
				require.NoError(t, err)
			}
			assertSyncInvariants(t, h)
			assert.Equal(t, remote.tree(), harnessLocalTree(t, syncRoot))
		})
	}
}

// Validates: R-2.12.4
//
// On a case-insensitive volume a remote case-only rename is applied, leaves
// the new spelling on disk, and the next pass is quiet (D-031, on the
// emulated volume rather than a hard link).
func TestCaseInsensitiveShim_RemoteCaseOnlyRenameConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "readme.txt", "readme\n")
	eng, syncRoot := newTestEngineOnFS(t, remote.client(t), synctest.CaseInsensitive)
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	runScenarioPass(t, eng)

	h.beginRound(t)
	remote.moveAt(t, "docs/readme.txt", "docs/README.txt")
	remote.moveAt(t, "docs", "Docs")
	for range 2 {
		runScenarioPass(t, eng)
	}

	assert.Equal(t, map[string]string{"Docs": modelFolderSentinel, "Docs/README.txt": "readme\n"}, harnessLocalTree(t, syncRoot))
	assertSyncInvariants(t, h)
}

// Validates: R-2.12.1
//
// On a case-insensitive volume a remote file whose name is a case variant of
// a different local file must not overwrite it: both survive.
func TestCaseInsensitiveShim_RemoteCaseVariantOfALocalFileKeepsBoth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngineOnFS(t, remote.client(t), synctest.CaseInsensitive)
	eng.clock = &modelClock{}
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	writeLocalFile(t, syncRoot, "notes.txt", "local\n")
	h.beginRound(t)
	remote.putFileAt(t, "NOTES.txt", "remote\n")

	for range 3 {
		_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}

	assertNothingLost(t, h, harnessLocalTree(t, syncRoot))
}
