package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestGetItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/000drive-abc-123/items/item-123", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-123",
			"name": "test-file.txt",
			"size": 1024,
			"eTag": "etag-abc",
			"cTag": "ctag-def",
			"createdDateTime": "2024-01-15T10:30:00Z",
			"lastModifiedDateTime": "2024-06-20T14:45:00Z",
			"parentReference": {
				"id": "parent-456",
				"driveId": "DRIVE-ABC-123"
			},
			"file": {
				"mimeType": "text/plain",
				"hashes": {
					"quickXorHash": "aGFzaHZhbHVl",
					"sha1Hash": "da39a3ee5e6b4b0d3255bfef95601890afd80709",
					"sha256Hash": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
				}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("drive-abc-123"), "item-123")
	require.NoError(t, err)

	assert.Equal(t, "item-123", item.ID)
	assert.Equal(t, "test-file.txt", item.Name)
	assert.Equal(t, driveid.New("drive-abc-123"), item.DriveID)
	assert.Equal(t, "parent-456", item.ParentID)
	assert.Equal(t, int64(1024), item.Size)
	assert.Equal(t, "etag-abc", item.ETag)
	assert.Equal(t, "ctag-def", item.CTag)
	assert.False(t, item.IsFolder)
	assert.False(t, item.IsDeleted)
	assert.False(t, item.IsPackage)
	assert.Equal(t, "text/plain", item.MimeType)
	assert.Equal(t, "aGFzaHZhbHVl", item.QuickXorHash)
	assert.Equal(t, "da39a3ee5e6b4b0d3255bfef95601890afd80709", item.SHA1Hash)
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", item.SHA256Hash)
	assert.Equal(t, 2024, item.CreatedAt.Year())
	assert.Equal(t, 2024, item.ModifiedAt.Year())
	assert.Equal(t, ChildCountUnknown, item.ChildCount)
}

func TestGetItem_Folder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "folder-789",
			"name": "Documents",
			"size": 0,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "root", "driveId": "drive-1"},
			"folder": {"childCount": 42}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("drive-1"), "folder-789")
	require.NoError(t, err)

	assert.True(t, item.IsFolder)
	assert.Equal(t, 42, item.ChildCount)
	assert.Empty(t, item.MimeType)
	assert.Empty(t, item.QuickXorHash)
}

func TestGetItem_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.GetItem(t.Context(), driveid.New("drive-1"), "nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetItem_DriveIDNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Graph API sometimes returns uppercase drive IDs
		fmt.Fprint(w, `{
			"id": "item-1",
			"name": "test.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "parent-1", "driveId": "B!UPPERCASE-DRIVE-ID"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("b!uppercase-drive-id"), "item-1")
	require.NoError(t, err)

	assert.Equal(t, driveid.New("b!uppercase-drive-id"), item.DriveID)
	assert.Equal(t, driveid.New("b!uppercase-drive-id"), item.ParentDriveID)
}

func TestGetItem_InvalidTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-ts",
			"name": "bad-time.txt",
			"createdDateTime": "not-a-date",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "item-ts")
	require.NoError(t, err)

	// Invalid timestamp should fall back to approximately now
	assert.InDelta(t, time.Now().Unix(), item.CreatedAt.Unix(), 5)
	// Valid timestamp should parse correctly
	assert.Equal(t, 2024, item.ModifiedAt.Year())
}

func TestGetItem_FutureTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-future",
			"name": "future.txt",
			"createdDateTime": "2200-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "item-future")
	require.NoError(t, err)

	// Year 2200 exceeds maxValidYear — should fall back to now
	assert.InDelta(t, time.Now().Unix(), item.CreatedAt.Unix(), 5)
}

func TestGetItem_PackageAndDeleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-pkg",
			"name": "Notebook.one",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"deleted": {"state": "deleted"},
			"package": {"type": "oneNote"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "item-pkg")
	require.NoError(t, err)

	assert.True(t, item.IsDeleted)
	assert.True(t, item.IsPackage)
}

func TestGetItem_NilParentReference(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Root items may not have parentReference
		fmt.Fprint(w, `{
			"id": "root-item",
			"name": "root",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"folder": {"childCount": 10}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "root-item")
	require.NoError(t, err)

	assert.True(t, item.DriveID.IsZero())
	assert.Empty(t, item.ParentID)
	assert.True(t, item.IsFolder)
	assert.Equal(t, 10, item.ChildCount)
}

func TestGetItem_NilFileFacet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "folder-1",
			"name": "Folder",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"folder": {"childCount": 0}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "folder-1")
	require.NoError(t, err)

	assert.Empty(t, item.MimeType)
	assert.Empty(t, item.QuickXorHash)
	assert.Empty(t, item.SHA1Hash)
	assert.Empty(t, item.SHA256Hash)
}

// --- ListChildren tests ---

