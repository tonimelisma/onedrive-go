package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// stubTS implements graph.TokenSource for tests.
type stubTS struct{}

func (s stubTS) Token() (string, error) { return "test", nil }

func makeTestSession(t *testing.T, handler http.Handler) *driveops.Session {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := graph.NewClient(srv.URL, srv.Client(), stubTS{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test/1.0", retry.Transport)

	return &driveops.Session{
		Meta:    client,
		DriveID: driveid.New("test-drive-id"),
	}
}

// --- resolveDest ---

func TestResolveDest_ForceReturnsExistingID(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ResolveItem("dest.txt") → returns existing file
		fmt.Fprintf(w, `{"id":"existing-file-id","name":"dest.txt","parentReference":{"id":"parent-folder-id"}}`)
	}))

	dest, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", true)
	require.NoError(t, err)
	assert.Equal(t, "parent-folder-id", dest.parentID)
	assert.Equal(t, "dest.txt", dest.newName)
	assert.Equal(t, "existing-file-id", dest.existingID, "should return existing file ID for deletion")
}

func TestResolveDest_ForceEmptyParentID(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// File exists but has no parentReference (shouldn't happen, but defensive)
		fmt.Fprintf(w, `{"id":"file-id","name":"dest.txt"}`)
	}))

	dest, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", true)
	require.Error(t, err)
	assert.Empty(t, dest.parentID)
	assert.Contains(t, err.Error(), "parent")
}

func TestResolveDest_NoForceFileExists(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"file-id","name":"dest.txt","parentReference":{"id":"p1"}}`)
	}))

	dest, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", false)
	require.Error(t, err)
	assert.Empty(t, dest.parentID)
	assert.Contains(t, err.Error(), "--force")
}

func TestResolveDest_FolderDest(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"folder-id","name":"destdir","folder":{}}`)
	}))

	dest, err := resolveDest(t.Context(), session, "destdir", "source.txt", false)
	require.NoError(t, err)
	assert.Equal(t, "folder-id", dest.parentID)
	assert.Equal(t, "source.txt", dest.newName)
	assert.Empty(t, dest.existingID, "folder dest should not return existingID")
	assert.True(t, dest.destIsDir, "folder dest should set destIsDir")
}

func TestResolveDest_NotFound(t *testing.T) {
	t.Parallel()

	callCount := 0
	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// dest doesn't exist
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"code":"itemNotFound"}}`)

			return
		}
		// parent exists
		fmt.Fprintf(w, `{"id":"parent-id","name":"parentdir","folder":{}}`)
	}))

	dest, err := resolveDest(t.Context(), session, "parentdir/newname.txt", "source.txt", false)
	require.NoError(t, err)
	assert.Equal(t, "parent-id", dest.parentID)
	assert.Equal(t, "newname.txt", dest.newName)
	assert.Empty(t, dest.existingID)
	assert.False(t, dest.destIsDir)
}

func TestResolveDest_SelfReferenceDetected(t *testing.T) {
	// When --force resolves to the same file, isSelfReference should detect it.
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":"item-1","name":"file.txt","parentReference":{"id":"parent-1"}}`)
	}))

	dest, err := resolveDest(t.Context(), session, "file.txt", "file.txt", true)
	require.NoError(t, err)
	assert.True(t, isSelfReference("item-1", dest), "should detect self-reference")
	assert.Equal(t, "item-1", dest.existingID)
}

func TestIsNoOpMove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		dest     destInfo
		srcPID   string
		srcName  string
		wantNoop bool
	}{
		{
			name:     "same parent and name",
			dest:     destInfo{parentID: "p1", newName: "file.txt"},
			srcPID:   "p1",
			srcName:  "file.txt",
			wantNoop: true,
		},
		{
			name:     "different parent",
			dest:     destInfo{parentID: "p2", newName: "file.txt"},
			srcPID:   "p1",
			srcName:  "file.txt",
			wantNoop: false,
		},
		{
			name:     "different name",
			dest:     destInfo{parentID: "p1", newName: "renamed.txt"},
			srcPID:   "p1",
			srcName:  "file.txt",
			wantNoop: false,
		},
		{
			name:     "self-reference via force",
			dest:     destInfo{parentID: "p1", newName: "file.txt", existingID: "item-1"},
			srcPID:   "p1",
			srcName:  "file.txt",
			wantNoop: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantNoop, isNoOpMove(tt.dest, tt.srcPID, tt.srcName))
		})
	}
}

func TestNoOpMoveProducesOutput(t *testing.T) {
	// When a move is a no-op, the command should still produce status output.
	t.Parallel()

	var buf bytes.Buffer
	cc := &CLIContext{StatusWriter: &buf}

	// Simulate a no-op move output.
	emitMoveResult(cc, "file.txt", "file.txt", "item-1")
	assert.Contains(t, buf.String(), "file.txt")
}

func TestMvJSONOutput_Serialization(t *testing.T) {
	out := mvJSONOutput{
		Source:      "/docs/report.pdf",
		Destination: "/archive/report.pdf",
		ID:          "item-456",
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var decoded mvJSONOutput
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/docs/report.pdf", decoded.Source)
	assert.Equal(t, "/archive/report.pdf", decoded.Destination)
	assert.Equal(t, "item-456", decoded.ID)
}

func TestMvJSONOutput_Fields(t *testing.T) {
	out := mvJSONOutput{
		Source:      "a",
		Destination: "b",
		ID:          "c",
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &raw))

	assert.Contains(t, raw, "source")
	assert.Contains(t, raw, "destination")
	assert.Contains(t, raw, "id")
	assert.Len(t, raw, 3)
}

// Validates: R-1.7.1
func TestPrintMvJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printMvJSON(&buf, mvJSONOutput{
		Source:      "/docs/report.pdf",
		Destination: "/archive/report.pdf",
		ID:          "item-456",
	})
	require.NoError(t, err)

	var decoded mvJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "/docs/report.pdf", decoded.Source)
	assert.Equal(t, "/archive/report.pdf", decoded.Destination)
	assert.Equal(t, "item-456", decoded.ID)
}
