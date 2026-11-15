package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The folder form of put prints this document byte for byte: its field names
// and their order, a path holding &, < and > as itself, and a list with
// nothing in it as [].
//
// Validates: R-1.3.4, R-6.6.33
func TestPutFolderJSONOutput_Golden(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSONDocument(&buf, folderTransferJSONOutput[putJSONOutput]{
		Files: []putJSONOutput{
			{Path: "/Backup/R&D <old>/a.txt", ID: "item-a", Size: 10},
			{Path: "/Backup/R&D <old>/b>c.txt", ID: "item-b", Size: 20},
		},
		FoldersCreated: 3,
		TotalSize:      30,
	}.withEmptyLists()))

	assertGoldenFile(t, "put_folder_json.golden", buf.Bytes())
}

// putCommand is the command path of put, for runFileCommand.
const putCommand = "onedrive-go put"

// writeEmptyLocalFiles writes each of files, a "/"-separated path below root,
// as an empty file, creating its folders. Empty: a file with content uploads
// through an upload session, which the fake drive does not serve.
func writeEmptyLocalFiles(t *testing.T, root string, files ...string) {
	t.Helper()

	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
}

// A folder put whose local folder cannot be opened creates nothing on
// OneDrive: the local folder is opened before the remote one is made, and the
// error names the local folder.
//
// Validates: R-1.3.1
func TestRunPut_FolderThatCannotBeOpenedCreatesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	local := filepath.Join(t.TempDir(), "Backup")
	writeEmptyLocalFiles(t, local, "a.txt")
	require.NoError(t, os.Chmod(local, 0o000))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(local, 0o700)) }) //nolint:gosec // restores the private test folder

	run := runFileCommand(t, putCommand, cc, false, local, "/Backup")

	require.ErrorIs(t, run.err, os.ErrPermission)
	require.ErrorContains(t, run.err, "opening "+local+":")
	assert.Empty(t, fakeItemID(fake, "Backup"), "nothing is created on OneDrive")
}

// A canceled folder put prints its summary, every file it had handed over and
// did not upload, and the folder it stopped in before that folder was
// finished, then ends with the cancellation, which exits 130 when the user
// asked the run to stop.
//
// Validates: R-6.3.6, R-1.3.1
func TestRunPut_CanceledFolderReportsThenEndsWithTheCancellation(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	local := filepath.Join(t.TempDir(), "Backup")
	writeEmptyLocalFiles(t, local, "a.txt", "b.txt")
	ctx, cancel := context.WithTimeout(t.Context(), fakeGraphTimeout)
	defer cancel()
	// The first file created stops the run and waits until its client gives
	// up.
	fake.onFileWrite = func(r *http.Request) {
		cancel()
		<-r.Context().Done()
	}

	run := runLeafIn(ctx, t, findLeaf(t, putCommand), cc, false, local, "/Backup")

	require.ErrorIs(t, run.err, context.Canceled, run.stderr)
	assert.Equal(t, exitInterrupted, commandExitCode(run.err, true), "a stop the user asked for exits as one")
	lines := strings.Split(strings.TrimSuffix(run.stderr, "\n"), "\n")
	require.NotEmpty(t, lines, run.stderr)
	assert.Equal(t, "Uploaded 0 files, 1 folder (0 B)", lines[0])
	// A stop that lands between the two files also reports the folder as cut
	// short; one that lands after both were handed over does not.
	cutShort := "Failed: " + local + ": stopped before the folder was finished: "
	failed := slices.DeleteFunc(slices.Clone(lines[1:]), func(line string) bool {
		return strings.HasPrefix(line, cutShort)
	})
	assert.LessOrEqual(t, len(lines)-1-len(failed), 1, run.stderr)
	require.Len(t, failed, 2, run.stderr)
	slices.Sort(failed)
	for i, name := range []string{"a.txt", "b.txt"} {
		assert.True(t, strings.HasPrefix(failed[i], "Failed: "+filepath.Join(local, name)+": "), failed[i])
		assert.Contains(t, failed[i], context.Canceled.Error())
	}
}