func TestListChildren_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/drives/000000000000000d/items/parent/children")
		assert.Equal(t, "200", r.URL.Query().Get("$top"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{"id":"a","name":"file-a.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"parent","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"b","name":"file-b.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"parent","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"c","name":"folder-c","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"parent","driveId":"d"},"folder":{"childCount":5}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildren(t.Context(), driveid.New("d"), "parent")
	require.NoError(t, err)

	assert.Len(t, items, 3)
	assert.Equal(t, "file-a.txt", items[0].Name)
	assert.Equal(t, "file-b.txt", items[1].Name)
	assert.Equal(t, "folder-c", items[2].Name)
	assert.False(t, items[0].IsFolder)
	assert.True(t, items[2].IsFolder)
	assert.Equal(t, 5, items[2].ChildCount)
}

func TestListChildren_MultiPage(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if strings.Contains(r.URL.Path, "/children") && !strings.Contains(r.URL.RawQuery, "page=2") {
			// First page — includes nextLink
			fmt.Fprintf(w, `{
				"value": [
					{"id":"a","name":"item-a","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/items/p/children?$top=200&page=2"
			}`, srv.URL)
		} else {
			// Second page — no nextLink
			fmt.Fprint(w, `{
				"value": [
					{"id":"b","name":"item-b","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"}}
				]
			}`)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildren(t.Context(), driveid.New("d"), "p")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "item-a", items[0].Name)
	assert.Equal(t, "item-b", items[1].Name)
}

func TestListChildren_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildren(t.Context(), driveid.New("d"), "empty-folder")
	require.NoError(t, err)

	assert.Empty(t, items)
}

func TestListChildren_MixedTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{"id":"file-1","name":"doc.pdf","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"},"file":{"mimeType":"application/pdf"}},
				{"id":"folder-1","name":"Photos","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"},"folder":{"childCount":100}},
				{"id":"pkg-1","name":"Notebook","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"},"package":{"type":"oneNote"}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildren(t.Context(), driveid.New("d"), "p")
	require.NoError(t, err)

	assert.Len(t, items, 3)
	assert.False(t, items[0].IsFolder)
	assert.Equal(t, "application/pdf", items[0].MimeType)
	assert.True(t, items[1].IsFolder)
	assert.Equal(t, 100, items[1].ChildCount)
	assert.True(t, items[2].IsPackage)
}

