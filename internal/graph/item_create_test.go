package graph

import (
	"bytes"
	"context"
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

// Validates: R-6.8.21
func TestCreate_TransientFailureIsResentByARetryingTransport(t *testing.T) {
	// Live, e2e TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges: one 503 on
	// the file create failed `put` outright. A create with
	// conflictBehavior=fail is safe to resend, so a retrying transport does;
	// without one (sync) the 503 comes back after one attempt.
	tests := []struct {
		name   string
		create func(c *Client) (*Item, error)
	}{
		{name: "file", create: func(c *Client) (*Item, error) {
			return c.CreateFile(t.Context(), driveid.New("d"), "parent", "a.txt")
		}},
		{name: "folder", create: func(c *Client) (*Item, error) {
			return c.CreateFolder(t.Context(), driveid.New("d"), "parent", "a")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusTeapot)
					return
				}
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Contains(t, string(body), `"@microsoft.graph.conflictBehavior":"fail"`)
				if posts.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				writeTestResponse(t, w, `{"id":"new-id","name":"a","eTag":"e1"}`)
			}))
			defer srv.Close()

			item, err := tt.create(newTestClient(t, srv.URL))
			require.NoError(t, err)
			assert.Equal(t, "new-id", item.ID)
			assert.Equal(t, int32(2), posts.Load())

			posts.Store(0)
			_, err = tt.create(newNoRetryTestClient(t, srv.URL))
			require.ErrorIs(t, err, ErrServerError)
			assert.Equal(t, int32(1), posts.Load(), "a transport that retries nothing sends the create once")
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

// Validates: R-5.6.14, R-5.6.15
//
// Live, 2026-11-02: OneDrive refused the create of a path it counts as too
// long with 400 invalidRequest. A refused create made nothing: the record
// hook does not run, no session or content follows, nothing is read back or
// deleted, and the failure is a definite refusal rather than an unknown
// outcome, so sync does not keep an intent for it.
func TestUpload_RefusedCreateSendsNoContentAndDeletesNothing(t *testing.T) {
	rec := &sessionUploadRecorder{createStatus: http.StatusBadRequest}
	srv := newSessionUploadServer(t, rec, http.StatusOK)
	defer srv.Close()

	content := []byte("my words")
	hookRan := false
	_, err := newTestClient(t, srv.URL).UploadCreating(t.Context(), driveid.New("d"), "parent", "doc.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
		func(*Item) error {
			hookRan = true
			return nil
		})

	require.ErrorIs(t, err, ErrBadRequest)
	require.NotErrorIs(t, err, ErrCreateOutcomeUnknown, "a refusal says the create did not happen")
	var created *CreatedItemError
	require.NotErrorAs(t, err, &created, "no item exists to name")
	assert.False(t, hookRan, "nothing to record")
	assert.Len(t, rec.createBodies, 1, "a refusal is not resent")
	assert.Empty(t, rec.sessionURLs)
	assert.Empty(t, rec.chunkBodies)
	assert.Zero(t, rec.simplePuts)
	assert.Zero(t, rec.reads)
	assert.Empty(t, rec.deleteIfMatch)
	assert.Empty(t, rec.other)
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

// Validates: R-5.6.14
//
// An upload session opened on the empty item moves its eTag although no
// content landed (live, e2e TestE2E_SyncWatch_FileModification). Conditional
// on the creation eTag, the delete failed with 412 and the placeholder stayed;
// it is conditional on the eTag read back, and only while the item is empty.
func TestUpload_FailedContentStepRemovesItsEmptyItemByItsCurrentETag(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		deleted []string
	}{
		{name: "still empty under a moved eTag", deleted: []string{`"moved-etag"`}},
		{name: "content landed meanwhile", size: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sessionUploadRecorder{currentETag: `"moved-etag"`, currentSize: tc.size}
			srv := newSessionUploadServer(t, rec, http.StatusForbidden)
			defer srv.Close()

			content := []byte("blocked")
			_, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "blocked.txt",
				bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
			require.ErrorIs(t, err, ErrForbidden)

			assert.Equal(t, tc.deleted, rec.deleteIfMatch)
		})
	}
}

// failUploadAfterCreate runs an upload whose session request is refused, so
// the client is left with the empty item it created, and returns the
// upload's error.
func failUploadAfterCreate(t *testing.T, rec *sessionUploadRecorder) error {
	t.Helper()

	srv := newSessionUploadServer(t, rec, http.StatusForbidden)
	defer srv.Close()

	content := []byte("blocked")
	_, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "blocked.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.ErrorIs(t, err, ErrForbidden)

	return err
}

