package sync

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// One delta window is one set of facts, not a sequence of steps: the final
// remote_state must not depend on the order its items arrive in, and items
// that only change places must never look like OneDrive name twins (D-060).

func orderTestStore(t *testing.T) (*SyncStore, *syncLockedBuffer) {
	t.Helper()

	mgr := newTestStore(t)
	logs := &syncLockedBuffer{}
	mgr.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	return mgr, logs
}

func remoteStatePathsByID(t *testing.T, mgr *SyncStore) map[string]string {
	t.Helper()

	rows, err := mgr.ListRemoteState(t.Context())
	require.NoError(t, err)
	paths := make(map[string]string, len(rows))
	for i := range rows {
		paths[rows[i].ItemID] = rows[i].Path
	}

	return paths
}

func obsFile(id, p string) observedItem {
	return observedItem{
		DriveID: driveid.New(testDriveID), ItemID: id, Path: p,
		ItemType: ItemTypeFile, Hash: "h-" + id, Size: 1,
	}
}

func obsFolder(id, p string) observedItem {
	return observedItem{DriveID: driveid.New(testDriveID), ItemID: id, Path: p, ItemType: ItemTypeFolder}
}

func obsDeleted(id string) observedItem {
	return observedItem{DriveID: driveid.New(testDriveID), ItemID: id, IsDeleted: true}
}

func requireNoTransientTwins(t *testing.T, mgr *SyncStore, logs *syncLockedBuffer) {
	t.Helper()

	assert.Empty(t, twinIssuePaths(t, mgr), "items that only changed places are not name twins")
	assert.NotContains(t, logs.String(), "two items", "no twin warning for a window that converges")
}

// Validates: R-2.12.7
func TestCommitObservation_PathSwapInOneWindow(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mgr, logs := orderTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("a", "a.txt"), obsFile("b", "b.txt"),
		obsFolder("x", "x"), obsFile("x1", "x/one.txt"),
		obsFolder("y", "y"), obsFile("y1", "y/two.txt"),
	}, "t1", driveID))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("a", "b.txt"), obsFile("b", "a.txt"),
		obsFolder("x", "y"), obsFolder("y", "x"),
	}, "t2", driveID))

	assert.Equal(t, map[string]string{
		"a": "b.txt", "b": "a.txt",
		"x": "y", "x1": "y/one.txt",
		"y": "x", "y1": "x/two.txt",
	}, remoteStatePathsByID(t, mgr))
	requireNoTransientTwins(t, mgr, logs)
}

// Validates: R-2.12.7
func TestCommitObservation_CreateAtPathBeforeOccupantRenamedAway(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mgr, logs := orderTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("old", "doc.txt"),
	}, "t1", driveID))

	// A save-as-new-then-rename: the new item is reported at the path first,
	// the old one's move away comes later in the window (another page).
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("new", "doc.txt"),
		obsFile("old", "doc (old).txt"),
	}, "t2", driveID))

	assert.Equal(t, map[string]string{"new": "doc.txt", "old": "doc (old).txt"}, remoteStatePathsByID(t, mgr))
	requireNoTransientTwins(t, mgr, logs)
}

// Validates: R-2.12.7
func TestCommitObservation_DeleteAndRecreateSamePathAcrossPages(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mgr, logs := orderTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFolder("photos-1", "Photos"), obsFile("p1", "Photos/a.jpg"),
	}, "t1", driveID))

	// The recreated folder and its child arrive before the old folder's
	// tombstone, which only comes on a later page.
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFolder("photos-2", "Photos"),
		{DriveID: driveID, ItemID: "p2", ParentID: "photos-2", Path: "Photos/a.jpg", ItemType: ItemTypeFile, Hash: "h-p2", Size: 1},
		obsDeleted("p1"),
		obsDeleted("photos-1"),
	}, "t2", driveID))

	assert.Equal(t, map[string]string{"photos-2": "Photos", "p2": "Photos/a.jpg"}, remoteStatePathsByID(t, mgr))
	requireNoTransientTwins(t, mgr, logs)
}

// Validates: R-2.12.7
func TestCommitObservation_SubtreeRepathOntoAHeldPathKeepsBothApart(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mgr, _ := orderTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFolder("src", "src"), obsFile("s1", "src/same.txt"),
		obsFolder("dst", "dst"), obsFile("d1", "dst/same.txt"),
	}, "t1", driveID))

	// src is renamed onto dst's path while dst is not reported at all in this
	// window: two live folders now claim one path. That is a real twin, held
	// for the user, and it must not fail the commit.
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFolder("src", "dst"),
	}, "t2", driveID))

	paths := remoteStatePathsByID(t, mgr)
	assert.Equal(t, "dst", paths["dst"])
	assert.Equal(t, "dst/same.txt", paths["d1"])
	assert.Equal(t, remoteTwinShadowPath("dst", "src"), paths["src"])
	assert.Equal(t, remoteTwinShadowPath("dst", "src")+"/same.txt", paths["s1"])
	assert.Equal(t, []string{"dst"}, twinIssuePaths(t, mgr))
}