func TestListChildren_InvalidNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// nextLink points to a different host — should be rejected
		fmt.Fprint(w, `{
			"value": [{"id":"a","name":"a","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],
			"@odata.nextLink": "https://evil.example.com/next-page"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.ListChildren(t.Context(), driveid.New("d"), "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match base URL")
}

// --- CreateFolder tests ---

func TestCreateFolder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/parent/children", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "New Folder", req["name"])
		assert.NotNil(t, req["folder"])
		assert.Equal(t, "fail", req["@microsoft.graph.conflictBehavior"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{
			"id": "new-folder-id",
			"name": "New Folder",
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"folder": {"childCount": 0}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.CreateFolder(t.Context(), driveid.New("d"), "parent", "New Folder")
	require.NoError(t, err)

	assert.Equal(t, "new-folder-id", item.ID)
	assert.Equal(t, "New Folder", item.Name)
	assert.True(t, item.IsFolder)
	assert.Equal(t, 0, item.ChildCount)
}

func TestCreateFolder_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-conflict")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"nameAlreadyExists"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateFolder(t.Context(), driveid.New("d"), "parent", "Existing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConflict)
}

// --- MoveItem tests ---

func TestMoveItem_MoveAndRename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-1", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &req))

		// Both parentReference and name should be present
		parentRef, ok := req["parentReference"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "new-parent", parentRef["id"])
		assert.Equal(t, "renamed.txt", req["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-1",
			"name": "renamed.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-06-01T00:00:00Z",
			"parentReference": {"id": "new-parent", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.MoveItem(t.Context(), driveid.New("d"), "item-1", "new-parent", "renamed.txt")
	require.NoError(t, err)

	assert.Equal(t, "renamed.txt", item.Name)
	assert.Equal(t, "new-parent", item.ParentID)
}

func TestMoveItem_RenameOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &req))

		// Only name, no parentReference
		assert.Equal(t, "new-name.txt", req["name"])
		assert.Nil(t, req["parentReference"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-1",
			"name": "new-name.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "old-parent", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.MoveItem(t.Context(), driveid.New("d"), "item-1", "", "new-name.txt")
	require.NoError(t, err)

	assert.Equal(t, "new-name.txt", item.Name)
}

func TestMoveItem_MoveOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &req))

		// Only parentReference, no name
		parentRef, ok := req["parentReference"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "new-parent", parentRef["id"])
		assert.Empty(t, req["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-1",
			"name": "unchanged.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "new-parent", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.MoveItem(t.Context(), driveid.New("d"), "item-1", "new-parent", "")
	require.NoError(t, err)

	assert.Equal(t, "new-parent", item.ParentID)
	assert.Equal(t, "unchanged.txt", item.Name)
}

func TestMoveItem_BothEmpty(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.MoveItem(t.Context(), driveid.New("d"), "item-1", "", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMoveNoChanges)
}

func TestMoveItem_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-move-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.MoveItem(t.Context(), driveid.New("d"), "nonexistent", "new-parent", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- DeleteItem tests ---

func TestDeleteItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-to-delete", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.DeleteItem(t.Context(), driveid.New("d"), "item-to-delete")
	require.NoError(t, err)
}

func TestDeleteItem_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-del-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.DeleteItem(t.Context(), driveid.New("d"), "nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- toItem edge cases ---

func TestToItem_EmptyTimestamp(t *testing.T) {
	dir := &driveItemResponse{
		ID:                   "item-empty-ts",
		Name:                 "empty.txt",
		CreatedDateTime:      "",
		LastModifiedDateTime: "",
		ParentReference:      &parentRef{ID: "p", DriveID: "d"},
	}

	item := dir.toItem(testNoopLogger())
	assert.InDelta(t, time.Now().Unix(), item.CreatedAt.Unix(), 5)
	assert.InDelta(t, time.Now().Unix(), item.ModifiedAt.Unix(), 5)
}

func TestToItem_RootFacet(t *testing.T) {
	dir := &driveItemResponse{
		ID:                   "root-item",
		Name:                 "root",
		CreatedDateTime:      "2024-01-01T00:00:00Z",
		LastModifiedDateTime: "2024-01-01T00:00:00Z",
		ParentReference:      &parentRef{ID: "", DriveID: "d"},
		Folder:               &folderFacet{ChildCount: 5},
		Root:                 ptrRawMsg(json.RawMessage(`{}`)),
	}

	item := dir.toItem(testNoopLogger())
	assert.True(t, item.IsRoot, "root facet should set IsRoot")
	assert.True(t, item.IsFolder, "root is also a folder")
	assert.Equal(t, 5, item.ChildCount)
}

func TestToItem_NonRootFolder(t *testing.T) {
	dir := &driveItemResponse{
		ID:                   "folder-1",
		Name:                 "Documents",
		CreatedDateTime:      "2024-01-01T00:00:00Z",
		LastModifiedDateTime: "2024-01-01T00:00:00Z",
		ParentReference:      &parentRef{ID: "root-id", DriveID: "d"},
		Folder:               &folderFacet{ChildCount: 3},
		// Root is nil — not a root folder.
	}

	item := dir.toItem(testNoopLogger())
	assert.False(t, item.IsRoot, "non-root folder should not have IsRoot")
	assert.True(t, item.IsFolder)
}

func TestGetItem_RootItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "root-id",
			"name": "root",
			"size": 0,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "", "driveId": "d"},
			"folder": {"childCount": 10},
			"root": {}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItem(t.Context(), driveid.New("d"), "root")
	require.NoError(t, err)

	assert.True(t, item.IsRoot, "root facet should be detected from API response")
	assert.True(t, item.IsFolder)
	assert.Equal(t, 10, item.ChildCount)
}

func TestToItem_FileWithNilHashes(t *testing.T) {
	dir := &driveItemResponse{
		ID:                   "item-no-hash",
		Name:                 "no-hash.txt",
		CreatedDateTime:      "2024-01-01T00:00:00Z",
		LastModifiedDateTime: "2024-01-01T00:00:00Z",
		ParentReference:      &parentRef{ID: "p", DriveID: "d"},
		File:                 &fileFacet{MimeType: "text/plain", Hashes: nil},
	}

	item := dir.toItem(testNoopLogger())
	assert.Equal(t, "text/plain", item.MimeType)
	assert.Empty(t, item.QuickXorHash)
}

func TestStripBaseURL(t *testing.T) {
	client := newTestClient(t, "https://graph.microsoft.com/v1.0")

	t.Run("valid URL", func(t *testing.T) {
		path, err := client.stripBaseURL("https://graph.microsoft.com/v1.0/drives/d/items/p/children?$top=200&$skiptoken=abc")
		require.NoError(t, err)
		assert.Equal(t, "/drives/d/items/p/children?$top=200&$skiptoken=abc", path)
	})

	t.Run("mismatched base", func(t *testing.T) {
		_, err := client.stripBaseURL("https://evil.example.com/path")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match base URL")
	})
}

// ptrRawMsg returns a pointer to a json.RawMessage, for test struct literals.
func ptrRawMsg(m json.RawMessage) *json.RawMessage {
	return &m
}

// testNoopLogger returns a logger that discards output, for unit tests that
// don't need log verification.
func testNoopLogger() *slog.Logger {
	return slog.Default()
}

// --- UpdateFileSystemInfo tests ---

func TestUpdateFileSystemInfo_Success(t *testing.T) {
	mtime := time.Date(2024, 8, 15, 14, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-fsi", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		bodyStr := string(body)
		assert.Contains(t, bodyStr, `"lastModifiedDateTime":"2024-08-15T14:30:00Z"`)
		assert.Contains(t, bodyStr, `"fileSystemInfo"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-fsi",
			"name": "patched.txt",
			"size": 100,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-08-15T14:30:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.UpdateFileSystemInfo(
		t.Context(), driveid.New("d"), "item-fsi", mtime,
	)
	require.NoError(t, err)

	assert.Equal(t, "item-fsi", item.ID)
	assert.Equal(t, "patched.txt", item.Name)
	assert.Equal(t, 2024, item.ModifiedAt.Year())
	assert.Equal(t, time.August, item.ModifiedAt.Month())
}

