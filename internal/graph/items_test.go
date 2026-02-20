package graph

import (
	"context"
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
)

func TestGetItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/drive-abc-123/items/item-123", r.URL.Path)
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
	item, err := client.GetItem(context.Background(), "drive-abc-123", "item-123")
	require.NoError(t, err)

	assert.Equal(t, "item-123", item.ID)
	assert.Equal(t, "test-file.txt", item.Name)
	assert.Equal(t, "drive-abc-123", item.DriveID)
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
	item, err := client.GetItem(context.Background(), "drive-1", "folder-789")
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
	_, err := client.GetItem(context.Background(), "drive-1", "nonexistent")
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
	item, err := client.GetItem(context.Background(), "b!uppercase-drive-id", "item-1")
	require.NoError(t, err)

	assert.Equal(t, "b!uppercase-drive-id", item.DriveID)
	assert.Equal(t, "b!uppercase-drive-id", item.ParentDriveID)
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
	item, err := client.GetItem(context.Background(), "d", "item-ts")
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
	item, err := client.GetItem(context.Background(), "d", "item-future")
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
	item, err := client.GetItem(context.Background(), "d", "item-pkg")
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
	item, err := client.GetItem(context.Background(), "d", "root-item")
	require.NoError(t, err)

	assert.Empty(t, item.DriveID)
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
	item, err := client.GetItem(context.Background(), "d", "folder-1")
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
		assert.Contains(t, r.URL.Path, "/drives/d/items/parent/children")
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
	items, err := client.ListChildren(context.Background(), "d", "parent")
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
	items, err := client.ListChildren(context.Background(), "d", "p")
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
	items, err := client.ListChildren(context.Background(), "d", "empty-folder")
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
	items, err := client.ListChildren(context.Background(), "d", "p")
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
	_, err := client.ListChildren(context.Background(), "d", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match base URL")
}

// --- CreateFolder tests ---

func TestCreateFolder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/drives/d/items/parent/children", r.URL.Path)

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
	item, err := client.CreateFolder(context.Background(), "d", "parent", "New Folder")
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
	_, err := client.CreateFolder(context.Background(), "d", "parent", "Existing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConflict)
}

// --- MoveItem tests ---

func TestMoveItem_MoveAndRename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/d/items/item-1", r.URL.Path)

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
	item, err := client.MoveItem(context.Background(), "d", "item-1", "new-parent", "renamed.txt")
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
	item, err := client.MoveItem(context.Background(), "d", "item-1", "", "new-name.txt")
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
	item, err := client.MoveItem(context.Background(), "d", "item-1", "new-parent", "")
	require.NoError(t, err)

	assert.Equal(t, "new-parent", item.ParentID)
	assert.Equal(t, "unchanged.txt", item.Name)
}

func TestMoveItem_BothEmpty(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.MoveItem(context.Background(), "d", "item-1", "", "")
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
	_, err := client.MoveItem(context.Background(), "d", "nonexistent", "new-parent", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- DeleteItem tests ---

func TestDeleteItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/drives/d/items/item-to-delete", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.DeleteItem(context.Background(), "d", "item-to-delete")
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
	err := client.DeleteItem(context.Background(), "d", "nonexistent")
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

// testNoopLogger returns a logger that discards output, for unit tests that
// don't need log verification.
func testNoopLogger() *slog.Logger {
	return slog.Default()
}

// --- GetItemByPath tests ---

func TestGetItemByPath_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/d/root:/Documents/file.txt:", r.URL.Path)
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
	item, err := client.GetItemByPath(context.Background(), "d", "Documents/file.txt")
	require.NoError(t, err)

	assert.Equal(t, "item-path-1", item.ID)
	assert.Equal(t, "file.txt", item.Name)
	assert.Equal(t, int64(2048), item.Size)
	assert.Equal(t, "etag-path", item.ETag)
	assert.Equal(t, "ctag-path", item.CTag)
	assert.Equal(t, "d", item.DriveID) // normalized to lowercase
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
		assert.Contains(t, r.RequestURI, "/drives/d/root:/folder/my%20file%232.txt:")

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
	item, err := client.GetItemByPath(context.Background(), "d", "folder/my file#2.txt")
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
	_, err := client.GetItemByPath(context.Background(), "d", "nonexistent/path.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- ListChildrenByPath tests ---

func TestListChildrenByPath_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/drives/d/root:/Documents:/children", r.URL.Path)
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
	items, err := client.ListChildrenByPath(context.Background(), "d", "Documents")
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
	items, err := client.ListChildrenByPath(context.Background(), "d", "Documents")
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
	items, err := client.ListChildrenByPath(context.Background(), "d", "EmptyFolder")
	require.NoError(t, err)

	assert.Empty(t, items)
}

// --- Path validation tests ---

func TestGetItemByPath_EmptyPath(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.GetItemByPath(context.Background(), "d", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestGetItemByPath_LeadingSlash(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.GetItemByPath(context.Background(), "d", "/foo/bar.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestListChildrenByPath_EmptyPath(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.ListChildrenByPath(context.Background(), "d", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestListChildrenByPath_LeadingSlash(t *testing.T) {
	client := newTestClient(t, "http://localhost")
	_, err := client.ListChildrenByPath(context.Background(), "d", "/Documents")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPath)
}
