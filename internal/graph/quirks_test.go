package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ---------------------------------------------------------------------------
// §1: Delta normalization pipeline quirk tests
// ---------------------------------------------------------------------------

// TestDeduplicateItems_KeepsLastOccurrence validates that when the Graph API
// returns the same item multiple times in a delta batch, only the LAST
// occurrence is kept (it represents the final state).
func TestDeduplicateItems_KeepsLastOccurrence(t *testing.T) {
	items := []Item{
		{ID: "id-1", Name: "first-version", Size: 100},
		{ID: "id-2", Name: "other-file"},
		{ID: "id-1", Name: "second-version", Size: 200},
		{ID: "id-3", Name: "third-file"},
		{ID: "id-1", Name: "third-version", Size: 300},
	}

	result := deduplicateItems(items, testNoopLogger())

	require.Len(t, result, 3)
	// deduplicateItems preserves the relative order of LAST occurrences:
	// id-2 (original pos 1), id-3 (original pos 3), id-1 last at pos 4.
	assert.Equal(t, "id-2", result[0].ID)
	assert.Equal(t, "id-3", result[1].ID)
	assert.Equal(t, "id-1", result[2].ID)
	assert.Equal(t, "third-version", result[2].Name)
	assert.Equal(t, int64(300), result[2].Size)
}

// TestReorderDeletions_DeleteBeforeCreate validates that within the same
// parent, deletions are reordered before creations. This prevents "item
// already exists" errors from the API when processing rename-then-recreate.
func TestReorderDeletions_DeleteBeforeCreate(t *testing.T) {
	items := []Item{
		{ID: "new-item", Name: "report.txt", ParentID: "folder-1", IsDeleted: false},
		{ID: "old-item", Name: "report.txt", ParentID: "folder-1", IsDeleted: true},
		{ID: "unrelated", Name: "other.txt", ParentID: "folder-2", IsDeleted: false},
	}

	result := reorderDeletions(items, testNoopLogger())

	require.Len(t, result, 3)
	// Within folder-1, deletion should come first.
	folder1Items := filterByParent(result, "folder-1")
	require.Len(t, folder1Items, 2)
	assert.True(t, folder1Items[0].IsDeleted, "deletion should come first")
	assert.False(t, folder1Items[1].IsDeleted, "creation should come second")
}

