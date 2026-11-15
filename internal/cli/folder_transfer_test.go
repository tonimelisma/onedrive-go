package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// folderCommandRun is what one run of a file command printed.
type folderCommandRun struct {
	stdout, stderr string
	err            error
}

// runFileCommand runs the command at path ("onedrive-go get") under cc, in
// --json mode or not, and returns what it printed.
func runFileCommand(t *testing.T, path string, cc *CLIContext, jsonMode bool, args ...string) folderCommandRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), fakeGraphTimeout)
	defer cancel()

	return runLeafIn(ctx, t, findLeaf(t, path), cc, jsonMode, args...)
}

// runLeafIn runs the command leaf under cc and ctx, which the test may
// cancel, in --json mode or not, and returns what it printed.
func runLeafIn(
	ctx context.Context, t *testing.T, leaf *cobra.Command, cc *CLIContext, jsonMode bool, args ...string,
) folderCommandRun {
	t.Helper()

	var stdout, stderr bytes.Buffer
	cc.Flags.JSON = jsonMode
	cc.OutputWriter = &stdout
	cc.StatusWriter = &stderr
	leaf.SetContext(context.WithValue(ctx, cliContextKey{}, cc))
	leaf.SetArgs(args)
	leaf.SetOut(io.Discard)
	leaf.SetErr(io.Discard)
	err := leaf.Execute()

	return folderCommandRun{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// addFakeItem adds a file or folder under parentID of fake and returns its ID.
func addFakeItem(fake *fakeGraphDrive, parentID, name string, folder bool, content string) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	return fake.addLocked(parentID, name, folder, []byte(content)).ID
}

// fakeItemID is the ID of the item at remotePath in fake, or "" when there is
// none.
func fakeItemID(fake *fakeGraphDrive, remotePath string) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	if item := fake.byPath(remotePath); item != nil {
		return item.ID
	}

	return ""
}

// A folder get downloads the whole tree and says so in one summary line, or
// prints one --json document with an entry per file, the folders it created
// and the bytes it moved.
//
// Validates: R-1.2.1, R-1.2.4, R-6.6.33
func TestRunGet_FolderReportsEveryFileAsTextAndJSON(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	cc.Logger = slog.New(slog.DiscardHandler)
	docs := addFakeItem(fake, fakeGraphRootID, "Docs", true, "")
	addFakeItem(fake, docs, "a.txt", false, "hello")
	addFakeItem(fake, addFakeItem(fake, docs, "Sub", true, ""), "b.txt", false, "world!")

	textTarget := filepath.Join(t.TempDir(), "Docs")
	text := runFileCommand(t, getCommand, cc, false, "/Docs", textTarget)

	require.NoError(t, text.err)
	assert.Equal(t, "Downloaded 2 files, 2 folders (11 B)\n", text.stderr)
	assertFileHolds(t, filepath.Join(textTarget, "a.txt"), "hello")
	assertFileHolds(t, filepath.Join(textTarget, "Sub", "b.txt"), "world!")

	jsonTarget := filepath.Join(t.TempDir(), "Docs")
	asJSON := runFileCommand(t, getCommand, cc, true, "/Docs", jsonTarget)

	require.NoError(t, asJSON.err)
	var doc folderTransferJSONOutput[getJSONOutput]
	require.NoError(t, json.Unmarshal([]byte(asJSON.stdout), &doc))
	assert.ElementsMatch(t, []getJSONOutput{
		{Path: filepath.Join(jsonTarget, "a.txt"), Size: 5},
		{Path: filepath.Join(jsonTarget, "Sub", "b.txt"), Size: 6},
	}, doc.Files)
	assert.Equal(t, 2, doc.FoldersCreated)
	assert.Equal(t, int64(11), doc.TotalSize)
	assert.Equal(t, []string{}, doc.Errors)
}

// A remote name that is not one plain local name is one failure that names
// the reason; its siblings download and the command fails. In --json the
// failure is the one entry of errors, and the command fails the same way.
//
// Validates: R-1.2.7, R-6.6.33
func TestRunGet_FolderRefusesAnUnsafeRemoteName(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	cc.Logger = slog.New(slog.DiscardHandler)
	docs := addFakeItem(fake, fakeGraphRootID, "Docs", true, "")
	addFakeItem(fake, docs, "a/b", false, "planted")
	addFakeItem(fake, docs, "ok.txt", false, "ok")
	target := filepath.Join(t.TempDir(), "Docs")

	run := runFileCommand(t, getCommand, cc, false, "/Docs", target)

	require.EqualError(t, run.err, "1 item failed to download")
	assert.Equal(t, "Downloaded 1 file, 1 folder (2 B)\n"+
		"Failed: "+target+`: refused the remote name "a/b": it holds a path separator`+"\n", run.stderr)
	assertFileHolds(t, filepath.Join(target, "ok.txt"), "ok")
	assert.NoDirExists(t, filepath.Join(target, "a"), "nothing is written for a refused name")

	jsonTarget := filepath.Join(t.TempDir(), "Docs")
	asJSON := runFileCommand(t, getCommand, cc, true, "/Docs", jsonTarget)

	require.EqualError(t, asJSON.err, "1 item failed to download")
	var doc folderTransferJSONOutput[getJSONOutput]
	require.NoError(t, json.Unmarshal([]byte(asJSON.stdout), &doc), asJSON.stdout)
	assert.Equal(t, []getJSONOutput{{Path: filepath.Join(jsonTarget, "ok.txt"), Size: 2}}, doc.Files)
	assert.Equal(t, []string{jsonTarget + `: refused the remote name "a/b": it holds a path separator`}, doc.Errors)
	assert.NoDirExists(t, filepath.Join(jsonTarget, "a"))
}