func TestUpdateFileSystemInfo_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.UpdateFileSystemInfo(
		t.Context(), driveid.New("d"), "item-decode",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding fileSystemInfo response")
}

func TestUpdateFileSystemInfo_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-fsi-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.UpdateFileSystemInfo(
		t.Context(), driveid.New("d"), "nonexistent",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- GetItemByPath tests ---

func TestGetItemByPath_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/000000000000000d/root:/Documents/file.txt:", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "item-path-1",
			"name": "file.txt",
			"size": 2048,
			"eTag": "etag-path",
			"cTag": "ctag-path",
			"createdDateTime": "2024-03-15T09:00:00Z",
			"lastModifiedDateTime": "2024-06-20T15:30:00Z",
			"parentReference": {
				"id": "documents-folder-id",
				"driveId": "D"
			},
			"file": {
				"mimeType": "text/plain",
				"hashes": {
					"quickXorHash": "cGF0aGhhc2g="
				}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItemByPath(t.Context(), driveid.New("d"), "Documents/file.txt")
	require.NoError(t, err)

	assert.Equal(t, "item-path-1", item.ID)
	assert.Equal(t, "file.txt", item.Name)
	assert.Equal(t, int64(2048), item.Size)
	assert.Equal(t, "etag-path", item.ETag)
	assert.Equal(t, "ctag-path", item.CTag)
	assert.Equal(t, driveid.New("d"), item.DriveID) // normalized to lowercase
	assert.Equal(t, "documents-folder-id", item.ParentID)
	assert.False(t, item.IsFolder)
	assert.Equal(t, "text/plain", item.MimeType)
	assert.Equal(t, "cGF0aGhhc2g=", item.QuickXorHash)
	assert.Equal(t, 2024, item.CreatedAt.Year())
	assert.Equal(t, 2024, item.ModifiedAt.Year())
}

func TestGetItemByPath_EncodesSpecialChars(t *testing.T) {
	// Verify that paths with special characters (#, spaces, ?) are URL-encoded
	// per-segment before being sent to the server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RequestURI preserves the raw percent-encoded path as sent on the wire.
		// "folder/my file#2.txt" → "folder/my%20file%232.txt"
		assert.Contains(t, r.RequestURI, "/drives/000000000000000d/root:/folder/my%20file%232.txt:")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "encoded-item",
			"name": "my file#2.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "folder-id", "driveId": "d"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.GetItemByPath(t.Context(), driveid.New("d"), "folder/my file#2.txt")
	require.NoError(t, err)

	assert.Equal(t, "encoded-item", item.ID)
	assert.Equal(t, "my file#2.txt", item.Name)
}

func TestEncodePathSegments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple path", "folder/file.txt", "folder/file.txt"},
		{"spaces", "my folder/my file.txt", "my%20folder/my%20file.txt"},
		{"hash", "folder/file#2.txt", "folder/file%232.txt"},
		{"question mark", "folder/file?.txt", "folder/file%3F.txt"},
		{"percent", "folder/100%.txt", "folder/100%25.txt"},
		{"mixed", "my docs/report #1.pdf", "my%20docs/report%20%231.pdf"},
		{"single segment", "file.txt", "file.txt"},
		{"deep path", "a/b/c/d.txt", "a/b/c/d.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, encodePathSegments(tt.input))
		})
	}
}

