package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// getCommand is the command path of get, for runFileCommand.
const getCommand = "onedrive-go get"

// reportFakeName makes fake report name for the item id, which a path still
// finds by its own name.
func reportFakeName(fake *fakeGraphDrive, id, name string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()

	fake.reportedNames[id] = name
}

// assertLocalDirHolds holds the local folder dir to exactly names.
func assertLocalDirHolds(t *testing.T, dir string, names ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		held = append(held, entry.Name())
	}
	assert.ElementsMatch(t, names, held, "the entries of %s", dir)
}

// assertRefusedName holds err to the refusal of the remote name name, which
// tells the user what to name instead.
func assertRefusedName(t *testing.T, err error, name, instead string) {
	t.Helper()

	var refused *driveops.RefusedNameError
	require.ErrorAs(t, err, &refused, "the remote name %q", name)
	assert.Equal(t, name, refused.Name)
	assert.Contains(t, err.Error(), instead)
}

// A get of the drive root with no local path takes the root's remote name,
// which another account chose for a mounted folder, as the local folder: a
// plain local name becomes the folder, and any other is refused before
// anything is listed or written.
//
// Validates: R-1.2.7, R-1.2.1
func TestRunGet_DriveRootWithNoLocalPathTakesTheRootsName(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	addFakeItem(fake, fakeGraphRootID, "a.txt", false, "hello")
	workDir := t.TempDir()
	t.Chdir(workDir)

	reportFakeName(fake, fakeGraphRootID, "..")
	refused := runFileCommand(t, getCommand, cc, false, "/")

	assertRefusedName(t, refused.err, "..", "name the local folder to download into")
	assertLocalDirHolds(t, workDir)

	reportFakeName(fake, fakeGraphRootID, "Drive Root")
	named := runFileCommand(t, getCommand, cc, false, "/")

	require.NoError(t, named.err)
	assertFileHolds(t, filepath.Join(workDir, "Drive Root", "a.txt"), "hello")
	assertLocalDirHolds(t, workDir, "Drive Root")
}

// A single-file get of a drive path takes no remote name but the path's own
// last element as the local file: a name OneDrive could report in its place
// that is not one plain local name (empty, . or ..) fails the path lookup,
// with no local path or with a local folder to download into, and nothing is
// written. The path's own name is the default.
//
// Validates: R-1.2.7, R-1.2.6
func TestRunGet_FileReportedUnderAnUnsafeNameIsNotWritten(t *testing.T) {
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	id := addFakeItem(fake, fakeGraphRootID, "report.txt", false, "hello")
	workDir := t.TempDir()
	t.Chdir(workDir)
	into := "into" + string(filepath.Separator)

	for _, name := range []string{"", ".", ".."} {
		reportFakeName(fake, id, name)

		bare := runFileCommand(t, getCommand, cc, false, "/report.txt")
		require.ErrorIs(t, bare.err, graph.ErrNotFound, "the remote name %q", name)
		inFolder := runFileCommand(t, getCommand, cc, false, "/report.txt", into)
		require.ErrorIs(t, inFolder.err, graph.ErrNotFound, "the remote name %q", name)
	}
	assertLocalDirHolds(t, workDir)

	reportFakeName(fake, id, "report.txt")
	named := runFileCommand(t, getCommand, cc, false, "/report.txt")
	require.NoError(t, named.err)
	assertFileHolds(t, filepath.Join(workDir, "report.txt"), "hello")
}

// readOnlyFolder creates the folder name in dir, which no file can be
// created in, and makes it writable again when the test ends so it can be
// removed.
func readOnlyFolder(t *testing.T, dir, name string) {
	t.Helper()

	folder := filepath.Join(dir, name)
	require.NoError(t, os.Mkdir(folder, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(folder, 0o700)) }) //nolint:gosec // restores the private test folder
}

