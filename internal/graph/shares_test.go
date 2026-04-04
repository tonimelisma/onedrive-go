package graph

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveShareURL_Success(t *testing.T) {
	rawURL := "https://1drv.ms/t/c/example/abcdef?e=xyz"
	expectedToken := "u!" + base64.RawURLEncoding.EncodeToString([]byte(rawURL))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/shares/"+expectedToken+"/driveItem", r.URL.Path)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "owner-item-123",
			"name": "Shared Document.docx",
			"size": 42,
			"createdDateTime": "2026-04-03T00:00:00Z",
			"lastModifiedDateTime": "2026-04-03T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "b!AbC12345678901"},
			"file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.ResolveShareURL(t.Context(), rawURL)
	require.NoError(t, err)

	assert.Equal(t, "owner-item-123", item.ID)
	assert.Equal(t, "shared document.docx", strings.ToLower(item.Name))
	assert.Equal(t, "b!abc12345678901", item.RemoteDriveID)
	assert.Equal(t, "owner-item-123", item.RemoteItemID)
}

func TestResolveShareURL_PreservesRemoteItemIdentity(t *testing.T) {
	rawURL := "https://contoso.sharepoint.com/:x:/g/personal/alice/example"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "receiver-wrapper",
			"name": "Shared Folder",
			"createdDateTime": "2026-04-03T00:00:00Z",
			"lastModifiedDateTime": "2026-04-03T00:00:00Z",
			"folder": {"childCount": 1},
			"parentReference": {"id": "wrapper-parent", "driveId": "wrapper-drive"},
			"remoteItem": {
				"id": "real-item-456",
				"parentReference": {"driveId": "real-drive-7890000"}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.ResolveShareURL(t.Context(), rawURL)
	require.NoError(t, err)

	assert.Equal(t, "real-drive-7890000", item.RemoteDriveID)
	assert.Equal(t, "real-item-456", item.RemoteItemID)
}

func TestResolveShareURL_Invalid(t *testing.T) {
	client := newTestClient(t, "http://unused")

	_, err := client.ResolveShareURL(t.Context(), "not a url")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use https")
}
