package sync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// spellingLeftoverName is the temporary sibling name a spelling-only rename
// to name moves its entry through first.
func spellingLeftoverName(name string) string {
	return syncdomain.SpellingRenameArtifactPrefix + name
}

// seedSpellingLeftoverRemote is a drive with one synced file, Docs/A.txt.
func seedSpellingLeftoverRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "Docs")
	remote.addFile(t, "item-a", "item-docs", "A.txt", "alpha\n")

	return remote
}

// requireNoRemoteSpellingLeftover fails when anything on the remote carries
// the temporary spelling-rename name: a leftover is never uploaded.
func requireNoRemoteSpellingLeftover(t *testing.T, remote *moveScenarioRemote) {
	t.Helper()

	for remotePath := range remote.tree() {
		require.NotContains(t, remotePath, syncdomain.SpellingRenameArtifactPrefix,
			"a leftover of an interrupted rename is never uploaded")
	}
}

// A crash between the two renames of a spelling-only rename leaves the synced
// file under the temporary sibling name, which observation never sees. The
// next run puts it back under its plain name before it plans, so the plan
// reads the file where it is: nothing is deleted on OneDrive, nothing is
// downloaded again, and the hidden name is gone.
//
// Validates: R-2.12.8
func TestRunOnce_SpellingRenameLeftoverIsPutBackBeforeThePlan(t *testing.T) {
	t.Parallel()

	remote := seedSpellingLeftoverRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	plain := filepath.Join(syncRoot, "Docs", "A.txt")
	leftover := filepath.Join(syncRoot, "Docs", spellingLeftoverName("A.txt"))
	require.NoError(t, os.Rename(plain, leftover))
	leftoverInfo, err := os.Stat(leftover)
	require.NoError(t, err)

	runScenarioPass(t, eng)

	assert.NoFileExists(t, leftover, "the temporary name is gone")
	plainInfo, err := os.Stat(plain)
	require.NoError(t, err, "the file is back under its plain name")
	assert.True(t, os.SameFile(leftoverInfo, plainInfo), "the entry itself is put back, not downloaded again")
	data, err := os.ReadFile(plain)
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(data))
	item, ok := remote.itemAt("Docs/A.txt")
	require.True(t, ok, "the OneDrive file is not deleted")
	assert.Equal(t, "item-a", item.ID)
	requireNoRemoteSpellingLeftover(t, remote)
	requireNoIssueOfType(t, eng, "Docs/A.txt", syncdomain.IssueLocalUnsyncableReplacement)
}

// A leftover whose plain name another entry took (the put-back of a failed
// spelling rename found the name taken) stays where it is, and so does the
// other entry: neither is deleted or replaced, the leftover is not uploaded,
// the OneDrive file stays, and the start warns, naming both paths.
//
// Validates: R-2.12.8
func TestRunOnce_SpellingRenameLeftoverWhosePlainNameIsTakenIsKeptAndReported(t *testing.T) {
	t.Parallel()

	remote := seedSpellingLeftoverRemote(t)
	logger, logs := syncfixture.NewCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
	runScenarioPass(t, eng)

	plain := filepath.Join(syncRoot, "Docs", "A.txt")
	leftover := filepath.Join(syncRoot, "Docs", spellingLeftoverName("A.txt"))
	require.NoError(t, os.Rename(plain, leftover))
	require.NoError(t, os.WriteFile(plain, []byte("other\n"), 0o600))

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	leftoverData, err := os.ReadFile(leftover)
	require.NoError(t, err, "the leftover is never deleted")
	assert.Equal(t, "alpha\n", string(leftoverData))
	plainData, err := os.ReadFile(plain)
	require.NoError(t, err)
	assert.Equal(t, "other\n", string(plainData), "the entry that took the name is never replaced")
	_, ok := remote.itemAt("Docs/A.txt")
	assert.True(t, ok, "the OneDrive file is not deleted")
	requireNoRemoteSpellingLeftover(t, remote)

	output := logs.String()
	assert.Contains(t, output, "is taken")
	assert.Contains(t, output, "leftover="+filepath.Join("Docs", spellingLeftoverName("A.txt")))
	assert.Contains(t, output, "path="+filepath.Join("Docs", "A.txt"))
}

// A dry run changes nothing on disk, a leftover of an interrupted rename
// included.
//
// Validates: R-2.12.8
func TestRunOnce_DryRunLeavesSpellingRenameLeftover(t *testing.T) {
	t.Parallel()

	remote := seedSpellingLeftoverRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	plain := filepath.Join(syncRoot, "Docs", "A.txt")
	leftover := filepath.Join(syncRoot, "Docs", spellingLeftoverName("A.txt"))
	require.NoError(t, os.Rename(plain, leftover))

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)

	assert.FileExists(t, leftover)
	assert.NoFileExists(t, plain)
	_, ok := remote.itemAt("Docs/A.txt")
	assert.True(t, ok, "the OneDrive file is not deleted")
}