// Validates: R-2.12.7
func TestCommitObservation_WindowResultIsOrderIndependent(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	const fileCount = 8
	rng := rand.New(rand.NewPCG(20261028, 1))

	for round := range 40 {
		// Start: files f0..f7 at n0..n7 plus a folder with one child.
		initial := []observedItem{obsFolder("dir", "dir"), obsFile("kid", "dir/kid.txt")}
		for i := range fileCount {
			initial = append(initial, obsFile(fmt.Sprintf("f%d", i), fmt.Sprintf("n%d", i)))
		}

		// The window: a random permutation of names among the surviving
		// files, one deleted file whose name a new item takes, and the
		// folder renamed onto a name a file gives up.
		names := rng.Perm(fileCount)
		deleted := rng.IntN(fileCount)
		var window []observedItem
		want := map[string]string{}
		for i := range fileCount {
			id := fmt.Sprintf("f%d", i)
			if i == deleted {
				window = append(window, obsDeleted(id))
				continue
			}
			target := fmt.Sprintf("n%d", names[i])
			if names[i] == names[deleted] {
				target = "dir"
			}
			window = append(window, obsFile(id, target))
			want[id] = target
		}
		window = append(window, obsFile("newcomer", fmt.Sprintf("n%d", names[deleted])))
		want["newcomer"] = fmt.Sprintf("n%d", names[deleted])
		freed := fmt.Sprintf("folder-%d", round)
		window = append(window, obsFolder("dir", freed))
		want["dir"] = freed
		want["kid"] = freed + "/kid.txt"
		rng.Shuffle(len(window), func(i, j int) { window[i], window[j] = window[j], window[i] })

		mgr, logs := orderTestStore(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), initial, "t1", driveID))
		require.NoError(t, mgr.CommitObservation(t.Context(), window, "t2", driveID), "round %d: %s", round, describeWindow(window))

		assert.Equal(t, want, remoteStatePathsByID(t, mgr), "round %d: %s", round, describeWindow(window))
		requireNoTransientTwins(t, mgr, logs)
	}
}

func describeWindow(window []observedItem) string {
	parts := make([]string, 0, len(window))
	for i := range window {
		if window[i].IsDeleted {
			parts = append(parts, window[i].ItemID+"=deleted")
			continue
		}
		parts = append(parts, window[i].ItemID+"->"+window[i].Path)
	}
	sort.Strings(parts)

	return strings.Join(parts, " ")
}

// Validates: R-2.12.7
func TestCommitObservation_FolderMovedOutOfADeletedFolderKeepsItsSubtree(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	for _, tombstoneFirst := range []bool{true, false} {
		mgr, logs := orderTestStore(t)
		require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
			obsFolder("old", "old"), obsFolder("keep", "old/keep"), obsFile("g", "old/keep/g.txt"),
		}, "t1", driveID))

		// Delta repeats neither the moved folder's children nor, here, any
		// order: when the tombstone was applied first, its subtree delete took
		// the moved folder's unreported children with it, and those files then
		// read as deleted on OneDrive.
		window := []observedItem{obsDeleted("old"), obsFolder("keep", "keep")}
		if !tombstoneFirst {
			window[0], window[1] = window[1], window[0]
		}
		require.NoError(t, mgr.CommitObservation(t.Context(), window, "t2", driveID))

		assert.Equal(t, map[string]string{"keep": "keep", "g": "keep/g.txt"}, remoteStatePathsByID(t, mgr),
			"tombstone first: %v", tombstoneFirst)
		requireNoTransientTwins(t, mgr, logs)
	}
}

// Validates: R-2.12.7
func TestCommitObservation_RepeatedItemInOneWindowUsesItsLastObservation(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mgr, logs := orderTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("a", "a.txt"), obsFile("b", "b.txt"),
	}, "t1", driveID))

	// a is renamed twice within the window; b takes a's first name between
	// the two reports.
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		obsFile("a", "b.txt"), obsFile("b", "a.txt"), obsFile("a", "c.txt"),
	}, "t2", driveID))

	assert.Equal(t, map[string]string{"a": "c.txt", "b": "a.txt"}, remoteStatePathsByID(t, mgr))
	requireNoTransientTwins(t, mgr, logs)
}