func TestGetItemByPath_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-path-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.GetItemByPath(t.Context(), driveid.New("d"), "nonexistent/path.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- ListChildrenByPath tests ---

func TestListChildrenByPath_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/000000000000000d/root:/Documents:/children", r.URL.Path)
		assert.Equal(t, "200", r.URL.Query().Get("$top"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{"id":"a","name":"report.pdf","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"docs","driveId":"d"},"file":{"mimeType":"application/pdf"}},
				{"id":"b","name":"notes.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"docs","driveId":"d"},"file":{"mimeType":"text/plain"}},
				{"id":"c","name":"Images","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"docs","driveId":"d"},"folder":{"childCount":12}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenByPath(t.Context(), driveid.New("d"), "Documents")
	require.NoError(t, err)

	assert.Len(t, items, 3)
	assert.Equal(t, "report.pdf", items[0].Name)
	assert.Equal(t, "notes.txt", items[1].Name)
	assert.Equal(t, "Images", items[2].Name)
	assert.False(t, items[0].IsFolder)
	assert.False(t, items[1].IsFolder)
	assert.True(t, items[2].IsFolder)
	assert.Equal(t, 12, items[2].ChildCount)
}

func TestListChildrenByPath_MultiPage(t *testing.T) {
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if !strings.Contains(r.URL.RawQuery, "page=2") {
			// First page — includes nextLink
			fmt.Fprintf(w, `{
				"value": [
					{"id":"a","name":"first.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"}}
				],
				"@odata.nextLink": "%s/drives/d/root:/Documents:/children?$top=200&page=2"
			}`, srv.URL)
		} else {
			// Second page — no nextLink
			fmt.Fprint(w, `{
				"value": [
					{"id":"b","name":"second.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"}}
				]
			}`)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenByPath(t.Context(), driveid.New("d"), "Documents")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "first.txt", items[0].Name)
	assert.Equal(t, "second.txt", items[1].Name)
}

func TestListChildrenByPath_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenByPath(t.Context(), driveid.New("d"), "EmptyFolder")
	require.NoError(t, err)

	assert.Empty(t, items)
}

// --- Path validation tests ---

func TestGetItemByPath_EmptyPath(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.GetItemByPath(t.Context(), driveid.New("d"), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestGetItemByPath_LeadingSlash(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.GetItemByPath(t.Context(), driveid.New("d"), "/foo/bar.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestListChildrenByPath_EmptyPath(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.ListChildrenByPath(t.Context(), driveid.New("d"), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestListChildrenByPath_LeadingSlash(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.ListChildrenByPath(t.Context(), driveid.New("d"), "/Documents")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

// --- remoteItem + shared facet tests ---

func TestToItem_SharedOwner_FallbackChain(t *testing.T) {
	// Fallback chain: remoteItem.shared.sharedBy > remoteItem.shared.owner >
	// remoteItem.createdBy > top-level shared.owner
	tests := []struct {
		name         string
		json         string
		wantName     string
		wantEmail    string
		wantRemoteID string
		wantDriveID  string
		wantIsFolder bool
		wantChildCnt int
	}{
		{
			name: "level 1: remoteItem.shared.sharedBy wins",
			json: `{
				"id": "item-1", "name": "Shared Folder",
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 5},
				"remoteItem": {
					"id": "source-item-id",
					"parentReference": {"driveId": "source-drive-id"},
					"shared": {
						"sharedBy": {"user": {"displayName": "Sharer", "email": "sharer@example.com"}},
						"owner": {"user": {"displayName": "Owner", "email": "owner@example.com"}}
					},
					"createdBy": {"user": {"displayName": "Creator", "email": "creator@example.com"}}
				},
				"shared": {"owner": {"user": {"displayName": "TopLevel", "email": "toplevel@example.com"}}}
			}`,
			wantName: "Sharer", wantEmail: "sharer@example.com",
			wantRemoteID: "source-item-id", wantDriveID: "source-drive-id",
			wantIsFolder: true, wantChildCnt: 5,
		},
		{
			name: "level 2: remoteItem.shared.owner when no sharedBy",
			json: `{
				"id": "item-2", "name": "Shared Folder",
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 3},
				"remoteItem": {
					"id": "src-id",
					"parentReference": {"driveId": "src-drive"},
					"shared": {
						"owner": {"user": {"displayName": "Owner", "email": "owner@example.com"}}
					},
					"createdBy": {"user": {"displayName": "Creator", "email": "creator@example.com"}}
				},
				"shared": {"owner": {"user": {"displayName": "TopLevel", "email": "toplevel@example.com"}}}
			}`,
			wantName: "Owner", wantEmail: "owner@example.com",
			wantRemoteID: "src-id", wantDriveID: "src-drive",
			wantIsFolder: true, wantChildCnt: 3,
		},
		{
			name: "level 3: remoteItem.createdBy when no shared facet",
			json: `{
				"id": "item-3", "name": "Shared Folder",
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 0},
				"remoteItem": {
					"id": "src-id",
					"parentReference": {"driveId": "src-drive"},
					"createdBy": {"user": {"displayName": "Creator", "email": "creator@example.com"}}
				},
				"shared": {"owner": {"user": {"displayName": "TopLevel", "email": "toplevel@example.com"}}}
			}`,
			wantName: "Creator", wantEmail: "creator@example.com",
			wantRemoteID: "src-id", wantDriveID: "src-drive",
			wantIsFolder: true, wantChildCnt: 0,
		},
		{
			name: "level 4: top-level shared.owner (no remoteItem identity)",
			json: `{
				"id": "item-4", "name": "Shared Folder",
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 5},
				"remoteItem": {
					"id": "source-item-id",
					"parentReference": {"driveId": "source-drive-id"}
				},
				"shared": {
					"owner": {
						"user": {
							"displayName": "John Doe",
							"email": "john@example.com"
						}
					}
				}
			}`,
			wantName: "John Doe", wantEmail: "john@example.com",
			wantRemoteID: "source-item-id", wantDriveID: "source-drive-id",
			wantIsFolder: true, wantChildCnt: 5,
		},
		{
			name: "createdBy displayName only (no email — search endpoint pattern)",
			json: `{
				"id": "item-5", "name": "Shared Folder",
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"folder": {"childCount": 0},
				"remoteItem": {
					"id": "src-id",
					"parentReference": {"driveId": "src-drive"},
					"createdBy": {"user": {"displayName": "Creator Only"}}
				}
			}`,
			wantName: "Creator Only", wantEmail: "",
			wantRemoteID: "src-id", wantDriveID: "src-drive",
			wantIsFolder: true, wantChildCnt: 0,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dir driveItemResponse
			require.NoError(t, json.Unmarshal([]byte(tt.json), &dir))
			item := dir.toItem(logger)

			assert.Equal(t, tt.wantName, item.SharedOwnerName)
			assert.Equal(t, tt.wantEmail, item.SharedOwnerEmail)
			assert.Equal(t, tt.wantRemoteID, item.RemoteItemID)
			assert.Equal(t, tt.wantDriveID, item.RemoteDriveID)
			assert.Equal(t, tt.wantIsFolder, item.IsFolder)
			assert.Equal(t, tt.wantChildCnt, item.ChildCount)
		})
	}
}

func TestToItem_SharedWithMe_NilRemoteItem(t *testing.T) {
	// Regular items have no remoteItem — fields should be empty.
	raw := `{
		"id": "regular-item",
		"name": "file.txt",
		"size": 1024,
		"createdDateTime": "2024-01-01T00:00:00Z",
		"lastModifiedDateTime": "2024-01-01T00:00:00Z",
		"file": {"mimeType": "text/plain"}
	}`

	var dir driveItemResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &dir))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := dir.toItem(logger)

	assert.Empty(t, item.RemoteItemID)
	assert.Empty(t, item.RemoteDriveID)
	assert.Empty(t, item.SharedOwnerName)
	assert.Empty(t, item.SharedOwnerEmail)
}

func TestToItem_SharedWithMe_NilSharedOwner(t *testing.T) {
	// shared facet present but owner is nil (edge case).
	raw := `{
		"id": "item-partial-shared",
		"name": "Partial",
		"size": 0,
		"createdDateTime": "2024-01-01T00:00:00Z",
		"lastModifiedDateTime": "2024-01-01T00:00:00Z",
		"shared": {}
	}`

	var dir driveItemResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &dir))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := dir.toItem(logger)

	assert.Empty(t, item.SharedOwnerName)
	assert.Empty(t, item.SharedOwnerEmail)
}

func TestToItem_RemoteItem_NilParentReference(t *testing.T) {
	// remoteItem present but parentReference is nil.
	raw := `{
		"id": "item-remote-no-parent",
		"name": "Remote",
		"size": 0,
		"createdDateTime": "2024-01-01T00:00:00Z",
		"lastModifiedDateTime": "2024-01-01T00:00:00Z",
		"remoteItem": {
			"id": "remote-id"
		}
	}`

	var dir driveItemResponse
	require.NoError(t, json.Unmarshal([]byte(raw), &dir))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	item := dir.toItem(logger)

	assert.Equal(t, "remote-id", item.RemoteItemID)
	assert.Empty(t, item.RemoteDriveID)
}

// --- CopyItem tests ---

func TestCopyItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-to-copy/copy", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req copyItemRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "dest-folder-id", req.ParentReference.ID)
		assert.Equal(t, "copy-of-file.txt", req.Name)

		w.Header().Set("Location", "https://monitor.example.com/status/abc")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	result, err := client.CopyItem(t.Context(), driveid.New("d"), "item-to-copy", "dest-folder-id", "copy-of-file.txt")
	require.NoError(t, err)

	assert.Equal(t, "https://monitor.example.com/status/abc", result.MonitorURL)
}

func TestCopyItem_NoName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req copyItemRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Empty(t, req.Name)
		assert.Equal(t, "dest-id", req.ParentReference.ID)

		w.Header().Set("Location", "https://monitor.example.com/status/def")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	result, err := client.CopyItem(t.Context(), driveid.New("d"), "item-1", "dest-id", "")
	require.NoError(t, err)

	assert.Equal(t, "https://monitor.example.com/status/def", result.MonitorURL)
}

func TestCopyItem_MissingLocationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CopyItem(t.Context(), driveid.New("d"), "item-1", "dest-id", "name.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing Location header")
}