// TestClearDeletedHashes_BogusAllZeros validates that all-zero hashes on
// deleted items (a known Graph API bug) are cleared to empty strings.
func TestClearDeletedHashes_BogusAllZeros(t *testing.T) {
	items := []Item{
		{
			ID:           "del-1",
			IsDeleted:    true,
			QuickXorHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			SHA256Hash:   "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}

	result := clearDeletedHashes(items, testNoopLogger())

	assert.Empty(t, result[0].QuickXorHash, "bogus hash should be cleared")
	assert.Empty(t, result[0].SHA256Hash, "bogus hash should be cleared")
}

// TestDecodeURLEncodedNames_UTF8Sequences validates decoding of multi-byte
// UTF-8 percent-encoded sequences in item names (Japanese characters).
func TestDecodeURLEncodedNames_UTF8Sequences(t *testing.T) {
	items := []Item{
		{ID: "utf8-1", Name: "%E6%97%A5%E6%9C%AC%E8%AA%9E.txt"},
		{ID: "space", Name: "file%20name.txt"},
		{ID: "plus", Name: "file+name.txt"},
	}

	result := decodeURLEncodedNames(items, testNoopLogger())

	assert.Equal(t, "日本語.txt", result[0].Name)
	assert.Equal(t, "file name.txt", result[1].Name)
	// Plus signs are NOT decoded by PathUnescape (only QueryUnescape does that).
	assert.Equal(t, "file+name.txt", result[2].Name)
}

// TestNormalizeDeltaItems_PipelineOrder validates that the full normalization
// pipeline runs in the correct order: decode → filter → clear hashes → dedup → reorder.
func TestNormalizeDeltaItems_PipelineOrder(t *testing.T) {
	items := []Item{
		// URL-encoded name that should be decoded first.
		{ID: "encoded", Name: "my%20doc.txt", ParentID: "root"},
		// Package that should be filtered out.
		{ID: "pkg", Name: "Notebook.one", IsPackage: true, ParentID: "root"},
		// Deleted item with bogus hash that should be cleared.
		{ID: "deleted", Name: "old.txt", IsDeleted: true, QuickXorHash: "bogus", ParentID: "folder"},
		// Duplicate item — only last version kept.
		{ID: "dup", Name: "v1", ParentID: "folder"},
		{ID: "dup", Name: "v2", ParentID: "folder"},
		// Create at same parent as deletion — deletion reordered first.
		{ID: "create", Name: "new.txt", ParentID: "folder", IsDeleted: false},
	}

	result := normalizeDeltaItems(items, testNoopLogger())

	// Package filtered, duplicate removed: 4 items remain.
	require.Len(t, result, 4)

	// Verify URL decoding applied.
	var encodedItem *Item
	for i := range result {
		if result[i].ID == "encoded" {
			encodedItem = &result[i]
			break
		}
	}
	require.NotNil(t, encodedItem)
	assert.Equal(t, "my doc.txt", encodedItem.Name)

	// Verify duplicate kept last version.
	for i := range result {
		if result[i].ID == "dup" {
			assert.Equal(t, "v2", result[i].Name)
		}
	}

	// Verify deleted hash cleared.
	for i := range result {
		if result[i].ID == "deleted" {
			assert.Empty(t, result[i].QuickXorHash)
		}
	}
}

// ---------------------------------------------------------------------------
// §1: Delta token handling
// ---------------------------------------------------------------------------

// TestStripBaseURL_FullURLToRelativePath validates that absolute delta token
// URLs from the API are converted to relative paths for Do().
func TestStripBaseURL_FullURLToRelativePath(t *testing.T) {
	client := NewClient("https://graph.microsoft.com/v1.0", nil, staticToken("tok"), nil, "")

	path, err := client.stripBaseURL("https://graph.microsoft.com/v1.0/drives/abc/root/delta?token=xxx")
	require.NoError(t, err)
	assert.Equal(t, "/drives/abc/root/delta?token=xxx", path)
}

// TestStripBaseURL_CrossDomainRejected validates that delta tokens pointing
// to a different domain are rejected (security: prevents SSRF via token).
func TestStripBaseURL_CrossDomainRejected(t *testing.T) {
	client := NewClient("https://graph.microsoft.com/v1.0", nil, staticToken("tok"), nil, "")

	_, err := client.stripBaseURL("https://evil.example.com/v1.0/drives/abc/root/delta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match base URL")
}

// TestBuildDeltaPath_EmptyTokenIsInitialSync validates that an empty token
// triggers the initial delta path (no token parameter).
func TestBuildDeltaPath_EmptyTokenIsInitialSync(t *testing.T) {
	client := NewClient("https://graph.microsoft.com/v1.0", nil, staticToken("tok"), nil, "")

	path, err := client.buildDeltaPath(driveid.New("abc123def4567890"), "")
	require.NoError(t, err)
	assert.Equal(t, "/drives/abc123def4567890/root/delta", path)
}

// TestBuildDeltaPath_FullURLToken validates that a full URL token from a
// previous response is correctly stripped to a relative path.
func TestBuildDeltaPath_FullURLTokenStripped(t *testing.T) {
	client := NewClient("https://graph.microsoft.com/v1.0", nil, staticToken("tok"), nil, "")

	path, err := client.buildDeltaPath(
		driveid.New("abc123def4567890"),
		"https://graph.microsoft.com/v1.0/drives/abc123def4567890/root/delta?token=opaque",
	)
	require.NoError(t, err)
	assert.Equal(t, "/drives/abc123def4567890/root/delta?token=opaque", path)
}

// ---------------------------------------------------------------------------
// §1: Pre-auth URL handling (no Authorization header)
// ---------------------------------------------------------------------------

// TestPreAuthURL_NoAuthorizationHeader validates that pre-authenticated URLs
// (download, upload chunk) never receive an Authorization header.
func TestPreAuthURL_NoAuthorizationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		assert.Empty(t, auth, "pre-auth URL must not have Authorization header")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("content"))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "test", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/download", http.NoBody)
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("User-Agent", client.userAgent)
		return req, nil
	})
	require.NoError(t, err)
	defer resp.Body.Close()
}

// ---------------------------------------------------------------------------
// §1: Simple upload content type
// ---------------------------------------------------------------------------

// TestSimpleUpload_OctetStreamContentType validates that SimpleUpload sends
// application/octet-stream, not application/json.
func TestSimpleUpload_OctetStreamContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"),
			"simple upload must use octet-stream")
		assert.NotEmpty(t, r.Header.Get("Authorization"),
			"simple upload is authenticated")

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(driveItemResponse{
			ID:   "item-1",
			Name: "test.txt",
			File: &fileFacet{Hashes: &hashFacet{QuickXorHash: "abc"}},
		})
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	content := bytes.NewReader([]byte("hello"))
	item, err := client.SimpleUpload(
		context.Background(), driveid.New("drive1"), "parent1", "test.txt", content, 5,
	)
	require.NoError(t, err)
	assert.Equal(t, "item-1", item.ID)
}

