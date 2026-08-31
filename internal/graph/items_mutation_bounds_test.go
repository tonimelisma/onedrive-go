package graph

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.7.30
//
// Every other response in this package is streamed or already bounded. The
// create-folder body is buffered whole so an empty success body can be told
// apart from a real item, which made it the one success path where a server
// could dictate the client's memory use.
func TestCreateFolder_OversizedResponseIsRejectedNotBuffered(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)

		// Valid JSON so the rejection is the size guard rather than a decode
		// failure.
		writeTestResponse(t, w, `{"id": "`+strings.Repeat("A", maxItemResponseSize+1024)+`"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	item, err := client.CreateFolder(t.Context(), driveid.New("d"), "parent", "New Folder")
	require.Error(t, err)
	assert.Nil(t, item)
	assert.Contains(t, err.Error(), "exceeds")
}

// Validates: R-6.7.30
//
// The cap must not clip ordinary responses: a normal driveItem is orders of
// magnitude below it.
func TestCreateFolder_NormalResponseIsWellUnderTheCap(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "new-folder-id",
			"name": "New Folder",
			"parentReference": {"id": "parent", "driveId": "d"},
			"folder": {"childCount": 0}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	item, err := client.CreateFolder(t.Context(), driveid.New("d"), "parent", "New Folder")
	require.NoError(t, err)
	assert.Equal(t, "new-folder-id", item.ID)
}
