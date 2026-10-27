package cli

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// putOverwriteServer is a Graph fake whose folder already holds name: the
// create answers 409, and the overwrite by ID must carry If-Match.
func putOverwriteServer(t *testing.T, existing string, isFolder bool) (*httptest.Server, *[]string) {
	t.Helper()

	var mu stdsync.Mutex
	var log []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path+" if-match="+r.Header.Get("If-Match"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/children"):
			w.WriteHeader(http.StatusConflict)
			writePutTestBody(t, w, []byte(`{"error":{"code":"nameAlreadyExists","message":"Name already exists"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			facet := `"file":{}`
			if isFolder {
				facet = `"folder":{"childCount":0}`
			}
			writePutTestBody(t, w, []byte(`{"value":[{"id":"existing","name":"`+existing+`","eTag":"\"e,3\"",`+facet+`}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items/existing/createUploadSession"):
			// Someone edited it after the listing: the conditional
			// overwrite must refuse rather than replace that edit.
			w.WriteHeader(http.StatusPreconditionFailed)
			writePutTestBody(t, w, []byte(`{"error":{"code":"resourceModified"}}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &log
}

func writePutTestBody(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()
	_, err := w.Write(body)
	assert.NoError(t, err)
}

// Validates: R-1.3.8
func TestPutFile_ExistingFileIsOverwrittenByIDConditionally(t *testing.T) {
	t.Parallel()

	// A create never replaces, so put's "upload over an existing file"
	// semantics must go by the existing item's ID, conditionally.
	srv, log := putOverwriteServer(t, "STRASSE.txt", false)
	client := newTestGraphClient(t, srv.URL)
	tm := driveops.NewTransferManager(client, client, nil, slog.Default())

	local := filepath.Join(t.TempDir(), "straße.txt")
	require.NoError(t, os.WriteFile(local, []byte("new"), 0o600))

	_, err := putFile(t.Context(), client, tm, driveid.New("d"), "parent", "stra\u00dfe.txt", local, driveops.UploadOpts{})
	require.ErrorIs(t, err, graph.ErrPreconditionFailed)
	assert.Contains(t, *log, `POST /drives/000000000000000d/items/existing/createUploadSession if-match="e,3"`,
		"the overwrite goes to the existing item, conditional on the eTag just listed")
}

// Validates: R-1.3.8
func TestPutFile_ExistingFolderIsAnError(t *testing.T) {
	t.Parallel()

	srv, _ := putOverwriteServer(t, "report", true)
	client := newTestGraphClient(t, srv.URL)
	tm := driveops.NewTransferManager(client, client, nil, slog.Default())

	local := filepath.Join(t.TempDir(), "report")
	require.NoError(t, os.WriteFile(local, []byte("new"), 0o600))

	_, err := putFile(t.Context(), client, tm, driveid.New("d"), "parent", "report", local, driveops.UploadOpts{})
	require.ErrorContains(t, err, "is a folder")
}
