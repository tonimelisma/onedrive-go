package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// Validates: R-1.2.4
func TestGetJSONOutput_Serialization(t *testing.T) {
	t.Parallel()

	out := getJSONOutput{
		Path:         "/tmp/test.txt",
		Size:         1024,
		HashVerified: true,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded getJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/tmp/test.txt", decoded.Path)
	assert.Equal(t, int64(1024), decoded.Size)
	assert.True(t, decoded.HashVerified)
}

// Validates: R-1.2.4
func TestGetFolderJSONOutput_Serialization(t *testing.T) {
	t.Parallel()

	out := folderTransferJSONOutput[getJSONOutput]{
		Files: []getJSONOutput{
			{Path: "dir/a.txt", Size: 100, HashVerified: true},
			{Path: "dir/b.txt", Size: 200, HashVerified: false},
		},
		FoldersCreated: 3,
		TotalSize:      300,
		Errors:         []string{"file c.txt: permission denied"},
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded folderTransferJSONOutput[getJSONOutput]
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 3, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
	assert.Contains(t, decoded.Errors[0], "permission denied")
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

// Validates: R-1.2.4
func TestGetJSONOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := writeJSONDocument(&buf, getJSONOutput{
		Path:         "report.pdf",
		Size:         4096,
		HashVerified: true,
	})
	require.NoError(t, err)

	var decoded getJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "report.pdf", decoded.Path)
	assert.Equal(t, int64(4096), decoded.Size)
	assert.True(t, decoded.HashVerified)
}

// Validates: R-1.2.4
func TestGetFolderJSONOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := writeJSONDocument(&buf, folderTransferJSONOutput[getJSONOutput]{
		Files: []getJSONOutput{
			{Path: "dir/a.txt", Size: 100, HashVerified: true},
			{Path: "dir/b.txt", Size: 200, HashVerified: false},
		},
		FoldersCreated: 2,
		TotalSize:      300,
		Errors:         []string{"dir/c.txt: access denied"},
	}.withEmptyLists())
	require.NoError(t, err)

	var decoded folderTransferJSONOutput[getJSONOutput]
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 2, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
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
