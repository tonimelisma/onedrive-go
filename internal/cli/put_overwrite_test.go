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

// putOverwriteServer is a Graph fake whose folder "parent" already holds the
// item existing: the create answers 409, a read of the one name answers the
// item under its stored spelling, a listing of the folder fails the test, and
// the overwrite by ID must carry If-Match.
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
			assert.Fail(t, "the folder was listed after a 409", "one read of the name finds the item")
			w.WriteHeader(http.StatusTeapot)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/drives/000000000000000d/items/parent:/"):
			facet := `"file":{}`
			if isFolder {
				facet = `"folder":{"childCount":0}`
			}
			writePutTestBody(t, w, []byte(`{"id":"existing","name":"`+existing+`","eTag":"\"e,3\"",`+facet+
				`,"parentReference":{"id":"parent"}}`))
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
	// semantics must go by the existing item's ID, conditionally. The item is
	// found by one read of its name, under the spelling OneDrive stores.
	srv, log := putOverwriteServer(t, "STRASSE.txt", false)
	client := newTestGraphClient(t, srv.URL)
	tm := driveops.NewTransferManager(client, client, nil, slog.Default())

	local := filepath.Join(t.TempDir(), "straße.txt")
	require.NoError(t, os.WriteFile(local, []byte("new"), 0o600))

	_, err := putFile(t.Context(), client, tm, driveid.New("d"), "parent", "stra\u00dfe.txt", local, driveops.UploadOpts{})
	require.ErrorIs(t, err, graph.ErrPreconditionFailed)
	assert.Contains(t, *log, `POST /drives/000000000000000d/items/existing/createUploadSession if-match="e,3"`,
		"the overwrite goes to the existing item, conditional on the eTag just read")
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

// putListingServer is a Graph fake whose folder "parent" holds the files
// listed: the create answers 409, the read of the name finds nothing (the
// path route lags behind the listing), the listing holds the files, and an
// overwrite by ID is answered 412.
func putListingServer(t *testing.T, listed string) (*httptest.Server, *[]string) {
	t.Helper()

	var mu stdsync.Mutex
	var log []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		mu.Lock()
		log = append(log, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/children"):
			w.WriteHeader(http.StatusConflict)
			writePutTestBody(t, w, []byte(`{"error":{"code":"nameAlreadyExists","message":"Name already exists"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			writePutTestBody(t, w, []byte(`{"value":[`+listed+`]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/drives/000000000000000d/items/parent:/"):
			w.WriteHeader(http.StatusNotFound)
			writePutTestBody(t, w, []byte(`{"error":{"code":"itemNotFound","message":"not yet"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/createUploadSession"):
			w.WriteHeader(http.StatusPreconditionFailed)
			writePutTestBody(t, w, []byte(`{"error":{"code":"resourceModified"}}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &log
}

// A folder whose listing holds the name as spelled and another spelling
// OneDrive treats as the same overwrites the one spelled as asked; one that
// holds two other spellings and not this one overwrites neither and answers
// the service's conflict.
//
// Validates: R-1.3.8, R-2.12.3
func TestPutFile_PrefersExactSpellingOverCaseVariant(t *testing.T) {
	t.Parallel()

	file := func(id, name string) string {
		return `{"id":"` + id + `","name":"` + name + `","eTag":"\"e,1\"","file":{},"parentReference":{"id":"parent"}}`
	}

	for _, tc := range []struct {
		label  string
		listed string
		wantTo string
	}{
		{label: "exact after a case variant", listed: file("upper", "A.TXT") + "," + file("exact", "a.txt"), wantTo: "exact"},
		{label: "two case variants", listed: file("upper", "A.TXT") + "," + file("title", "A.txt")},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			srv, log := putListingServer(t, tc.listed)
			client := newTestGraphClient(t, srv.URL)
			tm := driveops.NewTransferManager(client, client, nil, slog.Default())

			local := filepath.Join(t.TempDir(), "a.txt")
			require.NoError(t, os.WriteFile(local, []byte("new"), 0o600))

			_, err := putFile(t.Context(), client, tm, driveid.New("d"), "parent", "a.txt", local, driveops.UploadOpts{})
			if tc.wantTo == "" {
				require.ErrorIs(t, err, graph.ErrConflict)
				for _, request := range *log {
					assert.NotContains(t, request, "/items/upper/")
					assert.NotContains(t, request, "/items/title/")
				}
				return
			}
			require.ErrorIs(t, err, graph.ErrPreconditionFailed)
			assert.Contains(t, *log, "POST /drives/000000000000000d/items/"+tc.wantTo+"/createUploadSession")
		})
	}
}
