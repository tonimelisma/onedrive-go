package graph

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// testUploadSessionPath is where the fake servers here serve upload fragments.
const testUploadSessionPath = "/upload-session"

// Validates: R-5.6.14
func TestCreateFile_NameTravelsInTheBodyWithFail(t *testing.T) {
	// Live probe 4: `v1.0` uploaded through items/{parent}:/v1.0:/content
	// created a folder `_api` holding `v2.0`. Names Graph's router or a front
	// end might reinterpret must never reach a URL.
	names := []string{"v1.0", "a+b.txt", "01 - 2 + 2 = 5.mp3", "$x", "root", "delta", "a%20b.txt", "x;y,z@w.txt"}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/drives/000000000000000d/items/parent/children", r.URL.EscapedPath())

				var body map[string]any
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, name, body["name"])
				assert.Equal(t, "fail", body["@microsoft.graph.conflictBehavior"])
				assert.Equal(t, map[string]any{}, body["file"])

				serveCreateFile(t, w, r, "new-id")
			}))
			defer srv.Close()

			item, err := newTestClient(t, srv.URL).CreateFile(t.Context(), driveid.New("d"), "parent", name)
			require.NoError(t, err)
			assert.Equal(t, "new-id", item.ID)
		})
	}
}

// Validates: R-5.6.14
func TestUpload_ExistingNameIsNeverReplaced(t *testing.T) {
	// A name OneDrive already holds (or holds as an equivalent spelling such
	// as STRASSE.txt for straße.txt) answers 409; the upload stops there
	// instead of overwriting the other file.
	var other atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/children") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			writeTestResponse(t, w, `{"error":{"code":"nameAlreadyExists","message":"Name already exists"}}`)

			return
		}
		other.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	content := []byte("mine")
	_, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "STRASSE.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.ErrorIs(t, err, ErrConflict)
	assert.Zero(t, other.Load(), "nothing may follow a refused create")
}

// Validates: R-5.6.14, R-5.6.2
func TestUpload_NewFileContentGoesByIDConditionalOnTheCreatedETag(t *testing.T) {
	rec := &sessionUploadRecorder{}
	srv := newSessionUploadServer(t, rec, http.StatusOK)
	defer srv.Close()

	content := []byte("hello")
	item, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "v1.0",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "small-item", item.ID)

	require.Len(t, rec.createBodies, 1)
	assert.Equal(t, []string{"/drives/000000000000000d/items/small-item/createUploadSession"}, rec.sessionURLs)
	assert.Equal(t, []string{createdFileETag}, rec.sessionMatch,
		"the session may only fill the item this upload created")
	assert.Equal(t, []string{string(content)}, rec.chunkBodies)
	assert.Empty(t, rec.deleteIfMatch)
}

// Validates: R-5.6.14
func TestUpload_FailedContentStepRemovesItsEmptyItemConditionally(t *testing.T) {
	rec := &sessionUploadRecorder{}
	srv := newSessionUploadServer(t, rec, http.StatusForbidden)
	defer srv.Close()

	content := []byte("blocked")
	_, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "blocked.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.ErrorIs(t, err, ErrForbidden)

	// Left behind, the empty item would meet the retry as a conflicting file.
	assert.Equal(t, []string{createdFileETag}, rec.deleteIfMatch)
}

// Validates: R-5.6.2
func TestUploadToItem_OverwriteIsConditionalOnBothRoutes(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "zero-byte PUT", content: nil},
		{name: "session", content: []byte("new content")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ifMatch []string
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				assert.NoError(t, err)
				w.Header().Set("Content-Type", "application/json")

				switch {
				case r.URL.Path == testUploadSessionPath:
					w.WriteHeader(http.StatusCreated)
					writeTestResponse(t, w, `{"id":"item-1","name":"a.txt","size":11,"file":{}}`)
				case strings.HasSuffix(r.URL.Path, "createUploadSession"):
					ifMatch = append(ifMatch, r.Header.Get("If-Match"))
					w.WriteHeader(http.StatusOK)
					writeTestResponsef(t, w, `{"uploadUrl":"%s%s","expirationDateTime":"2030-01-01T00:00:00Z"}`, srv.URL, testUploadSessionPath)
				default:
					ifMatch = append(ifMatch, r.Header.Get("If-Match"))
					w.WriteHeader(http.StatusOK)
					writeTestResponse(t, w, `{"id":"item-1","name":"a.txt","size":0,"file":{}}`)
				}
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv.URL).UploadToItem(t.Context(), driveid.New("d"), "item-1", `"preflight"`,
				bytes.NewReader(tt.content), int64(len(tt.content)), time.Time{}, nil)
			require.NoError(t, err)
			assert.Equal(t, []string{`"preflight"`}, ifMatch)
		})
	}
}

// Validates: R-5.6.2
func TestUploadToItem_StaleETagIsPreconditionFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"id":"item-1","name":"a.txt","eTag":"\"someone-else\"","file":{}}`)

			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
		writeTestResponse(t, w, `{"error":{"code":"resourceModified"}}`)
	}))
	defer srv.Close()

	content := []byte("mine")
	_, err := newTestClient(t, srv.URL).UploadToItem(t.Context(), driveid.New("d"), "item-1", `"preflight"`,
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.ErrorIs(t, err, ErrPreconditionFailed)
}

// Validates: R-5.6.2
func TestCreateUploadSessionForItem_PreconditionFailureIsNeverRetriedUnconditionally(t *testing.T) {
	// Even when the item still carries the sent eTag, a 412 is the answer:
	// retrying without If-Match could overwrite a concurrent edit, and the
	// "eTag changes during session creation" report is unprobed on Business.
	var posts atomic.Int32
	var gets atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"id":"item-1","name":"a.txt","eTag":"\"same\"","file":{}}`)

			return
		}

		posts.Add(1)
		w.WriteHeader(http.StatusPreconditionFailed)
		writeTestResponse(t, w, `{"error":{"code":"resourceModified"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).CreateUploadSessionForItem(t.Context(), driveid.New("d"), "item-1", `"same"`, 10, time.Time{})
	require.ErrorIs(t, err, ErrPreconditionFailed)
	assert.Equal(t, int32(1), posts.Load())
	assert.Zero(t, gets.Load())
}
