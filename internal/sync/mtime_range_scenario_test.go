package sync

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Modification times at and beyond the ends of what Unix nanoseconds hold,
// end to end: what a download and a folder create set, what an upload records,
// and that the next pass plans nothing.

// assertPassPlannedNothing asserts a pass planned no transfer, delete, move,
// folder create, conflict copy or baseline publication.
func assertPassPlannedNothing(t *testing.T, report *Report) {
	t.Helper()

	assert.Zero(t,
		report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+
			report.FolderCreates+report.Moves+report.ConflictCopies+report.BaselineUpdates,
		"a converged drive plans nothing: %+v", report)
}

// setFileTimeAtOrPastUnixNanoEnd dates the file at path at, which is the last
// time Unix nanoseconds hold (2262) or later. os.Chtimes converts through
// UnixNano and would set a wrapped time instead, about 584 years off. Not
// every filesystem holds a date past 2262: the macOS CI runner reads a file
// dated 2300 back as the last Unix-nanosecond time. So the helper accepts any
// held time from that last one up to at, and nothing earlier.
func setFileTimeAtOrPastUnixNanoEnd(t *testing.T, path string, at time.Time) {
	t.Helper()

	stamp := syscall.Timespec{Sec: at.Unix(), Nsec: int64(at.Nanosecond())}
	require.NoError(t, syscall.UtimesNano(path, []syscall.Timespec{stamp, stamp}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	held := info.ModTime()
	t.Logf("asked for %s, the filesystem under the test holds %s", at.UTC(), held.UTC())
	require.False(t, held.Before(time.Unix(0, math.MaxInt64)), "the filesystem holds %s, before the last Unix-nanosecond time", held)
	require.False(t, held.After(at), "the filesystem holds %s, after the %s it was given", held, at)
}

// setRemoteModified dates a remote item and makes the next delta report it.
func setRemoteModified(remote *moveScenarioRemote, id string, at time.Time) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.items[id].ModifiedAt = at
	remote.touch(id)
}

// Validates: R-6.7.34
func TestRunOnce_ARemoteFileDated2300DownloadsWithTheLastStorableTime(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-far", "root", "far.txt", "from the far future\n")
	setRemoteModified(remote, "item-far", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	info, err := os.Stat(filepath.Join(syncRoot, "far.txt"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(time.Unix(0, math.MaxInt64)),
		"the local copy carries the last storable time, not %s", info.ModTime())

	setRemoteModified(remote, "item-far", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))
	assertPassPlannedNothing(t, runScenarioPass(t, eng))
}

// Validates: R-6.7.34
func TestRunOnce_ARemoteFileDated1601DownloadsWithTheFirstStorableTime(t *testing.T) {
	t.Parallel()

	// 1601-01-01 is the Windows file-time epoch, which real items carry.
	windowsEpoch := time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-old", "root", "old.txt", "from long ago\n")
	setRemoteModified(remote, "item-old", windowsEpoch)

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	info, err := os.Stat(filepath.Join(syncRoot, "old.txt"))
	require.NoError(t, err)
	t.Logf("the filesystem under the test dated the local copy %s", info.ModTime().UTC())
	// The first storable time, or the filesystem's earliest when it holds
	// none that early; never the date about 584 years later a wrapped value
	// would set.
	assert.True(t, info.ModTime().Before(time.Unix(0, 0)), "the local copy is dated before 1970, not %s", info.ModTime())
	assert.False(t, info.ModTime().Before(time.Unix(0, math.MinInt64)), "the local copy is not dated before 1677: %s", info.ModTime())
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath("old.txt")
	require.True(t, ok)
	assert.Equal(t, info.ModTime().UnixNano(), entry.LocalMtime, "the outcome records what the filesystem holds")

	setRemoteModified(remote, "item-old", windowsEpoch)
	assertPassPlannedNothing(t, runScenarioPass(t, eng))
}

// Validates: R-6.7.34
func TestRunOnce_ALocalFolderForARemoteFolderDated2300GetsTheLastStorableTime(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-far", "root", "far")
	setRemoteModified(remote, "item-far", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC))

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	info, err := os.Stat(filepath.Join(syncRoot, "far"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(time.Unix(0, math.MaxInt64)),
		"the folder carries the time a file dated the same would, not %s", info.ModTime())
}

// Validates: R-6.7.34
func TestRunOnce_ALocalFileDated2300UploadsAndConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "2300", at: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
		// The time a filesystem that cannot hold 2300 keeps for it, set
		// directly so that every platform runs this case too.
		{name: "the last storable time", at: time.Unix(0, math.MaxInt64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			client := remote.client(t)
			// The one-request create, which carries the modification time;
			// the remote keeps the time it was sent, as OneDrive keeps
			// fileSystemInfo.
			client.uploadCreatingFn = nil
			client.uploadFn = func(
				_ context.Context, _ driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, _ graph.ProgressFunc,
			) (*graph.Item, error) {
				item, err := remote.upload(t, parentID, name, content, size)
				if err != nil {
					return nil, err
				}
				remote.mu.Lock()
				remote.items[item.ID].ModifiedAt = mtime
				remote.mu.Unlock()
				item.ModifiedAt = mtime

				return item, nil
			}

			eng, syncRoot := newTestEngine(t, client)
			writeLocalFile(t, syncRoot, "far.txt", "to the far future\n")
			setFileTimeAtOrPastUnixNanoEnd(t, filepath.Join(syncRoot, "far.txt"), tc.at)
			report := runScenarioPass(t, eng)
			require.Equal(t, 1, report.Uploads)

			bl, err := eng.baseline.Load(t.Context())
			require.NoError(t, err)
			entry, ok := bl.GetByPath("far.txt")
			require.True(t, ok)
			assert.Equal(t, int64(math.MaxInt64), entry.RemoteMtime, "the upload records the remote time saturated")

			// The next observation of the item stores the same value: a
			// different one would be metadata drift and a baseline update.
			item, ok := remote.itemAt("far.txt")
			require.True(t, ok)
			setRemoteModified(remote, item.ID, tc.at)
			assertPassPlannedNothing(t, runScenarioPass(t, eng))
		})
	}
}