// ---------------------------------------------------------------------------
// §1: Upload chunk response shapes (handleChunkResponse)
// ---------------------------------------------------------------------------

// TestHandleChunkResponse_Intermediate202 validates that a 202 Accepted
// response (intermediate chunk) returns nil item and nil error.
func TestHandleChunkResponse_Intermediate202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"nextExpectedRanges":["10485760-"]}`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "chunk", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/upload", http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	item, err := client.handleChunkResponse(resp)
	require.NoError(t, err)
	assert.Nil(t, item, "intermediate chunk should return nil item")
}

// TestHandleChunkResponse_Final201 validates that a 201 Created response
// (final chunk) returns the completed item.
func TestHandleChunkResponse_Final201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(driveItemResponse{
			ID:   "completed-item",
			Name: "uploaded.txt",
			File: &fileFacet{Hashes: &hashFacet{QuickXorHash: "hash123"}},
		})
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "chunk", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/upload", http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	item, err := client.handleChunkResponse(resp)
	require.NoError(t, err)
	require.NotNil(t, item, "final chunk should return item")
	assert.Equal(t, "completed-item", item.ID)
}

// TestHandleChunkResponse_Final200 validates that a 200 OK response
// (alternative final chunk) also returns the completed item.
func TestHandleChunkResponse_Final200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(driveItemResponse{
			ID:   "completed-item",
			Name: "uploaded.txt",
		})
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "chunk", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/upload", http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	item, err := client.handleChunkResponse(resp)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "completed-item", item.ID)
}

// TestHandleChunkResponse_Unexpected2xx validates that unexpected 2xx codes
// (e.g. 204 No Content) are treated as errors.
func TestHandleChunkResponse_Unexpected2xx(t *testing.T) {
	// Build a response manually since we can't make doPreAuthRetry return 204.
	resp := &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
	}

	client := newTestClient(t, "http://unused")

	item, err := client.handleChunkResponse(resp)
	require.Error(t, err)
	assert.Nil(t, item)
	assert.Contains(t, err.Error(), "unexpected status 204")
}

// ---------------------------------------------------------------------------
// §1: HTTP 410 delta token expiry
// ---------------------------------------------------------------------------

// TestDelta_HTTP410_TokenExpired validates that an HTTP 410 Gone response
// on delta is classified as ErrGone for token re-enumeration.
func TestDelta_HTTP410_TokenExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "gone-req")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"resyncRequired"}}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	_, err := client.Delta(context.Background(), driveid.New("drive1"), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}

// ---------------------------------------------------------------------------
// §1: rewindBody edge cases
// ---------------------------------------------------------------------------

// TestRewindBody_NilBody validates that rewindBody handles nil body gracefully.
func TestRewindBody_NilBody(t *testing.T) {
	err := rewindBody(nil)
	assert.NoError(t, err)
}

// TestRewindBody_NonSeeker validates that rewindBody is a no-op for non-seekable readers.
func TestRewindBody_NonSeeker(t *testing.T) {
	reader := strings.NewReader("data")
	// strings.Reader IS a Seeker, so use a non-seekable wrapper.
	err := rewindBody(io.NopCloser(reader))
	assert.NoError(t, err)
}

// TestRewindBody_FailOnSecondSeek validates the retry error path when
// the body rewind fails on the retry attempt (not the initial attempt).
func TestRewindBody_FailOnSecondSeek(t *testing.T) {
	body := &failOnSecondSeeker{data: []byte("test")}

	// First seek succeeds.
	err := rewindBody(body)
	assert.NoError(t, err)

	// Second seek fails — this is what happens on retry.
	err = rewindBody(body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rewinding request body")
}

// ---------------------------------------------------------------------------
// §9: Error classification edge cases
// ---------------------------------------------------------------------------

// TestGraphError_StatusBeforeSentinel validates that GraphError wrapping a
// sentinel is classified by its StatusCode, not the generic sentinel.
// E.g., HTTP 507 wrapping ErrServerError is classified as 507 (fatal),
// not generic 5xx (retryable).
func TestGraphError_StatusBeforeSentinel(t *testing.T) {
	// 507 Insufficient Storage should NOT be retried despite being 5xx.
	assert.False(t, isRetryable(http.StatusInsufficientStorage),
		"507 should not be retryable")

	// But generic 500/502/503/504 ARE retryable.
	assert.True(t, isRetryable(http.StatusInternalServerError))
	assert.True(t, isRetryable(http.StatusBadGateway))
	assert.True(t, isRetryable(http.StatusServiceUnavailable))
	assert.True(t, isRetryable(http.StatusGatewayTimeout))
}

// TestRetryBackoff_429_LargeRetryAfter validates that the Retry-After
// header value is honored, not capped by the normal backoff max.
func TestRetryBackoff_429_LargeRetryAfter(t *testing.T) {
	client := NewClient("http://localhost", nil, staticToken("tok"), nil, "")

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"120"}},
	}

	backoff := client.retryBackoff(resp, 0)
	assert.Equal(t, 120*time.Second, backoff,
		"Retry-After: 120 should produce exactly 120s backoff")
}

// TestRetryBackoff_Non429_IgnoresRetryAfter validates that Retry-After
// headers are ignored for non-429 responses.
func TestRetryBackoff_Non429_IgnoresRetryAfter(t *testing.T) {
	client := NewClient("http://localhost", nil, staticToken("tok"), nil, "")

	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Retry-After": {"120"}},
	}

	backoff := client.retryBackoff(resp, 0)
	// Should use calculated backoff (1s ± jitter), not 120s.
	assert.Less(t, backoff, 2*time.Second,
		"non-429 should use calculated backoff, not Retry-After")
}

// TestIsRetryable_UnmappedStatusCodes validates that unmapped status codes
// (3xx, 4xx not in the switch) are not silently treated as retryable.
func TestIsRetryable_UnmappedStatusCodes(t *testing.T) {
	tests := []struct {
		code      int
		retryable bool
	}{
		{http.StatusMovedPermanently, false},    // 301
		{http.StatusFound, false},               // 302
		{http.StatusNotModified, false},         // 304
		{http.StatusMethodNotAllowed, false},    // 405
		{http.StatusTeapot, false},              // 418
		{http.StatusInsufficientStorage, false}, // 507
		{509, true},                             // Bandwidth Limit Exceeded
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("HTTP_%d", tt.code), func(t *testing.T) {
			assert.Equal(t, tt.retryable, isRetryable(tt.code))
		})
	}
}

// TestClassifyStatus_UnmappedCodes validates that unmapped status codes
// outside the 5xx range return nil (not classified as server error).
func TestClassifyStatus_UnmappedCodes(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusMovedPermanently, nil},                                // 301 — not an error
		{http.StatusFound, nil},                                           // 302 — not an error
		{http.StatusMethodNotAllowed, nil},                                // 405 — not mapped
		{http.StatusRequestedRangeNotSatisfiable, ErrRangeNotSatisfiable}, // 416
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("HTTP_%d", tt.code), func(t *testing.T) {
			got := classifyStatus(tt.code)
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §8: Upload session lifecycle
// ---------------------------------------------------------------------------

// TestUpload_SessionCanceledOnChunkError validates that when an upload chunk
// fails, the session is canceled via DELETE to the pre-auth URL with
// background context (original context may already be canceled).
func TestUpload_SessionCanceledOnChunkError(t *testing.T) {
	var sessionCanceled atomic.Int32
	var srvURL atomic.Value

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "createUploadSession"):
			// Create session succeeds — use the stored server URL.
			url := srvURL.Load().(string)
			_ = json.NewEncoder(w).Encode(uploadSessionResponse{
				UploadURL:          url + "/upload-session",
				ExpirationDateTime: time.Now().Add(time.Hour).Format(time.RFC3339),
			})

		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "upload-session"):
			// Chunk upload fails with non-retryable error.
			w.Header().Set("request-id", "chunk-fail")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))

		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "upload-session"):
			// Session cancel.
			sessionCanceled.Add(1)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(driveItemResponse{ID: "parent-1"})
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL.Store(srv.URL)

	client := newTestClient(t, srv.URL)

	// Use chunkedUploadEncapsulated directly to test session lifecycle.
	content := bytes.NewReader(make([]byte, 5*1024*1024)) // 5 MiB to force chunked.
	_, err := client.chunkedUploadEncapsulated(
		context.Background(),
		driveid.New("drive1"), "parent1", "big.txt",
		content, int64(content.Len()), time.Time{}, nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
	assert.Equal(t, int32(1), sessionCanceled.Load(),
		"upload session should be canceled on error")
}

// TestUpload_SimpleUploadFollowedByMtimePatch validates that files ≤4 MiB
// use simple upload and then PATCH to set mtime.
func TestUpload_SimpleUploadFollowedByMtimePatch(t *testing.T) {
	var patchCalled atomic.Int32

	mtime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "content"):
			// Simple upload.
			assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(driveItemResponse{
				ID:   "new-item",
				Name: "small.txt",
			})

		case r.Method == http.MethodPatch:
			// Mtime PATCH.
			patchCalled.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)

			fsInfo, ok := body["fileSystemInfo"].(map[string]any)
			require.True(t, ok, "PATCH should include fileSystemInfo")
			assert.Contains(t, fsInfo["lastModifiedDateTime"], "2025-06-15")

			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(driveItemResponse{
				ID:   "new-item",
				Name: "small.txt",
			})

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	content := bytes.NewReader([]byte("small file content"))

	item, err := client.Upload(
		context.Background(),
		driveid.New("drive1"), "parent1", "small.txt",
		content, int64(content.Len()), mtime, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "new-item", item.ID)
	assert.Equal(t, int32(1), patchCalled.Load(),
		"mtime PATCH should be called after simple upload")
}

// TestUpload_SimpleUploadNoMtime validates that when mtime is zero,
// no PATCH is sent after simple upload.
func TestUpload_SimpleUploadNoMtime(t *testing.T) {
	var patchCalled atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalled.Add(1)
		}

		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "content") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(driveItemResponse{ID: "item-1", Name: "test.txt"})
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	content := bytes.NewReader([]byte("content"))

	_, err := client.Upload(
		context.Background(),
		driveid.New("drive1"), "parent1", "test.txt",
		content, int64(content.Len()), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, int32(0), patchCalled.Load(),
		"no PATCH when mtime is zero")
}

// TestUploadChunk_ReaderAtEnablesRetrySafe validates that UploadChunk uses
// io.ReaderAt to create fresh SectionReaders per retry attempt, preventing
// races with previous attempt's transport writeLoop goroutine.
func TestUploadChunk_ReaderAtEnablesRetrySafe(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Content-Range header format.
		cr := r.Header.Get("Content-Range")
		assert.Contains(t, cr, "bytes 0-")

		// Read entire body to verify data integrity.
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)

		n := attempts.Add(1)
		if n <= 1 {
			// First attempt fails with retryable error.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		// Second attempt succeeds: verify body is complete (not empty from exhausted reader).
		assert.Len(t, body, 1024, "retry should have full body via fresh SectionReader")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(driveItemResponse{ID: "item-1", Name: "file.txt"})
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	session := &UploadSession{UploadURL: srv.URL + "/upload"}
	chunk := bytes.NewReader(data)

	item, err := client.UploadChunk(context.Background(), session, chunk, 0, 1024, 1024)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, int32(2), attempts.Load(), "should have retried once")
}

// TestUploadChunk_416_ClassifiedAsRangeError validates that HTTP 416 from
// the upload session is classified as ErrRangeNotSatisfiable via GraphError.
func TestUploadChunk_416_ClassifiedAsRangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "range-err")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		_, _ = w.Write([]byte(`{"error":"range"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	session := &UploadSession{UploadURL: srv.URL + "/upload"}
	chunk := bytes.NewReader(make([]byte, 1024))

	_, err := client.UploadChunk(context.Background(), session, chunk, 0, 1024, 2048)
	require.Error(t, err)

	var graphErr *GraphError
	require.True(t, errors.As(err, &graphErr), "error should be a GraphError")
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, graphErr.StatusCode)
}

// TestQueryUploadSession_ReturnsNextExpectedRanges validates that
// QueryUploadSession correctly parses the nextExpectedRanges field.
func TestQueryUploadSession_ReturnsNextExpectedRanges(t *testing.T) {
	var srvURL atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"), "session URL is pre-authenticated")

		url := srvURL.Load().(string)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(uploadSessionStatusResponse{
			UploadURL:          url + "/upload",
			ExpirationDateTime: time.Now().Add(time.Hour).Format(time.RFC3339),
			NextExpectedRanges: []string{"327680-"},
		})
	}))
	defer srv.Close()
	srvURL.Store(srv.URL)

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: srv.URL + "/upload"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)
	require.Len(t, status.NextExpectedRanges, 1)
	assert.Equal(t, "327680-", status.NextExpectedRanges[0])
}

// ---------------------------------------------------------------------------
// §9: HTTP 410 on delta = token expired
// ---------------------------------------------------------------------------

// TestDeltaAll_HTTP410_ReturnsGone validates that DeltaAll correctly
// propagates ErrGone when the delta token has expired.
func TestDeltaAll_HTTP410_ReturnsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"resyncRequired"}}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	_, _, err := client.DeltaAll(context.Background(), driveid.New("drive1"), "old-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrGone)
}