func TestCopyItem_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-copy-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CopyItem(t.Context(), driveid.New("d"), "nonexistent", "dest-id", "name.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPollCopyStatus_Completed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"status": "completed",
			"percentageComplete": 100.0,
			"resourceId": "new-item-id"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	status, err := client.PollCopyStatus(t.Context(), srv.URL+"/monitor/abc")
	require.NoError(t, err)

	assert.Equal(t, "completed", status.Status)
	assert.Equal(t, 100.0, status.PercentageComplete)
	assert.Equal(t, "new-item-id", status.ResourceID)
}

func TestPollCopyStatus_InProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"status": "inProgress",
			"percentageComplete": 45.5,
			"resourceId": ""
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	status, err := client.PollCopyStatus(t.Context(), srv.URL+"/monitor/def")
	require.NoError(t, err)

	assert.Equal(t, "inProgress", status.Status)
	assert.Equal(t, 45.5, status.PercentageComplete)
	assert.Empty(t, status.ResourceID)
}

func TestPollCopyStatus_Failed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"status": "failed",
			"percentageComplete": 0,
			"resourceId": ""
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	status, err := client.PollCopyStatus(t.Context(), srv.URL+"/monitor/fail")
	require.NoError(t, err)

	assert.Equal(t, "failed", status.Status)
}