// A synced file re-dated to exactly the epoch, on either side, is published
// once: the epoch's 0 is also "no time", and the publication records the
// observed time, not the baseline's.
//
// Validates: R-2.2.3
func TestRunOnce_AFileRedatedToTheEpochConverges(t *testing.T) {
	t.Parallel()

	epoch := time.Unix(0, 0)
	for _, tc := range []struct {
		side   string
		redate func(t *testing.T, remote *moveScenarioRemote, syncRoot string)
	}{
		{side: "local", redate: func(t *testing.T, _ *moveScenarioRemote, syncRoot string) {
			t.Helper()
			require.NoError(t, os.Chtimes(filepath.Join(syncRoot, "a.txt"), epoch, epoch))
		}},
		{side: "remote", redate: func(t *testing.T, remote *moveScenarioRemote, _ string) {
			t.Helper()
			setRemoteModified(remote, "item-a", epoch)
		}},
	} {
		t.Run(tc.side, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "item-a", "root", "a.txt", "a\n")
			eng, syncRoot := newTestEngine(t, remote.client(t))
			runScenarioPass(t, eng)

			tc.redate(t, remote, syncRoot)
			report := runScenarioPass(t, eng)
			assert.Equal(t, 1, report.BaselineUpdates, "the new time is published")
			assert.Zero(t, report.Uploads+report.Downloads)

			assertPassPlannedNothing(t, runScenarioPass(t, eng))
		})
	}
}

// Validates: R-6.7.34
func TestRunOnce_ARemoteFileDatedTheEpochKeepsItsTimeLocally(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-a", "root", "a.txt", "a\n")
	setRemoteModified(remote, "item-a", time.Unix(0, 0))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	info, err := os.Stat(filepath.Join(syncRoot, "a.txt"))
	require.NoError(t, err)
	sinceEpoch := info.ModTime().Sub(time.Unix(0, 0))
	assert.True(t, sinceEpoch >= 0 && sinceEpoch < time.Second,
		"the local copy keeps the epoch, not the download time: %s", info.ModTime())
	assertPassPlannedNothing(t, runScenarioPass(t, eng))
}