// treeListing is every path under root, relative and sorted, with each
// file's contents, so a test can tell that a walk changed nothing.
func treeListing(t *testing.T, root string) []string {
	t.Helper()

	var listing []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		if d.IsDir() {
			listing = append(listing, rel+"/")
			return nil
		}
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		listing = append(listing, rel+"="+string(data))

		return nil
	}))
	slices.Sort(listing)

	return listing
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// A start that finds no leftover changes nothing, whatever else the tree
// holds, and says nothing above Debug.
//
// Validates: R-2.12.8
func TestRestoreSpellingRenameLeftovers_NoLeftoverTouchesNothing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "A.txt"), "alpha\n")
	writeTestFile(t, filepath.Join(root, "Docs", "b.txt"), "beta\n")
	writeTestFile(t, filepath.Join(root, "Docs", ".onedrive-go.spelling"), "not a leftover: no plain name\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Empty"), 0o700))
	before := treeListing(t, root)
	logger, logs := syncfixture.NewCapturedLogger()

	restoreSpellingRenameLeftovers(t.Context(), syncfixture.MustOpenSyncTree(t, root),
		syncdomain.ContentFilterConfig{}, nil, logger)

	assert.Equal(t, before, treeListing(t, root))
	assert.Zero(t, syncfixture.CountLevel(logs.String(), "INFO"))
	assert.Zero(t, syncfixture.CountLevel(logs.String(), "WARN"))
}

// A leftover file and a leftover folder, its contents inside it, are put back
// under their plain names.
//
// Validates: R-2.12.8
func TestRestoreSpellingRenameLeftovers_PutsBackFilesAndFolders(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Docs", spellingLeftoverName("Report.txt")), "report\n")
	writeTestFile(t, filepath.Join(root, spellingLeftoverName("Photos"), "one.jpg"), "one\n")

	restoreSpellingRenameLeftovers(t.Context(), syncfixture.MustOpenSyncTree(t, root),
		syncdomain.ContentFilterConfig{}, nil, syncfixture.TestLogger(t))

	assert.Equal(t, []string{
		"Docs/",
		"Docs/Report.txt=report\n",
		"Photos/",
		"Photos/one.jpg=one\n",
	}, withoutRoot(treeListing(t, root)))
}

// A leftover whose plain name the folder holds in another spelling OneDrive
// treats as the same name is kept: putting it back would make two local
// names of one OneDrive name. A leftover whose name does not say which name
// it had (a later attempt's -N suffix) is kept too. Both are warned about,
// and nothing is deleted.
//
// Validates: R-2.12.8
func TestRestoreSpellingRenameLeftovers_KeepsWhatItCannotPutBack(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "other spelling\n")
	writeTestFile(t, filepath.Join(root, spellingLeftoverName("A.txt")), "leftover\n")
	writeTestFile(t, filepath.Join(root, spellingLeftoverName("v-2")), "numbered\n")
	before := treeListing(t, root)
	logger, logs := syncfixture.NewCapturedLogger()

	restoreSpellingRenameLeftovers(t.Context(), syncfixture.MustOpenSyncTree(t, root),
		syncdomain.ContentFilterConfig{}, nil, logger)

	assert.Equal(t, before, treeListing(t, root))
	output := logs.String()
	assert.Equal(t, 2, syncfixture.CountLevel(output, "WARN"), output)
	assert.Contains(t, output, "leftover="+spellingLeftoverName("A.txt"))
	assert.Contains(t, output, "leftover="+spellingLeftoverName("v-2"))
}

// A leftover in a folder local observation does not enter (an ignored
// folder, a shared folder's own mount) is not sync's to move: it is left
// alone, and so is one whose plain name the content filter excludes.
//
// Validates: R-2.12.8
func TestRestoreSpellingRenameLeftovers_LeavesFoldersSyncDoesNotObserveAlone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Ignored", spellingLeftoverName("x.txt")), "ignored\n")
	writeTestFile(t, filepath.Join(root, "Shared", spellingLeftoverName("y.txt")), "shared\n")
	writeTestFile(t, filepath.Join(root, spellingLeftoverName("z.tmp")), "junk\n")
	before := treeListing(t, root)

	restoreSpellingRenameLeftovers(t.Context(), syncfixture.MustOpenSyncTree(t, root),
		syncdomain.ContentFilterConfig{IgnoredDirs: []string{"Ignored"}, IgnoreJunkFiles: true},
		[]syncshortcut.ProtectedRoot{{Path: "Shared"}}, syncfixture.TestLogger(t))

	assert.Equal(t, before, treeListing(t, root))
}

// withoutRoot drops the root folder's own entry from a treeListing.
func withoutRoot(listing []string) []string {
	return slices.DeleteFunc(listing, func(entry string) bool { return strings.HasPrefix(entry, "./") })
}