// --- ListChildrenRecursive tests ---

func TestListChildrenRecursive_FlatFolder(t *testing.T) {
	// Folder with only files, no subfolders.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/drives/000000000000000d/items/folder-root/children")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"value": [
				{"id":"f1","name":"a.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-root","driveId":"d"},"file":{"mimeType":"text/plain"},"size":100},
				{"id":"f2","name":"b.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"folder-root","driveId":"d"},"file":{"mimeType":"text/plain"},"size":200}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "folder-root")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "a.txt", items[0].Name)
	assert.Equal(t, "b.txt", items[1].Name)
}

func TestListChildrenRecursive_NestedFolders(t *testing.T) {
	// Root has file + subfolder; subfolder has a file.
	// Server dispatches based on parent ID in the path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if strings.Contains(r.URL.Path, "/items/root-folder/children") {
			fmt.Fprint(w, `{
				"value": [
					{"id":"f1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"file":{"mimeType":"text/plain"}},
					{"id":"sub1","name":"subdir","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"folder":{"childCount":1}}
				]
			}`)
		} else if strings.Contains(r.URL.Path, "/items/sub1/children") {
			fmt.Fprint(w, `{
				"value": [
					{"id":"f2","name":"nested.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"sub1","driveId":"d"},"file":{"mimeType":"text/plain"}}
				]
			}`)
		} else {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "root-folder")
	require.NoError(t, err)

	// Should include the subfolder itself + all files (3 total).
	assert.Len(t, items, 3)

	// Verify we got all items (order may vary due to recursion).
	ids := make(map[string]bool)
	for _, item := range items {
		ids[item.ID] = true
	}

	assert.True(t, ids["f1"], "should contain file1")
	assert.True(t, ids["sub1"], "should contain subfolder")
	assert.True(t, ids["f2"], "should contain nested file")
}

func TestListChildrenRecursive_DeeplyNested(t *testing.T) {
	// root -> sub1 -> sub2 -> file.txt (3 levels deep)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		switch {
		case strings.Contains(r.URL.Path, "/items/root-folder/children"):
			fmt.Fprint(w, `{
				"value": [
					{"id":"sub1","name":"level1","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"folder":{"childCount":1}}
				]
			}`)
		case strings.Contains(r.URL.Path, "/items/sub1/children"):
			fmt.Fprint(w, `{
				"value": [
					{"id":"sub2","name":"level2","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"sub1","driveId":"d"},"folder":{"childCount":1}}
				]
			}`)
		case strings.Contains(r.URL.Path, "/items/sub2/children"):
			fmt.Fprint(w, `{
				"value": [
					{"id":"f1","name":"deep.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"sub2","driveId":"d"},"file":{"mimeType":"text/plain"}}
				]
			}`)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "root-folder")
	require.NoError(t, err)

	// sub1, sub2, and deep.txt
	assert.Len(t, items, 3)
}

func TestListChildrenRecursive_EmptyFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "empty-folder")
	require.NoError(t, err)

	assert.Empty(t, items)
}

func TestListChildrenRecursive_WithPagination(t *testing.T) {
	// Root folder children are paginated across two pages.
	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if strings.Contains(r.URL.Path, "/items/root-folder/children") && !strings.Contains(r.URL.RawQuery, "page=2") {
			fmt.Fprintf(w, `{
				"value": [
					{"id":"f1","name":"file1.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"file":{"mimeType":"text/plain"}}
				],
				"@odata.nextLink": "%s/drives/d/items/root-folder/children?$top=200&page=2"
			}`, srv.URL)
		} else if strings.Contains(r.URL.RawQuery, "page=2") {
			fmt.Fprint(w, `{
				"value": [
					{"id":"f2","name":"file2.txt","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"file":{"mimeType":"text/plain"}}
				]
			}`)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "root-folder")
	require.NoError(t, err)

	assert.Len(t, items, 2)
	assert.Equal(t, "f1", items[0].ID)
	assert.Equal(t, "f2", items[1].ID)
}