// Validates: R-5.6.14
//
// Nightly run 42865435904, e2e TestE2E_SyncWatch_PauseResume: the cleanup's
// one delete, conditional on the eTag it had just read, answered 412 and the
// empty file stayed on OneDrive. The session request the client had given up
// on most likely still reached the server and moved the eTag after that read
// (inferred from the order of the requests). A 412 is answered by reading
// the item again.
func TestUpload_FailedContentStepReadsAgainWhenItsDeleteIsRefused(t *testing.T) {
	rec := &sessionUploadRecorder{
		readBacks:      []createdItemReadBack{{etag: `"read-1"`}, {etag: `"read-2"`}},
		deleteStatuses: []int{http.StatusPreconditionFailed},
	}

	require.Error(t, failUploadAfterCreate(t, rec))

	assert.Equal(t, []string{`"read-1"`, `"read-2"`}, rec.deleteIfMatch,
		"each delete is conditional on the eTag its own attempt read")
	assert.Equal(t, 2, rec.reads)
}

// Validates: R-5.6.14
func TestUpload_FailedContentStepLeavesAnItemThatGainedContentBetweenAttempts(t *testing.T) {
	// The refused delete may mean another writer filled the item. The second
	// read shows content, so nothing more is deleted.
	rec := &sessionUploadRecorder{
		readBacks:      []createdItemReadBack{{etag: `"read-1"`}, {etag: `"read-2"`, size: 7}},
		deleteStatuses: []int{http.StatusPreconditionFailed},
	}

	require.Error(t, failUploadAfterCreate(t, rec))

	assert.Equal(t, []string{`"read-1"`}, rec.deleteIfMatch)
	assert.Equal(t, 2, rec.reads)
}

// Validates: R-5.6.14
func TestUpload_FailedContentStepStopsAfterThreeRefusedDeletes(t *testing.T) {
	// One more scripted refusal than the limit: a fourth attempt would show.
	refused := http.StatusPreconditionFailed
	rec := &sessionUploadRecorder{deleteStatuses: []int{refused, refused, refused, refused}}

	err := failUploadAfterCreate(t, rec)

	assert.Len(t, rec.deleteIfMatch, 3)
	assert.Equal(t, 3, rec.reads)

	// The upload's own failure is what the caller gets, with the item that
	// was left, never the cleanup's.
	var created *CreatedItemError
	require.ErrorAs(t, err, &created)
	assert.Equal(t, "small-item", created.Item.ID)
	assert.NotErrorIs(t, err, ErrPreconditionFailed)
}

// Validates: R-5.6.14
func TestUpload_FailedContentStepStopsWhenItsEmptyItemIsAlreadyGone(t *testing.T) {
	rec := &sessionUploadRecorder{
		readBacks:      []createdItemReadBack{{etag: `"read-1"`}, {status: http.StatusNotFound}},
		deleteStatuses: []int{http.StatusPreconditionFailed},
	}

	require.Error(t, failUploadAfterCreate(t, rec))

	assert.Equal(t, []string{`"read-1"`}, rec.deleteIfMatch)
	assert.Equal(t, 2, rec.reads)
}

// Validates: R-5.6.14
//
// The sequence of 2026-11-10 (LI-20261110-01) through the HTTP fake, on sync's
// transport, which resends nothing: the session request reaches OneDrive and
// opens the session, which moves the empty item's eTag, and its answer is
// lost. The cleanup's first read still shows the creation eTag (the session
// landed after it), so that delete is refused with 412; the second read shows
// the moved eTag, and its delete removes the empty item.
func TestUpload_SessionRequestThatLandsWithoutAnAnswerStillRemovesTheEmptyItem(t *testing.T) {
	rec := &sessionUploadRecorder{
		sessionLandsThenDrops: true,
		readBacks:             []createdItemReadBack{{etag: createdFileETag}},
		deleteStatuses:        []int{http.StatusPreconditionFailed},
	}
	srv := newSessionUploadServer(t, rec, http.StatusOK)
	defer srv.Close()

	content := []byte("my words")
	_, err := newNoRetryTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "doc.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)

	var created *CreatedItemError
	require.ErrorAs(t, err, &created, "the failure names the item the create made")
	assert.Equal(t, "small-item", created.Item.ID)
	assert.False(t, IsFinalFragmentUncertain(err), "no content was sent")
	assert.Len(t, rec.sessionURLs, 1, "the session request is sent once")
	assert.Empty(t, rec.chunkBodies, "no content without a session answer")
	assert.Equal(t, []string{createdFileETag, sessionLandedETag}, rec.deleteIfMatch,
		"each delete is conditional on the eTag its own attempt read")
	assert.Equal(t, 2, rec.reads)
	assert.Empty(t, rec.other)
}

