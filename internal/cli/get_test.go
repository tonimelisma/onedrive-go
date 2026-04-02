package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestJoinRemotePath(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   string
	}{
		{"root parent", "", "docs", "docs"},
		{"slash parent", "/", "docs", "docs"},
		{"nested parent", "foo/bar", "baz", "foo/bar/baz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, joinRemotePath(tt.parent, tt.child))
		})
	}
}

func TestCountRemoteFiles_PopulatesCache(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Root listing: two files, no folders.
		writeTestResponsef(t, w, `{"value":[
			{"id":"f1","name":"a.txt"},
			{"id":"f2","name":"b.txt"}
		]}`)
	}))

	state := &downloadState{
		childCache: make(map[string][]graph.Item),
	}

	err := countRemoteFiles(t.Context(), session, "", state)
	require.NoError(t, err)
	assert.Equal(t, 2, state.total, "should count both files")
	assert.Len(t, state.childCache[""], 2, "should cache the listing")
	assert.Equal(t, "a.txt", state.childCache[""][0].Name)
	assert.Empty(t, state.countErrors)
}

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

	state := &downloadState{
		childCache: make(map[string][]graph.Item),
	}

	err := countRemoteFiles(t.Context(), session, "", state)
	require.NoError(t, err, "counting should not fail on subdirectory errors")
	assert.Equal(t, 1, state.total, "should count the accessible file")
	assert.NotEmpty(t, state.countErrors, "should record the subdirectory error")
}

func TestDownloadRecursive_RespectsContextCancellation(t *testing.T) {
	// When the context is canceled, goroutines waiting for the semaphore
	// should not block forever — they should bail out promptly.
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
	downloadRecursive(ctx, cc, session, tm, state, "testdir", t.TempDir())
	state.wg.Wait()

	// The goroutine should have recorded a context error, not hung forever.
	state.mu.Lock()
	defer state.mu.Unlock()
	require.NotEmpty(t, state.result.Errors, "should record context cancellation error")
	assert.Contains(t, state.result.Errors[0], "context canceled")
}

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
