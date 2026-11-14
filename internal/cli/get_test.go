package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-1.2.1
func TestJoinRemotePath(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   string
	}{
		{name: "root parent", parent: "", child: "docs", want: "docs"},
		{name: "slash parent", parent: "/", child: "docs", want: "docs"},
		{name: "nested parent", parent: "foo/bar", child: "baz", want: "foo/bar/baz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, joinRemotePath(tt.parent, tt.child))
		})
	}
}

// Validates: R-1.2.1
func TestCountRemoteFiles_PopulatesCache(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Root listing: two files, no folders.
		writeTestResponsef(t, w, `{"value":[
			{"id":"f1","name":"a.txt"},
			{"id":"f2","name":"b.txt"}
		]}`)
	}))

	state := newDownloadState()

	err := countRemoteFiles(t.Context(), session, "", "root-id", state)
	require.NoError(t, err)
	assert.Equal(t, 2, state.total, "should count both files")
	assert.Len(t, state.childCache["root-id"], 2, "should cache the listing under the folder's item ID")
	assert.Equal(t, "a.txt", state.childCache["root-id"][0].Name)
	assert.Empty(t, state.countErrors)
}

// Validates: R-1.2.1
func TestCountRemoteFiles_SurvivesSubdirError(t *testing.T) {
	// countRemoteFiles should record errors for inaccessible subdirs
	// but continue counting accessible parts of the tree.
	t.Parallel()

	callCount := 0
	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// Root listing: one file + one folder
			writeTestResponsef(t, w, `{"value":[
				{"id":"f1","name":"a.txt"},
				{"id":"d1","name":"subdir","folder":{}}
			]}`)

			return
		}
		// Subdir listing fails
		w.WriteHeader(http.StatusForbidden)
		writeTestResponsef(t, w, `{"error":{"code":"accessDenied"}}`)
	}))

	state := newDownloadState()

	err := countRemoteFiles(t.Context(), session, "", "root-id", state)
	require.NoError(t, err, "counting should not fail on subdirectory errors")
	assert.Equal(t, 1, state.total, "should count the accessible file")
	assert.NotEmpty(t, state.countErrors, "should record the subdirectory error")
}

// Validates: R-6.3.6
func TestDownloadTree_RespectsContextCancellation(t *testing.T) {
	// When the context is canceled, goroutines waiting for the semaphore
	// should not block forever — they should bail out promptly, and say so.
	// The shared folder walk used to return without a word.
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// This handler should never be called because the context is canceled
		// before any download starts.
		w.WriteHeader(http.StatusInternalServerError)
	}))

	state := &downloadState{
		childCache: make(map[string][]graph.Item),
		sem:        make(chan struct{}, 1), // capacity 1
	}

	// Fill the semaphore so the download goroutine must wait.
	state.sem <- struct{}{}

	// Cache a single file child.
	state.childCache["testdir"] = []graph.Item{
		{Name: "blocked.txt", ID: "f1"},
	}

	// Cancel the context immediately.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cc := &CLIContext{Flags: CLIFlags{Quiet: true}}

	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, nil, cc.Logger)
	downloadTree(ctx, cc, tm, state, session.DriveID, "testdir", t.TempDir())
	state.wg.Wait()

	// The goroutine should have recorded a context error, not hung forever.
	state.mu.Lock()
	defer state.mu.Unlock()
	require.NotEmpty(t, state.result.Errors, "should record context cancellation error")
	assert.Contains(t, state.result.Errors[0], "context canceled")
}

// Validates: R-1.2.4
func TestGetJSONOutput_Serialization(t *testing.T) {
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
	out := getFolderJSONOutput{
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

	var decoded getFolderJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 3, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
	assert.Contains(t, decoded.Errors[0], "permission denied")
}

// Validates: R-1.2.4
func TestGetFolderJSONOutput_EmptyErrors(t *testing.T) {
	out := getFolderJSONOutput{
		Files:          []getJSONOutput{},
		FoldersCreated: 1,
		TotalSize:      0,
		Errors:         nil,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	// nil slice should serialize as null, not [].
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &decoded))

	errs, present := decoded["errors"]
	assert.True(t, present, "errors is always in the output")
	assert.Nil(t, errs, "no errors is null")
	assert.InDelta(t, 1, decoded["folders_created"], 0)
	assert.InDelta(t, 0, decoded["total_size"], 0)
}