// Validates: R-5.6.14
func TestRemoveEmptyItem(t *testing.T) {
	refused := http.StatusPreconditionFailed

	for _, tc := range []struct {
		name        string
		rec         *sessionUploadRecorder
		wantRemoved bool
		wantErr     error
		wantDeletes []string
		wantReads   int
	}{
		{
			name:        "empty item is deleted by the eTag just read",
			rec:         &sessionUploadRecorder{currentETag: `"moved-etag"`},
			wantRemoved: true,
			wantDeletes: []string{`"moved-etag"`},
			wantReads:   1,
		},
		{
			name:        "already gone",
			rec:         &sessionUploadRecorder{readBacks: []createdItemReadBack{{status: http.StatusNotFound}}},
			wantRemoved: true,
			wantReads:   1,
		},
		{
			name:        "gone between the read and the delete",
			rec:         &sessionUploadRecorder{deleteStatuses: []int{http.StatusNotFound}},
			wantRemoved: true,
			wantDeletes: []string{createdFileETag},
			wantReads:   1,
		},
		{
			name:      "an item with content is never deleted",
			rec:       &sessionUploadRecorder{currentSize: 7},
			wantReads: 1,
		},
		{
			name:      "an item without an eTag is never deleted",
			rec:       &sessionUploadRecorder{readBacks: []createdItemReadBack{{}}},
			wantReads: 1,
		},
		{
			name: "a refused delete is answered by reading again",
			rec: &sessionUploadRecorder{
				readBacks:      []createdItemReadBack{{etag: `"read-1"`}, {etag: `"read-2"`}},
				deleteStatuses: []int{refused},
			},
			wantRemoved: true,
			wantDeletes: []string{`"read-1"`, `"read-2"`},
			wantReads:   2,
		},
		{
			name: "content that landed after a refused delete stays",
			rec: &sessionUploadRecorder{
				readBacks:      []createdItemReadBack{{etag: `"read-1"`}, {etag: `"read-2"`, size: 7}},
				deleteStatuses: []int{refused},
			},
			wantDeletes: []string{`"read-1"`},
			wantReads:   2,
		},
		{
			name:        "gives up after three refused deletes",
			rec:         &sessionUploadRecorder{deleteStatuses: []int{refused, refused, refused, refused}},
			wantErr:     ErrPreconditionFailed,
			wantDeletes: []string{createdFileETag, createdFileETag, createdFileETag},
			wantReads:   3,
		},
		{
			name:      "a failed read is reported, nothing is deleted",
			rec:       &sessionUploadRecorder{readBacks: []createdItemReadBack{{status: http.StatusForbidden}}},
			wantErr:   ErrForbidden,
			wantReads: 1,
		},
		{
			name:        "a delete refused for another reason is not tried again",
			rec:         &sessionUploadRecorder{deleteStatuses: []int{http.StatusForbidden}},
			wantErr:     ErrForbidden,
			wantDeletes: []string{createdFileETag},
			wantReads:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSessionUploadServer(t, tc.rec, http.StatusOK)
			defer srv.Close()

			removed, err := newTestClient(t, srv.URL).RemoveEmptyItem(t.Context(), driveid.New("d"), "small-item")

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.ErrorContains(t, err, "small-item")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantRemoved, removed)
			assert.Equal(t, tc.wantDeletes, tc.rec.deleteIfMatch)
			assert.Equal(t, tc.wantReads, tc.rec.reads)
			assert.Empty(t, tc.rec.other)
		})
	}
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

// createAnswerFunc is how the fake server of the create-outcome test answers
// one request. cancel cancels the caller's context.
type createAnswerFunc func(t *testing.T, w http.ResponseWriter, r *http.Request, cancel context.CancelFunc)

// answerWithStatus answers with a Graph error of the given status and code.
func answerWithStatus(status int, code string) createAnswerFunc {
	return func(t *testing.T, w http.ResponseWriter, _ *http.Request, _ context.CancelFunc) {
		t.Helper()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		writeTestResponsef(t, w, `{"error":{"code":%q,"message":"refused"}}`, code)
	}
}

// answerWithNothing takes the request and hangs up without a response.
func answerWithNothing(t *testing.T, w http.ResponseWriter, _ *http.Request, _ context.CancelFunc) {
	t.Helper()

	hj, ok := w.(http.Hijacker)
	if !assert.True(t, ok) {
		return
	}
	conn, _, err := hj.Hijack()
	if assert.NoError(t, err) {
		assert.NoError(t, conn.Close())
	}
}