func TestListChildrenRecursive_ErrorPropagation(t *testing.T) {
	// Root has a subfolder; listing the subfolder's children returns an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/items/root-folder/children") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{
				"value": [
					{"id":"sub1","name":"subdir","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"root-folder","driveId":"d"},"folder":{"childCount":1}}
				]
			}`)
		} else {
			w.Header().Set("request-id", "req-err")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"code":"internalError","message":"something broke"}}`)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "root-folder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestListChildrenRecursive_MaxDepth(t *testing.T) {
	origMax := maxRecursionDepth
	maxRecursionDepth = 2
	defer func() { maxRecursionDepth = origMax }()

	// Each folder contains one subfolder — will exceed depth 2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Always return a single subfolder, creating infinite depth.
		fmt.Fprint(w, `{
			"value": [
				{"id":"sub","name":"deep","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","parentReference":{"id":"p","driveId":"d"},"folder":{"childCount":1}}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.ListChildrenRecursive(t.Context(), driveid.New("d"), "root-folder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded max depth")
	assert.Contains(t, err.Error(), "2")
}

// ---------------------------------------------------------------------------
// ListItemPermissions + HasWriteAccess tests
// ---------------------------------------------------------------------------

func TestListItemPermissions_ReadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/permissions")

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value": [{"id": "perm-1", "roles": ["read"]}]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	perms, err := client.ListItemPermissions(t.Context(), driveid.New("drive-1"), "item-1")
	require.NoError(t, err)
	require.Len(t, perms, 1)
	assert.Equal(t, []string{"read"}, perms[0].Roles)
	assert.False(t, HasWriteAccess(perms))
}

func TestListItemPermissions_Writable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value": [{"id": "perm-1", "roles": ["read", "write"]}]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	perms, err := client.ListItemPermissions(t.Context(), driveid.New("drive-1"), "item-1")
	require.NoError(t, err)
	require.Len(t, perms, 1)
	assert.True(t, HasWriteAccess(perms))
}

func TestListItemPermissions_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound","message":"not found"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.ListItemPermissions(t.Context(), driveid.New("drive-1"), "item-1")
	require.Error(t, err)
}

func TestHasWriteAccess(t *testing.T) {
	tests := []struct {
		name  string
		perms []Permission
		want  bool
	}{
		{"empty", nil, false},
		{"read only", []Permission{{Roles: []string{"read"}}}, false},
		{"write", []Permission{{Roles: []string{"write"}}}, true},
		{"owner", []Permission{{Roles: []string{"owner"}}}, true},
		{"mixed read and write", []Permission{{Roles: []string{"read"}}, {Roles: []string{"write"}}}, true},
		{"member only", []Permission{{Roles: []string{"member"}}}, false},
		{"owner in mixed", []Permission{{Roles: []string{"read", "owner"}}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HasWriteAccess(tt.perms))
		})
	}
}

// --- ListRecycleBinItems tests ---

func TestListRecycleBinItems_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Contains(t, r.URL.Path, "/drives/000000000000000d/special/recyclebin/children")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"value": [
				{
					"id": "deleted-1",
					"name": "old-file.txt",
					"size": 1024,
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-01-01T00:00:00Z",
					"deleted": {},
					"file": {"mimeType": "text/plain"}
				},
				{
					"id": "deleted-2",
					"name": "old-folder",
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-01-01T00:00:00Z",
					"deleted": {},
					"folder": {"childCount": 3}
				}
			]
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListRecycleBinItems(t.Context(), driveid.New("d"))
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "deleted-1", items[0].ID)
	assert.Equal(t, "old-file.txt", items[0].Name)
	assert.True(t, items[0].IsDeleted)
	assert.Equal(t, "deleted-2", items[1].ID)
	assert.True(t, items[1].IsFolder)
	assert.True(t, items[1].IsDeleted)
}

func TestListRecycleBinItems_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value": []}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	items, err := client.ListRecycleBinItems(t.Context(), driveid.New("d"))
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestListRecycleBinItems_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-rb-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.ListRecycleBinItems(t.Context(), driveid.New("d"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- RestoreItem tests ---

func TestRestoreItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/deleted-1/restore", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "restored-1",
			"name": "old-file.txt",
			"size": 1024,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "root-id", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.RestoreItem(t.Context(), driveid.New("d"), "deleted-1")
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "restored-1", item.ID)
	assert.Equal(t, "old-file.txt", item.Name)
}

func TestRestoreItem_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-restore-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.RestoreItem(t.Context(), driveid.New("d"), "nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRestoreItem_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-restore-409")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"nameAlreadyExists"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.RestoreItem(t.Context(), driveid.New("d"), "conflict-item")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConflict)
}