// A folder get names a file it cannot write once, as the folder the user
// typed names it, and not the partial file it could not create; under
// --json the failure is the same string.
//
// Validates: R-6.6.26, R-1.2.1, R-6.6.33
func TestRunGet_FolderNamesAnUnwritableFileOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	cc.Logger = slog.New(slog.DiscardHandler)
	addFakeItem(fake, addFakeItem(fake, fakeGraphRootID, "Docs", true, ""), "a.txt", false, "hello")
	parent := t.TempDir()
	readOnlyFolder(t, parent, "Docs")
	t.Chdir(parent)
	failure := filepath.Join("Docs", "a.txt") + ": creating the partial file: opening: permission denied"

	text := runFileCommand(t, getCommand, cc, false, "/Docs", "Docs")

	require.EqualError(t, text.err, "1 item failed to download")
	assert.Equal(t, "Downloaded 0 files, 0 folders (0 B)\nFailed: "+failure+"\n", text.stderr)

	asJSON := runFileCommand(t, getCommand, cc, true, "/Docs", "Docs")

	require.EqualError(t, asJSON.err, "1 item failed to download")
	var doc folderTransferJSONOutput[getJSONOutput]
	require.NoError(t, json.Unmarshal([]byte(asJSON.stdout), &doc), asJSON.stdout)
	assert.Equal(t, []string{failure}, doc.Errors)
}

// A single get that cannot write its file says so in one sentence, and its
// details name the partial file once, below the folder the user typed, with
// no other local path.
//
// Validates: R-6.6.26
func TestRunGet_AnUnwritableFileIsNamedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}
	setTestDriveHome(t)
	cc, fake := jsonContractFileContext(t)
	addFakeItem(fake, fakeGraphRootID, "b.txt", false, "world")
	parent := t.TempDir()
	readOnlyFolder(t, parent, "ro")
	t.Chdir(parent)

	run := runFileCommand(t, getCommand, cc, false, "/b.txt", filepath.Join("ro", "b.txt"))

	require.ErrorIs(t, run.err, os.ErrPermission)
	plain, details, found := strings.Cut(commandErrorText(run.err, true), "\nDetails: ")
	require.True(t, found)
	assert.Equal(t, "Error: Permission denied for "+filepath.Join(parent, "ro")+". Check its file permissions.", plain,
		"the sentence names the folder the hidden partial file could not be made in")
	assert.Regexp(t, `^download file: creating the partial file: opening ro/\.onedrive-go\.b\.txt[^ ]*\.partial: permission denied$`,
		details)
	assert.NotContains(t, details, parent, "the absolute path is not printed")
}

// A folder download with no failures prints "errors": [], and one that
// downloaded nothing prints "files": []: a list is never null.
//
// Validates: R-1.2.4, R-6.6.33
func TestGetFolderJSONOutput_EmptyErrors(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSONDocument(&buf, folderTransferJSONOutput[getJSONOutput]{FoldersCreated: 1}.withEmptyLists()))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))

	assert.Equal(t, []any{}, decoded["errors"], "no errors is [], not null")
	assert.Equal(t, []any{}, decoded["files"], "no files is [], not null")
	assert.InDelta(t, 1, decoded["folders_created"], 0)
	assert.InDelta(t, 0, decoded["total_size"], 0)
}

// The folder form of get prints this document byte for byte: its field names
// and their order, a path holding &, < and > as itself, and a list with
// nothing in it as [].
//
// Validates: R-1.2.4, R-6.6.33
func TestGetFolderJSONOutput_Golden(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, writeJSONDocument(&buf, folderTransferJSONOutput[getJSONOutput]{
		Files: []getJSONOutput{
			{Path: "Q&A <draft>/notes.txt", Size: 100, HashVerified: true},
			{Path: "Q&A <draft>/v2>v1.txt", Size: 200},
		},
		FoldersCreated: 2,
		TotalSize:      300,
	}.withEmptyLists()))

	assertGoldenFile(t, "get_folder_json.golden", buf.Bytes())
}

// A configured min_free_space the disk-space check cannot read is reported,
// never taken as 0, which would turn the check off without a word. Every get,
// of a file or a folder, a drive path or a shared item, builds its transfer
// manager through newDownloadManager, which reads it here.
//
// Validates: R-6.4.7
func TestConfiguredMinFreeSpace(t *testing.T) {
	t.Parallel()

	minFree, err := configuredMinFreeSpace(&CLIContext{})
	require.NoError(t, err)
	assert.Zero(t, minFree, "no configured drive, no check")

	minFree, err = configuredMinFreeSpace(&CLIContext{Cfg: &config.ResolvedDrive{
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "2KiB"},
	}})
	require.NoError(t, err)
	assert.Equal(t, int64(2048), minFree)

	unreadable := &CLIContext{Cfg: &config.ResolvedDrive{
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "not-a-size"},
	}}
	_, err = configuredMinFreeSpace(unreadable)
	require.ErrorContains(t, err, "min_free_space")
	_, err = newDownloadManager(unreadable, nil)
	require.ErrorContains(t, err, "min_free_space")
}