// A folder put uploads the whole tree and says so in one summary line, or
// prints one --json document with an entry per file under its remote path;
// the folder it was asked to upload into counts among the folders.
//
// Validates: R-1.3.1, R-1.3.4, R-6.6.33
func TestRunPut_FolderReportsEveryFileAsTextAndJSON(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	cc.Logger = slog.New(slog.DiscardHandler)
	local := filepath.Join(t.TempDir(), "Backup")
	// Zero bytes: a file with content uploads through an upload session,
	// which the fake drive does not serve.
	for _, file := range []string{"a.txt", filepath.Join("Sub", "b.txt")} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(local, file)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(local, file), nil, 0o600))
	}

	text := runFileCommand(t, putCommand, cc, false, local, "/Backup")

	require.NoError(t, text.err)
	assert.Equal(t, "Uploaded 2 files, 2 folders (0 B)\n", text.stderr)
	assert.NotEmpty(t, fakeItemID(fake, "Backup/a.txt"))
	assert.NotEmpty(t, fakeItemID(fake, "Backup/Sub/b.txt"))

	asJSON := runFileCommand(t, putCommand, cc, true, local, "/Copy")

	require.NoError(t, asJSON.err)
	var doc folderTransferJSONOutput[putJSONOutput]
	require.NoError(t, json.Unmarshal([]byte(asJSON.stdout), &doc))
	assert.ElementsMatch(t, []putJSONOutput{
		{Path: "/Copy/a.txt", ID: fakeItemID(fake, "Copy/a.txt")},
		{Path: "/Copy/Sub/b.txt", ID: fakeItemID(fake, "Copy/Sub/b.txt")},
	}, doc.Files)
	assert.Equal(t, 2, doc.FoldersCreated)
	assert.Equal(t, []string{}, doc.Errors)
}

// A folder put of a tree that is already on OneDrive finds each folder and
// each file that holds a name by one read of that name, never by a listing of
// its folder, so a second upload of n files in f folders costs n + f reads
// and no listing. Each file is overwritten by ID and printed under the same
// path as before.
//
// Validates: R-1.3.1, R-1.3.4, R-1.3.8
func TestRunPut_FolderUploadedAgainListsNoFolder(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	cc.Logger = slog.New(slog.DiscardHandler)
	local := filepath.Join(t.TempDir(), "Backup")
	files := []string{"a.txt", "b.txt", "Sub/c.txt"}
	folders := []string{"Backup", "Backup/Sub"}
	writeEmptyLocalFiles(t, local, files...)
	first := runFileCommand(t, putCommand, cc, true, local, "Backup")
	require.NoError(t, first.err, first.stderr)
	want := make([]putJSONOutput, 0, len(files))
	for _, file := range files {
		want = append(want, putJSONOutput{Path: "/Backup/" + file, ID: fakeItemID(fake, "Backup/"+file)})
	}
	listingsBefore, readsBefore := fakeRequestCounts(fake)

	again := runFileCommand(t, putCommand, cc, true, local, "Backup")

	require.NoError(t, again.err, again.stderr)
	listings, reads := fakeRequestCounts(fake)
	assert.Zero(t, listings-listingsBefore, "an upload onto names that exist lists no folder")
	assert.Equal(t, len(files)+len(folders), reads-readsBefore, "one read per file and per folder")
	var doc folderTransferJSONOutput[putJSONOutput]
	require.NoError(t, json.Unmarshal([]byte(again.stdout), &doc))
	assert.ElementsMatch(t, want, doc.Files, "the same items, under the same paths")
	assert.Equal(t, []string{}, doc.Errors)
}

// fakeRequestCounts is how many listings and reads of one name fake answered.
func fakeRequestCounts(fake *fakeGraphDrive) (listings, nameReads int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	return fake.listings, fake.nameReads
}

// assertFileHolds holds the local file at path to content.
func assertFileHolds(t *testing.T, path, content string) {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

// A canceled folder transfer still prints what it did and every file it left,
// then ends with the cancellation, so a stop the user asked for exits as a
// stop and not as a failed transfer.
//
// Validates: R-6.3.6, R-1.2.1
func TestFolderTransferReport_ACanceledRunReportsThenEndsWithTheCancellation(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cc := &CLIContext{StatusWriter: &stderr}
	report := &folderTransferReport{cc: cc, verb: "Downloaded", noun: "download", keepFile: func(*driveops.TreeEvent) {
		assert.Fail(t, "a text run keeps no file entries")
	}}
	events := []driveops.TreeEvent{
		{Kind: driveops.TreeFolderCreated, LocalPath: "Docs"},
		{Kind: driveops.TreeFileDone, RelPath: "a.txt", LocalPath: "Docs/a.txt", Size: 3},
		{Kind: driveops.TreeFileFailed, RelPath: "b.txt", LocalPath: "Docs/b.txt", Err: errors.New("Docs/b.txt: context canceled")},
	}
	for i := range events {
		report.add(events[i])
	}
	canceled := context.Canceled

	err := report.finish(driveops.TreeSummary{Files: 1, Folders: 1, Failed: 1, Bytes: 3}, canceled,
		func(int, []string) error {
			assert.Fail(t, "a text run prints no document")
			return nil
		})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "Downloaded 1 file, 1 folder (3 B)\nFailed: Docs/b.txt: context canceled\n", stderr.String())
}

// The progress line and the summary count files and folders in words that
// agree with the number: one file, not one files.
//
// Validates: R-6.6.29
func TestCountOf_OneIsSingular(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0 files", countOf(0, "file"))
	assert.Equal(t, "1 file", countOf(1, "file"))
	assert.Equal(t, "2 folders", countOf(2, "folder"))
}
