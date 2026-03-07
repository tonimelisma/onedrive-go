package main

import (
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
)

// stubTS implements graph.TokenSource for tests.
type stubTS struct{}

func (s stubTS) Token() (string, error) { return "test", nil }

func makeTestSession(t *testing.T, handler http.Handler) *driveops.Session {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := graph.NewClient(srv.URL, srv.Client(), stubTS{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test/1.0")

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

	parentID, newName, existingID, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", true)
	require.NoError(t, err)
	assert.Equal(t, "parent-folder-id", parentID)
	assert.Equal(t, "dest.txt", newName)
	assert.Equal(t, "existing-file-id", existingID, "should return existing file ID for deletion")
}

func TestResolveDest_ForceEmptyParentID(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// File exists but has no parentReference (shouldn't happen, but defensive)
		fmt.Fprintf(w, `{"id":"file-id","name":"dest.txt"}`)
	}))

	parentID, _, _, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", true)
	require.Error(t, err)
	assert.Empty(t, parentID)
	assert.Contains(t, err.Error(), "parent")
}

func TestResolveDest_NoForceFileExists(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"file-id","name":"dest.txt","parentReference":{"id":"p1"}}`)
	}))

	parentID, _, _, err := resolveDest(t.Context(), session, "dest.txt", "source.txt", false)
	require.Error(t, err)
	assert.Empty(t, parentID)
	assert.Contains(t, err.Error(), "--force")
}

func TestResolveDest_FolderDest(t *testing.T) {
	t.Parallel()

	session := makeTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"folder-id","name":"destdir","folder":{}}`)
	}))

	parentID, newName, existingID, err := resolveDest(t.Context(), session, "destdir", "source.txt", false)
	require.NoError(t, err)
	assert.Equal(t, "folder-id", parentID)
	assert.Equal(t, "source.txt", newName)
	assert.Empty(t, existingID, "folder dest should not return existingID")
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

	parentID, newName, existingID, err := resolveDest(t.Context(), session, "parentdir/newname.txt", "source.txt", false)
	require.NoError(t, err)
	assert.Equal(t, "parent-id", parentID)
	assert.Equal(t, "newname.txt", newName)
	assert.Empty(t, existingID)
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