// answerAfterTheCallerGaveUp reads the whole request, then has the caller
// cancel while the server still holds it: the create was received, and no
// answer ever leaves.
func answerAfterTheCallerGaveUp(t *testing.T, _ http.ResponseWriter, r *http.Request, cancel context.CancelFunc) {
	t.Helper()

	// Not require: this runs on the server's goroutine, not the test's.
	if _, err := io.ReadAll(r.Body); !assert.NoError(t, err) {
		return
	}
	cancel()
	<-r.Context().Done()
}

// answerCreatedWithoutItem accepts the create and names no item.
func answerCreatedWithoutItem(t *testing.T, w http.ResponseWriter, _ *http.Request, _ context.CancelFunc) {
	t.Helper()

	w.WriteHeader(http.StatusCreated)
}

// answerWithEmptyListing lists a folder that does not show the new file.
func answerWithEmptyListing(t *testing.T, w http.ResponseWriter, _ *http.Request, _ context.CancelFunc) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	writeTestResponse(t, w, `{"value":[]}`)
}

// Validates: R-5.6.15
//
// A create that ended without an answer naming its item may have made the
// empty file, and sync keeps its create intent on exactly those failures. A
// refusal says the create did not happen: it never carries the mark, or a
// plain 409 for someone else's file could be read as a lost answer.
func TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile(t *testing.T) {
	tests := []struct {
		name    string
		create  createAnswerFunc
		listing createAnswerFunc // the read-back after a create without an item; nil: never listed
		expired bool             // the caller's deadline has passed before the request
		unknown bool
		also    error // the cause that must stay readable
	}{
		{name: "no response", create: answerWithNothing, unknown: true},
		{
			name:   "500",
			create: answerWithStatus(http.StatusInternalServerError, "generalException"), unknown: true, also: ErrServerError,
		},
		{
			name:   "503",
			create: answerWithStatus(http.StatusServiceUnavailable, "serviceNotAvailable"), unknown: true, also: ErrServerError,
		},
		{
			name:   "504",
			create: answerWithStatus(http.StatusGatewayTimeout, "gatewayTimeout"), unknown: true, also: ErrServerError,
		},
		{name: "408", create: answerWithStatus(http.StatusRequestTimeout, "requestTimeout"), unknown: true},
		{name: "canceled context", create: answerAfterTheCallerGaveUp, unknown: true, also: context.Canceled},
		{name: "expired context", create: answerWithNothing, expired: true, unknown: true, also: context.DeadlineExceeded},
		{
			name:    "created, and the read-back fails",
			create:  answerCreatedWithoutItem,
			listing: answerWithStatus(http.StatusInternalServerError, "generalException"), unknown: true, also: ErrServerError,
		},
		{
			name:    "created, and not listed yet",
			create:  answerCreatedWithoutItem,
			listing: answerWithEmptyListing, unknown: true,
		},
		{name: "409", create: answerWithStatus(http.StatusConflict, "nameAlreadyExists"), also: ErrConflict},
		{name: "429", create: answerWithStatus(http.StatusTooManyRequests, "activityLimitReached"), also: ErrThrottled},
		{name: "400", create: answerWithStatus(http.StatusBadRequest, "invalidRequest"), also: ErrBadRequest},
		{name: "403", create: answerWithStatus(http.StatusForbidden, "accessDenied"), also: ErrForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.expired {
				var release context.CancelFunc
				ctx, release = context.WithDeadline(ctx, time.Now().Add(-time.Minute))
				defer release()
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					tc.create(t, w, r, cancel)

					return
				}
				if assert.NotNil(t, tc.listing, "the folder is only read after a create that named no item") {
					tc.listing(t, w, r, cancel)
				}
			}))
			defer srv.Close()

			item, err := newNoRetryTestClient(t, srv.URL).CreateFile(ctx, driveid.New("d"), "parent", "a.txt")
			require.Error(t, err)
			assert.Nil(t, item)
			if tc.unknown {
				require.ErrorIs(t, err, ErrCreateOutcomeUnknown)
			} else {
				require.NotErrorIs(t, err, ErrCreateOutcomeUnknown)
			}
			if tc.also != nil {
				require.ErrorIs(t, err, tc.also)
			}
			// No content was sent. The transfer layer answers a completed
			// upload by checking the file against the content, which would
			// fail the empty file and drop the mark.
			require.NotErrorIs(t, err, ErrUploadCompletedWithoutItem)
		})
	}
}