// A folder put in which one file fails uploads the others, names the failed
// file and fails the command.
//
// Validates: R-1.3.1
func TestRunPut_FolderWithAFailingFileUploadsTheOthers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores file modes")
	}
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	local := filepath.Join(t.TempDir(), "Backup")
	writeEmptyLocalFiles(t, local, "kept.txt", "locked.txt")
	locked := filepath.Join(local, "locked.txt")
	require.NoError(t, os.Chmod(locked, 0o000))

	run := runFileCommand(t, putCommand, cc, false, local, "/Backup")

	require.EqualError(t, run.err, "1 item failed to upload")
	assert.True(t, strings.HasPrefix(run.stderr, "Uploaded 1 file, 1 folder (0 B)\nFailed: "+locked+": "), run.stderr)
	assert.Contains(t, run.stderr, "permission denied")
	assert.Equal(t, 2, strings.Count(run.stderr, "\n"), "the summary and the one failure")
	assert.NotEmpty(t, fakeItemID(fake, "Backup/kept.txt"))
	assert.Empty(t, fakeItemID(fake, "Backup/locked.txt"))
}

// A folder put follows a symbolic link the user names as the folder to
// upload. Inside the tree it reads a link as a file: a link to a regular file
// uploads that file under the link's name, and a link to a folder is one
// failed file, since the walk does not follow it.
//
// Validates: R-1.3.1
func TestRunPut_SymbolicLinksInAFolder(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	tree := filepath.Join(t.TempDir(), "tree")
	writeEmptyLocalFiles(t, tree, "a.txt", "sub/b.txt")
	require.NoError(t, os.Symlink(filepath.Join(tree, "a.txt"), filepath.Join(tree, "link.txt")))
	require.NoError(t, os.Symlink(filepath.Join(tree, "sub"), filepath.Join(tree, "linked")))
	local := filepath.Join(t.TempDir(), "Backup")
	require.NoError(t, os.Symlink(tree, local))

	run := runFileCommand(t, putCommand, cc, false, local, "/Backup")

	require.EqualError(t, run.err, "1 item failed to upload")
	failure := "Failed: " + filepath.Join(local, "linked") + ": "
	assert.True(t, strings.HasPrefix(run.stderr, "Uploaded 3 files, 2 folders (0 B)\n"+failure), run.stderr)
	assert.Contains(t, run.stderr, "not a regular file")
	assert.Equal(t, 2, strings.Count(run.stderr, "\n"), "the summary and the one failure")
	assert.NotEmpty(t, fakeItemID(fake, "Backup/link.txt"))
	assert.NotEmpty(t, fakeItemID(fake, "Backup/sub/b.txt"))
	assert.Empty(t, fakeItemID(fake, "Backup/linked"))
}

type fakeUploadParentResolver struct {
	path string
	item *graph.Item
	err  error
}

func (f *fakeUploadParentResolver) WaitPathVisible(_ context.Context, remotePath string) (*graph.Item, error) {
	f.path = remotePath
	return f.item, f.err
}

func TestResolveUploadParent_UsesVisibilityWait(t *testing.T) {
	t.Parallel()

	resolver := &fakeUploadParentResolver{
		item: &graph.Item{
			ID:       "parent-123",
			IsFolder: true,
		},
	}

	parentItem, err := resolveUploadParent(t.Context(), resolver, "/Documents/projects")
	require.NoError(t, err)
	require.NotNil(t, parentItem)
	assert.Equal(t, "/Documents/projects", resolver.path)
	assert.Equal(t, "parent-123", parentItem.ID)
}

func TestResolveUploadParent_WrapsVisibilityError(t *testing.T) {
	t.Parallel()

	resolver := &fakeUploadParentResolver{
		err: driveops.ErrPathNotVisible,
	}

	_, err := resolveUploadParent(t.Context(), resolver, "/Documents/projects")
	require.Error(t, err)
	assert.Equal(t, "/Documents/projects", resolver.path)
	require.ErrorIs(t, err, driveops.ErrPathNotVisible)
	assert.Contains(t, err.Error(), `resolving parent "/Documents/projects"`)
}