// Validates: R-1.2.4
func TestPrintGetJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printGetJSON(&buf, getJSONOutput{
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
func TestPrintGetFolderJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printGetFolderJSON(&buf, getFolderJSONOutput{
		Files: []getJSONOutput{
			{Path: "dir/a.txt", Size: 100, HashVerified: true},
			{Path: "dir/b.txt", Size: 200, HashVerified: false},
		},
		FoldersCreated: 2,
		TotalSize:      300,
		Errors:         []string{"dir/c.txt: access denied"},
	})
	require.NoError(t, err)

	var decoded getFolderJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Len(t, decoded.Files, 2)
	assert.Equal(t, 2, decoded.FoldersCreated)
	assert.Equal(t, int64(300), decoded.TotalSize)
	assert.Len(t, decoded.Errors, 1)
}

// buildFolderOnlyTree fills the child cache with a wide, shallow folder tree
// containing no files, so a recursive download performs directory descent and
// nothing else. Listings are cached under their folder's item ID, as both
// counting passes cache them.
func buildFolderOnlyTree(state *downloadState, rootID string, folders int) {
	children := make([]graph.Item, 0, folders)
	for i := range folders {
		id := fmt.Sprintf("dir-%d", i)
		children = append(children, graph.Item{Name: id, ID: id, IsFolder: true})
		state.childCache[id] = nil
	}

	state.childCache[rootID] = children
}

// folderOnlyDownload is what downloadTree needs for a tree without files: a
// file download would fail, so the walk is all it does.
type folderOnlyDownload struct {
	cc      *CLIContext
	tm      *driveops.TransferManager
	state   *downloadState
	driveID driveid.ID
}

func newFolderOnlyDownload(t *testing.T, folders int) folderOnlyDownload {
	t.Helper()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	state := newDownloadState()
	buildFolderOnlyTree(state, "testdir", folders)
	cc := &CLIContext{Flags: CLIFlags{Quiet: true}}

	return folderOnlyDownload{
		cc:      cc,
		tm:      driveops.NewTransferManager(session.Transfer, session.Transfer, nil, cc.Logger),
		state:   state,
		driveID: session.DriveID,
	}
}

func (d folderOnlyDownload) walk(t *testing.T, localPath string) {
	t.Helper()

	downloadTree(t.Context(), d.cc, d.tm, d.state, d.driveID, "testdir", localPath)
}

// Directory descent is inline, for a drive path and a shared folder alike: a
// goroutine per subdirectory would make the goroutine count follow the number
// of directories in the tree, and the shared semaphore bounds only file
// transfers. The observable consequence is that the whole walk has finished
// when downloadTree returns, without anyone waiting on the WaitGroup: every
// directory is counted and every local directory exists. Spawned descent
// cannot offer that, because its work would still be queued when the call
// returns.
//
// This deliberately does not sample runtime.NumGoroutine. That counter is
// process-global, so parallel tests move it between two samples and the
// assertion fails for reasons that have nothing to do with this code.
//
// Validates: R-1.2.1
func TestDownloadTree_DirectoryDescentDoesNotFanOutGoroutines(t *testing.T) {
	t.Parallel()

	const folders = 200

	download := newFolderOnlyDownload(t, folders)
	localRoot := filepath.Join(t.TempDir(), "testdir")

	download.walk(t, localRoot)

	download.state.mu.Lock()
	created := download.state.result.FoldersCreated
	errs := download.state.result.Errors
	download.state.mu.Unlock()

	assert.Empty(t, errs)
	assert.Equal(t, folders+1, created,
		"the directory walk must be complete when downloadTree returns")

	for i := range folders {
		dir := filepath.Join(localRoot, fmt.Sprintf("dir-%d", i))
		info, statErr := os.Stat(dir)
		require.NoError(t, statErr, "every directory must exist before downloadTree returns")
		assert.True(t, info.IsDir())
	}
}

// folders_created counted every folder walked, so a download into an
// existing tree claimed to have created folders that were already there.
// An existing folder is used as it is and keeps its mode.
//
// Validates: R-1.2.4
func TestDownloadTree_CountsOnlyTheFoldersItCreates(t *testing.T) {
	t.Parallel()

	download := newFolderOnlyDownload(t, 2)
	localRoot := t.TempDir()
	existing := filepath.Join(localRoot, "dir-0")
	require.NoError(t, os.Mkdir(existing, 0o700))

	download.walk(t, localRoot)

	assert.Empty(t, download.state.result.Errors)
	assert.Equal(t, 1, download.state.result.FoldersCreated, "only dir-1 is new")
	assert.DirExists(t, filepath.Join(localRoot, "dir-1"))
	info, err := os.Stat(existing)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "an existing folder keeps its mode")
}

// A file where the download needs a folder is reported once, naming the
// path, and nothing is walked below it.
//
// Validates: R-1.2.1
func TestDownloadTree_FileWhereAFolderBelongsIsReported(t *testing.T) {
	t.Parallel()

	download := newFolderOnlyDownload(t, 1)
	target := filepath.Join(t.TempDir(), "testdir")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))

	download.walk(t, target)

	require.Len(t, download.state.result.Errors, 1)
	assert.Equal(t, "creating directory "+target+": it is not a folder", download.state.result.Errors[0])
	assert.Zero(t, download.state.result.FoldersCreated)
}

// A folder `get` creates for a folder download is asked for 0777 and gets the
// umask, like the files downloaded into it. It used to be 0755 whatever the
// umask, so under 002 the files were group-writable and their folder was not.
//
// Validates: R-5.10.2, R-1.2.1
func TestDownloadTree_CreatedFolderFollowsTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	download := newFolderOnlyDownload(t, 1)
	target := filepath.Join(t.TempDir(), "testdir")

	download.walk(t, target)

	assert.Empty(t, download.state.result.Errors)
	for _, dir := range []string{target, filepath.Join(target, "dir-0")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "%s is 0777 minus the umask", dir)
	}
}

// A configured min_free_space the disk-space check cannot read was taken as
// 0, which turns the check off without a word; both get flavors read it in
// one place now and report it.
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

	_, err = configuredMinFreeSpace(&CLIContext{Cfg: &config.ResolvedDrive{
		SafetyConfig: config.SafetyConfig{MinFreeSpace: "not-a-size"},
	}})
	require.ErrorContains(t, err, "min_free_space")
}
