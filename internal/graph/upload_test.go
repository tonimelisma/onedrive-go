package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// errorReadCloser is an io.ReadCloser that always returns an error from Read.
// Used to test drain-failure paths in handleChunkResponse.
type errorReadCloser struct{}

func (errorReadCloser) Read(_ []byte) (int, error) {
	return 0, errors.New("read failed")
}

func (errorReadCloser) Close() error {
	return nil
}

func writeUploadTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

// createdFileETag is the eTag serveCreateFile gives the item it creates.
const createdFileETag = `"created-etag"`

// serveCreateFile answers the POST /items/{parent}/children that every new
// file upload starts with (create by body, conflictBehavior=fail), and
// reports whether r was that request.
func serveCreateFile(t *testing.T, w http.ResponseWriter, r *http.Request, itemID string) bool {
	t.Helper()

	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/children") {
		return false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeTestResponsef(t, w, `{"id":%q,"name":"created","size":0,"eTag":%q,"file":{}}`, itemID, createdFileETag)

	return true
}

func newUpload416RecoveryServer(t *testing.T, nextExpectedRange string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			writeTestResponse(t, w, `{"error":{"code":"invalidRange"}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			writeTestResponsef(t, w, `{
				"uploadUrl": "https://uploads.contoso.sharepoint.com/session/recover",
				"expirationDateTime": "2025-12-31T23:59:59Z",
				"nextExpectedRanges": ["%s"]
			}`, nextExpectedRange)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

func TestSimpleUploadToItem_ContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "ct-item",
			"name": "binary.dat",
			"size": 4,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUploadToItem(
		t.Context(), driveid.New("d"), "item-1", "",
		strings.NewReader("data"), 4,
	)
	require.NoError(t, err)
}

// Validates: R-5.6.9
func TestSimpleUploadToItem_Success(t *testing.T) {
	content := "overwrite file content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-123/content", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, content, string(body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-123",
			"name": "upload.txt",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`, len(content))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.SimpleUploadToItem(
		t.Context(), driveid.New("d"), "item-123", "",
		strings.NewReader(content), int64(len(content)),
	)
	require.NoError(t, err)

	assert.Equal(t, "item-123", item.ID)
	assert.Equal(t, int64(len(content)), item.Size)
}

func TestSimpleUploadToItem_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-upload-err")
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUploadToItem(
		t.Context(), driveid.New("d"), "item-1", "",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestSimpleUploadToItem_TokenError(t *testing.T) {
	client := MustNewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")

	_, err := client.SimpleUploadToItem(
		t.Context(), driveid.New("d"), "item-1", "",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestSimpleUploadToItem_NetworkError(t *testing.T) {
	client := MustNewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")

	_, err := client.SimpleUploadToItem(
		t.Context(), driveid.New("d"), "item-1", "",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw upload request failed")
}

// Validates: R-5.6.9
func TestCreateUploadSessionForItem_ParsesSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "createUploadSession")

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Contains(t, string(body), `"@microsoft.graph.conflictBehavior":"replace"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/abc123",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 10485760, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/abc123"), session.UploadURL)
	assert.Equal(t, 2024, session.ExpirationTime.Year())
	assert.Equal(t, 12, int(session.ExpirationTime.Month()))
	assert.Equal(t, 31, session.ExpirationTime.Day())
}

func TestCreateUploadSession_RetriesTransientNotFoundOnCreate(t *testing.T) {
	var creates atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/children") {
			// A just-created parent can answer itemNotFound for a while.
			if creates.Add(1) <= 2 {
				w.Header().Set("request-id", "req-create-404")
				w.WriteHeader(http.StatusNotFound)
				writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
				return
			}
			serveCreateFile(t, w, r, "retry-item")
			return
		}

		assert.Equal(t, "/drives/000000000000000d/items/retry-item/createUploadSession", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/retry123",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	client.policies.uploadSessionCreate = testRetryPolicy()

	session, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "retry-file.bin", 10485760, time.Time{},
	)
	require.NoError(t, err)
	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/retry123"), session.UploadURL)
	assert.Equal(t, int32(3), creates.Load())
}

// Validates: R-5.6.9
func TestCreateUploadSessionForItem_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/item-123/createUploadSession", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Contains(t, string(body), `"@microsoft.graph.conflictBehavior":"replace"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/abc123",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 10485760, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/abc123"), session.UploadURL)
}

func TestCreateUploadSessionForItem_PersonalUploadHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "createUploadSession")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://my.microsoftpersonalcontent.com/personal/upload/session-abc123",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 10485760, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(
		t,
		UploadURL("https://my.microsoftpersonalcontent.com/personal/upload/session-abc123"),
		session.UploadURL,
	)
}

func TestCreateUploadSessionForItem_InvalidExpiration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/xyz",
			"expirationDateTime": "not-a-date"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 1024, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/xyz"), session.UploadURL)
	assert.True(t, session.ExpirationTime.IsZero())
}

func TestCreateUploadSessionForItem_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-session-err")
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestCreateUploadSessionForItem_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{invalid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding upload session response")
}

// Validates: R-5.3, R-5.6.6
func TestUploadChunk_Intermediate(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Empty(t, r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusAccepted)
		writeTestResponse(t, w, `{"nextExpectedRanges":["327680-"]}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	chunkData := bytes.Repeat([]byte("A"), ChunkAlignment)
	item, complete, err := client.UploadChunk(
		t.Context(), session,
		bytes.NewReader(chunkData),
		0, int64(ChunkAlignment), 2*int64(ChunkAlignment),
	)
	require.NoError(t, err)
	assert.False(t, complete)
	assert.Nil(t, item, "intermediate chunk should return nil item")
}

func TestUploadChunk_Final(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "completed-item",
			"name": "large-file.bin",
			"size": 655360,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {
				"mimeType": "application/octet-stream",
				"hashes": {"quickXorHash": "aGFzaA=="}
			}
		}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	chunkData := bytes.Repeat([]byte("B"), ChunkAlignment)
	totalSize := 2 * int64(ChunkAlignment)
	item, complete, err := client.UploadChunk(
		t.Context(), session,
		bytes.NewReader(chunkData),
		int64(ChunkAlignment), int64(ChunkAlignment), totalSize,
	)
	require.NoError(t, err)
	assert.True(t, complete)
	require.NotNil(t, item, "final chunk should return completed item")

	assert.Equal(t, "completed-item", item.ID)
	assert.Equal(t, "large-file.bin", item.Name)
	assert.Equal(t, "aGFzaA==", item.QuickXorHash)
}

func TestUploadChunk_FinalWith200(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "updated-item",
			"name": "updated-file.bin",
			"size": 327680,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-06-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	item, complete, err := client.UploadChunk(
		t.Context(), session,
		strings.NewReader("final-data"),
		0, 10, 10,
	)
	require.NoError(t, err)
	assert.True(t, complete)
	require.NotNil(t, item)
	assert.Equal(t, "updated-item", item.ID)
}

func TestUploadChunk_ContentRange(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentRange := r.Header.Get("Content-Range")
		assert.Equal(t, "bytes 327680-655359/983040", contentRange)

		w.WriteHeader(http.StatusAccepted)
		writeTestResponse(t, w, `{}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	offset := int64(ChunkAlignment)
	length := int64(ChunkAlignment)
	total := int64(3 * ChunkAlignment)

	_, complete, err := client.UploadChunk(
		t.Context(), session,
		bytes.NewReader(make([]byte, ChunkAlignment)),
		offset, length, total,
	)
	require.NoError(t, err)
	assert.False(t, complete)
}

func TestUploadChunk_ErrorClassification(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			body:    `{"error":"internal"}`,
			wantErr: ErrServerError,
		},
		{
			name:    "range not satisfiable",
			status:  http.StatusRequestedRangeNotSatisfiable,
			body:    `{"error":"range not satisfiable"}`,
			wantErr: ErrRangeNotSatisfiable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, err := fmt.Fprint(w, tt.body)
				assert.NoError(t, err)
			}))
			defer chunkSrv.Close()

			client := newTestClient(t, "http://unused")
			session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

			_, _, err := client.UploadChunk(
				t.Context(), session,
				strings.NewReader("data"),
				0, 4, 4,
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestUploadChunk_ContextCanceled(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/upload"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := client.UploadChunk(
		ctx, session,
		strings.NewReader("data"),
		0, 4, 4,
	)
	require.Error(t, err)
}

func TestCancelUploadSession_Success(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))

		w.WriteHeader(http.StatusNoContent)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/session/abc")}

	err := client.CancelUploadSession(t.Context(), session)
	require.NoError(t, err)
}

func TestCancelUploadSession_Error(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/session/gone")}

	err := client.CancelUploadSession(t.Context(), session)
	require.Error(t, err)
	// 404 is non-retryable; doPreAuth returns *GraphError with ErrNotFound.
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCancelUploadSession_ContextCanceled(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/session"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := client.CancelUploadSession(ctx, session)
	require.Error(t, err)
}

// Validates: R-5.3.3, R-6.7.6
func TestChunkAlignment(t *testing.T) {
	assert.Equal(t, 327680, ChunkAlignment)
}

// Validates: R-5.6.4
func TestCreateUploadSessionForItem_WithFileSystemInfo(t *testing.T) {
	mtime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		// Verify fileSystemInfo is included with the correct timestamp.
		bodyStr := string(body)
		assert.Contains(t, bodyStr, `"lastModifiedDateTime":"2024-06-15T10:30:00Z"`)
		assert.Contains(t, bodyStr, `"fileSystemInfo"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/fsi",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 5242880, mtime,
	)
	require.NoError(t, err)
	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/fsi"), session.UploadURL)
}

// Validates: R-5.6.4
func TestCreateUploadSessionForItem_WithoutFileSystemInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		// Verify fileSystemInfo is NOT included when mtime is zero.
		bodyStr := string(body)
		assert.NotContains(t, bodyStr, "fileSystemInfo")
		assert.NotContains(t, bodyStr, "lastModifiedDateTime")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/nofsi",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 5242880, time.Time{},
	)
	require.NoError(t, err)
	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/nofsi"), session.UploadURL)
}

func TestErrRangeNotSatisfiable_Classification(t *testing.T) {
	sentinel := classifyStatus(http.StatusRequestedRangeNotSatisfiable)
	assert.ErrorIs(t, sentinel, ErrRangeNotSatisfiable)
}

func TestQueryUploadSession_Success(t *testing.T) {
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/resume",
			"expirationDateTime": "2024-12-31T23:59:59Z",
			"nextExpectedRanges": ["327680-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/session")}

	status, err := client.QueryUploadSession(t.Context(), session)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/resume"), status.UploadURL)
	assert.Equal(t, 2024, status.ExpirationTime.Year())
	assert.Equal(t, []string{"327680-"}, status.NextExpectedRanges)
}

func TestQueryUploadSession_Expired(t *testing.T) {
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponse(t, w, `{"error":"session not found"}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/session")}

	_, err := client.QueryUploadSession(t.Context(), session)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestQueryUploadSession_NetworkError(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/session"}

	_, err := client.QueryUploadSession(t.Context(), session)
	require.Error(t, err)
	// RetryTransport exhausts retries, then doPreAuth wraps the network error.
	assert.Contains(t, err.Error(), "query upload session failed")
}

func TestHandleChunkResponse_FinalDecodeError(t *testing.T) {
	// Verify that handleChunkResponse returns a decode error when the final
	// chunk response (201) contains invalid JSON.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{not valid json`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	_, _, err := client.UploadChunk(
		t.Context(), session,
		strings.NewReader("final-data"),
		0, 10, 10,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUploadCompletedWithoutItem)
}

func TestHandleChunkResponse_IntermediateDrainError(t *testing.T) {
	// Verify that handleChunkResponse returns a drain error when the 202
	// response body fails to read during drain.
	client := newTestClient(t, "http://unused")

	// Build a crafted *http.Response with an errorReadCloser body.
	resp := &http.Response{
		StatusCode: http.StatusAccepted,
		Body:       errorReadCloser{},
	}

	_, _, err := client.handleChunkResponse(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "draining chunk response body")
}

func TestQueryUploadSession_DecodeError(t *testing.T) {
	// Verify that QueryUploadSession returns a decode error when the server
	// returns 200 with invalid JSON.
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{not valid json`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/session")}

	_, err := client.QueryUploadSession(t.Context(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding session status response")
}

func TestQueryUploadSession_InvalidExpiration(t *testing.T) {
	// Verify that QueryUploadSession handles an unparseable expirationDateTime
	// gracefully by using zero time (matching CreateUploadSession behavior).
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/inv-exp",
			"expirationDateTime": "not-a-date",
			"nextExpectedRanges": ["0-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/session")}

	status, err := client.QueryUploadSession(t.Context(), session)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/inv-exp"), status.UploadURL)
	assert.True(t, status.ExpirationTime.IsZero(), "invalid expiration should produce zero time")
	assert.Equal(t, []string{"0-"}, status.NextExpectedRanges)
}

// sessionUploadRecorder is a Graph plus upload-URL fake that records every
// request a small-file upload makes.
type sessionUploadRecorder struct {
	mu            stdsync.Mutex
	createBodies  []string
	createURLs    []string
	deleteIfMatch []string
	sessionURLs   []string
	sessionMatch  []string
	sessionBodies []string
	chunkBodies   []string
	simplePuts    int
	patches       int
	other         []string
}

func newSessionUploadServer(t *testing.T, rec *sessionUploadRecorder, sessionStatus int) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		rec.mu.Lock()
		defer rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/children"):
			rec.createBodies = append(rec.createBodies, string(body))
			rec.createURLs = append(rec.createURLs, r.URL.RawPath+"|"+r.URL.Path)
			serveCreateFile(t, w, r, "small-item")
		case r.Method == http.MethodDelete:
			rec.deleteIfMatch = append(rec.deleteIfMatch, r.Header.Get("If-Match"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "createUploadSession"):
			rec.sessionURLs = append(rec.sessionURLs, r.URL.Path)
			rec.sessionMatch = append(rec.sessionMatch, r.Header.Get("If-Match"))
			rec.sessionBodies = append(rec.sessionBodies, string(body))
			if sessionStatus != http.StatusOK {
				w.WriteHeader(sessionStatus)
				writeTestResponse(t, w, `{"error":{"code":"accessDenied"}}`)

				return
			}

			w.WriteHeader(http.StatusOK)
			writeTestResponsef(t, w, `{"uploadUrl":"%s/upload-session","expirationDateTime":"2030-01-01T00:00:00Z"}`, srv.URL)
		case r.Method == http.MethodPut && r.URL.Path == "/upload-session":
			rec.chunkBodies = append(rec.chunkBodies, string(body))
			w.WriteHeader(http.StatusCreated)
			writeTestResponsef(t, w, `{"id":"small-item","name":"small.txt","size":%d,
				"lastModifiedDateTime":"2024-07-10T09:00:00Z","file":{}}`, len(body))
		case r.Method == http.MethodPut:
			rec.simplePuts++
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"id":"small-item","name":"small.txt","size":0,"file":{}}`)
		case r.Method == http.MethodPatch:
			rec.patches++
			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"id":"small-item","name":"small.txt","size":0,"file":{}}`)
		default:
			rec.other = append(rec.other, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))

	return srv
}

// Validates: R-5.3.1, R-5.6.4
func TestUpload_SmallFileUsesSessionWithFileSystemInfo(t *testing.T) {
	content := bytes.Repeat([]byte("k"), 1024)
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		upload func(*Client) (*Item, error)
	}{
		{name: "CreateByParent", upload: func(c *Client) (*Item, error) {
			return c.Upload(t.Context(), driveid.New("d"), "parent", "small.txt",
				bytes.NewReader(content), int64(len(content)), mtime, nil)
		}},
		{name: "OverwriteByID", upload: func(c *Client) (*Item, error) {
			return c.UploadToItem(t.Context(), driveid.New("d"), "small-item", "",
				bytes.NewReader(content), int64(len(content)), mtime, nil)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &sessionUploadRecorder{}
			srv := newSessionUploadServer(t, rec, http.StatusOK)
			defer srv.Close()

			item, err := tt.upload(newTestClient(t, srv.URL))
			require.NoError(t, err)
			assert.Equal(t, "small-item", item.ID)

			// One session create carrying the mtime and one fragment with the
			// whole file: content and mtime land in one version. The simple
			// PUT plus mtime PATCH made two versions on Personal (probe 7).
			require.Len(t, rec.sessionBodies, 1)
			assert.Contains(t, rec.sessionBodies[0], `"fileSystemInfo":{"lastModifiedDateTime":"2024-07-10T09:00:00Z"}`)
			assert.Equal(t, []string{string(content)}, rec.chunkBodies)
			assert.Zero(t, rec.simplePuts, "a non-empty file must not use the simple PUT")
			assert.Zero(t, rec.patches, "a session upload must not PATCH the mtime afterwards")
			assert.Empty(t, rec.other)
		})
	}
}

// Validates: R-5.3.1
func TestUpload_SmallFileSessionPermissionDeniedIsNotRetried(t *testing.T) {
	rec := &sessionUploadRecorder{}
	srv := newSessionUploadServer(t, rec, http.StatusForbidden)
	defer srv.Close()

	content := []byte("shared-folder-write")
	_, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "shared-folder", "blocked.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil)
	require.ErrorIs(t, err, ErrForbidden)
	assert.Len(t, rec.sessionBodies, 1)
	assert.Zero(t, rec.simplePuts)
}

// Validates: R-5.6.5, R-5.6.7
func TestUpload_ZeroByteCreatesByBodyThenPatchesMtime(t *testing.T) {
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	for _, withMtime := range []bool{true, false} {
		t.Run(fmt.Sprintf("mtime=%v", withMtime), func(t *testing.T) {
			rec := &sessionUploadRecorder{}
			srv := newSessionUploadServer(t, rec, http.StatusOK)
			defer srv.Close()

			sent := time.Time{}
			if withMtime {
				sent = mtime
			}

			item, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "empty.txt",
				bytes.NewReader(nil), 0, sent, nil)
			require.NoError(t, err)
			assert.Equal(t, "small-item", item.ID)
			// The create by body is the whole upload (L-01): the new item
			// already is the empty file, so no content request follows.
			assert.Len(t, rec.createBodies, 1)
			assert.Zero(t, rec.simplePuts)
			assert.Empty(t, rec.sessionBodies, "a zero-byte file needs no session")
			if withMtime {
				assert.Equal(t, 1, rec.patches)
			} else {
				assert.Zero(t, rec.patches)
			}
		})
	}
}

// Validates: R-5.6.5
func TestUpload_ZeroByteMtimePatchFailureReturnsUploadedItem(t *testing.T) {
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	var puts, patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "empty-item") {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodPut:
			puts.Add(1)
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"id":"empty-item","name":"empty.txt","size":0,"file":{}}`)
		case http.MethodPatch:
			patches.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			writeTestResponse(t, w, `{"error":{"code":"generalException"}}`)
		default:
			assert.Failf(t, "unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	item, err := newTestClient(t, srv.URL).Upload(t.Context(), driveid.New("d"), "parent", "empty.txt",
		bytes.NewReader(nil), 0, mtime, nil)

	// The content landed, so the caller gets the item and a typed "mtime not
	// applied", not a plain failure that makes it upload the file again.
	var mtimeErr *MtimeNotAppliedError
	require.ErrorAs(t, err, &mtimeErr)
	require.NotNil(t, item)
	assert.Equal(t, "empty-item", item.ID)
	assert.Same(t, item, mtimeErr.Item)
	assert.Zero(t, puts.Load(), "a zero-byte file is created by body, never PUT")
	assert.Positive(t, patches.Load())
}

// Validates: R-5.6.5
func TestUpload_ZeroByteMtimePatchTransientRetries(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		errorBody  string
	}{
		{name: "item not found", statusCode: http.StatusNotFound, errorBody: `{"error":{"code":"itemNotFound","message":"not ready"}}`},
		{name: "gateway timeout", statusCode: http.StatusGatewayTimeout, errorBody: `{"error":{"code":"UnknownError"}}`},
		{name: "resource modified", statusCode: http.StatusConflict, errorBody: `{"error":{"code":"resourceModified"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

			var patchCalls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveCreateFile(t, w, r, "retry-item") {
					return
				}

				w.Header().Set("Content-Type", "application/json")

				switch r.Method {
				case http.MethodPut:
					w.WriteHeader(http.StatusCreated)
					writeTestResponse(t, w, `{"id":"retry-item","name":"retry.txt","size":0,
						"lastModifiedDateTime":"2024-06-01T12:00:00Z","file":{}}`)
				case http.MethodPatch:
					if patchCalls.Add(1) == 1 {
						w.WriteHeader(tt.statusCode)
						writeTestResponse(t, w, tt.errorBody)

						return
					}

					w.WriteHeader(http.StatusOK)
					writeTestResponse(t, w, `{"id":"retry-item","name":"retry.txt","size":0,
						"lastModifiedDateTime":"2024-07-10T09:00:00Z","file":{}}`)
				default:
					assert.Failf(t, "unexpected request", "method=%s path=%s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			client.policies.simpleUploadMtime = testRetryPolicy()

			item, err := client.Upload(t.Context(), driveid.New("d"), "parent", "retry.txt",
				bytes.NewReader(nil), 0, mtime, nil)
			require.NoError(t, err)
			assert.Equal(t, int32(2), patchCalls.Load())
			assert.Equal(t, time.July, item.ModifiedAt.Month())
		})
	}
}

// Validates: R-5.6.5
func TestUpload_ZeroByteMtimePatchTransientNotFoundRetriesAcrossLongerLag(t *testing.T) {
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	var patchCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "lag-item") {
			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"id":"lag-item","name":"lag.txt","size":0,"file":{}}`)
		case http.MethodPatch:
			if patchCalls.Add(1) < 7 {
				w.WriteHeader(http.StatusNotFound)
				writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)

				return
			}

			w.WriteHeader(http.StatusOK)
			writeTestResponse(t, w, `{"id":"lag-item","name":"lag.txt","size":0,
				"lastModifiedDateTime":"2024-07-10T09:00:00Z","file":{}}`)
		default:
			assert.Failf(t, "unexpected request", "method=%s path=%s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	client.policies.simpleUploadMtime = retry.SimpleUploadMtimePatchPolicy()
	client.policies.simpleUploadMtime.Base = 1 * time.Millisecond
	client.policies.simpleUploadMtime.Max = 8 * time.Millisecond

	item, err := client.Upload(t.Context(), driveid.New("d"), "parent", "lag.txt",
		bytes.NewReader(nil), 0, mtime, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(7), patchCalls.Load())
	assert.Equal(t, "lag-item", item.ID)
}

// Validates: R-5.2.1
func TestUpload_ChunkedForLargeFile(t *testing.T) {
	// Files > 4 MiB should create a session and upload in chunks.
	fileSize := int64(4*1024*1024 + 1) // 4 MiB + 1 byte
	content := bytes.Repeat([]byte("X"), int(fileSize))

	var sessionCreated bool
	var chunksReceived int

	// The main server handles createUploadSession (authenticated).
	// A separate server handles the chunk uploads (pre-authenticated URL).
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// CancelUploadSession — should not be called on success.
			assert.Fail(t, "unexpected session cancel")
			w.WriteHeader(http.StatusNoContent)

			return
		}

		chunksReceived++

		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))

		// Drain the body.
		n, err := io.Copy(io.Discard, r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Positive(t, n)

		// Final chunk — return completed item.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "chunked-item-id",
			"name": "large.bin",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`, fileSize)
	}))
	defer chunkSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "chunked-created") {
			return
		}

		// createUploadSession request.
		assert.Contains(t, r.URL.Path, "createUploadSession")
		sessionCreated = true

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "large.bin",
		bytes.NewReader(content), fileSize, time.Time{}, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "chunked-item-id", item.ID)
	assert.True(t, sessionCreated, "should have created an upload session")
	assert.Equal(t, 1, chunksReceived, "file just over 4 MiB should need 1 chunk")
}

// Validates: R-5.6.3
func TestUpload_ChunkedCancelsSessionOnError(t *testing.T) {
	// When a chunk upload fails, the session should be canceled.
	fileSize := int64(4*1024*1024 + 1)
	content := bytes.Repeat([]byte("Y"), int(fileSize))

	var sessionCanceled bool

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			sessionCanceled = true
			w.WriteHeader(http.StatusNoContent)

			return
		}

		// Fail the chunk upload.
		w.WriteHeader(http.StatusInternalServerError)
		writeTestResponse(t, w, `{"error":"server error"}`)
	}))
	defer chunkSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "fail-created") {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "fail.bin",
		bytes.NewReader(content), fileSize, time.Time{}, nil,
	)
	require.Error(t, err)
	assert.True(t, sessionCanceled, "session should be canceled on chunk failure")
}

func TestUpload_ProgressCallback(t *testing.T) {
	// Verify that the progress callback is invoked with correct values.
	// Use a file size that requires exactly 2 chunks (10 MiB + 1 byte).
	fileSize := int64(ChunkedUploadChunkSize + 1) // 10 MiB + 1
	content := bytes.Repeat([]byte("P"), int(fileSize))

	chunkCount := 0

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		chunkCount++

		// Drain body.
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		if err != nil {
			return
		}

		if chunkCount < 2 {
			// Intermediate chunk.
			w.WriteHeader(http.StatusAccepted)
			writeTestResponse(t, w, `{}`)

			return
		}

		// Final chunk.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "progress-item",
			"name": "progress.bin",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`, fileSize)
	}))
	defer chunkSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "progress-created") {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	var progressCalls []int64

	progress := func(uploaded, total int64) {
		progressCalls = append(progressCalls, uploaded)
		assert.Equal(t, fileSize, total)
	}

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "progress.bin",
		bytes.NewReader(content), fileSize, time.Time{}, progress,
	)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "progress-item", item.ID)

	// Should have 2 progress calls: after first chunk (10 MiB) and after second (10 MiB + 1).
	require.Len(t, progressCalls, 2)
	assert.Equal(t, int64(ChunkedUploadChunkSize), progressCalls[0])
	assert.Equal(t, fileSize, progressCalls[1])
}

func TestUpload_NilProgress(t *testing.T) {
	// Verify that nil progress callback doesn't panic on the session route.
	content := []byte("nil-progress-file")

	rec := &sessionUploadRecorder{}
	srv := newSessionUploadServer(t, rec, http.StatusOK)
	defer srv.Close()

	item, err := newTestClient(t, srv.URL).Upload(
		t.Context(), driveid.New("d"), "parent", "nilprog.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "small-item", item.ID)
}

func TestUpload_ChunkedUploadChunkSize(t *testing.T) {
	assert.Equal(t, 10*1024*1024, ChunkedUploadChunkSize)
	assert.Equal(t, 0, ChunkedUploadChunkSize%ChunkAlignment, "chunk size must be aligned to 320 KiB")
}

// --- Pre-auth retry tests for upload operations ---

func TestUploadChunk_RetriesOn503(t *testing.T) {
	var calls atomic.Int32

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)

		// Verify body is re-read on each attempt.
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Len(t, body, 4, "retry attempt %d should have full body", n)

		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		writeTestResponse(t, w, `{}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload")}

	item, complete, err := client.UploadChunk(
		t.Context(), session,
		strings.NewReader("data"),
		0, 4, 8,
	)
	require.NoError(t, err)
	assert.False(t, complete)
	assert.Nil(t, item, "intermediate chunk should return nil item")
	assert.Equal(t, int32(3), calls.Load())
}

func TestCancelUploadSession_Unexpected2xx(t *testing.T) {
	// doPreAuth passes through any 2xx, but CancelUploadSession expects 204.
	// If the server returns 200, the explicit 204 check should fail.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeUploadTestBody(t, w, `{"unexpected":"response"}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/session")}

	err := client.CancelUploadSession(t.Context(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 200")
}

func TestCancelUploadSession_RetriesOn503(t *testing.T) {
	var calls atomic.Int32

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)

		n := calls.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/session")}

	err := client.CancelUploadSession(t.Context(), session)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

func TestResumeUpload_Success(t *testing.T) {
	totalSize := int64(ChunkedUploadChunkSize + 1024) // just over one chunk
	resumeOffset := int64(ChunkedUploadChunkSize)     // first chunk already uploaded
	content := make([]byte, totalSize)

	for i := range content {
		content[i] = byte(i % 256)
	}

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// QueryUploadSession — return status with resume offset.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"uploadUrl":          "http://" + r.Host + r.URL.Path,
				"expirationDateTime": "2025-12-31T23:59:59Z",
				"nextExpectedRanges": []string{fmt.Sprintf("%d-", resumeOffset)},
			}))

		case http.MethodPut:
			// Chunk upload — final chunk.
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			expectedLen := totalSize - resumeOffset
			assert.Len(t, body, int(expectedLen))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{
				"id": "resumed-item",
				"name": "big.bin",
				"size": `+fmt.Sprintf("%d", totalSize)+`,
				"createdDateTime": "2024-01-01T00:00:00Z",
				"lastModifiedDateTime": "2024-01-01T00:00:00Z",
				"parentReference": {"id": "parent", "driveId": "d"},
				"file": {"mimeType": "application/octet-stream"}
			}`)

		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload/session")}
	reader := bytes.NewReader(content)

	item, err := client.ResumeUpload(t.Context(), session, reader, totalSize, nil)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "resumed-item", item.ID)
}

func TestResumeUpload_SessionExpired(t *testing.T) {
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/expired")}

	_, err := client.ResumeUpload(t.Context(), session, bytes.NewReader(nil), 1024, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUploadSessionExpired)
}

// Validates: R-5.6.1
func TestUploadFromSession_RecoversFrom416UsingSessionStatus(t *testing.T) {
	totalSize := int64(ChunkedUploadChunkSize + 1024)
	resumeOffset := int64(ChunkedUploadChunkSize)
	content := bytes.Repeat([]byte("A"), int(totalSize))

	var initialPutCount atomic.Int32
	var resumedPutCount atomic.Int32

	resumedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t,
			fmt.Sprintf("bytes %d-%d/%d", resumeOffset, totalSize-1, totalSize),
			r.Header.Get("Content-Range"))
		resumedPutCount.Add(1)

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Len(t, body, int(totalSize-resumeOffset))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "recovered-item",
			"name": "big.bin",
			"size": %d,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`, totalSize)
	}))
	defer resumedSrv.Close()

	initialSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			initialPutCount.Add(1)
			assert.Equal(t,
				fmt.Sprintf("bytes 0-%d/%d", ChunkedUploadChunkSize-1, totalSize),
				r.Header.Get("Content-Range"))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			writeTestResponse(t, w, `{"error":{"code":"invalidRange"}}`)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"uploadUrl":          resumedSrv.URL + "/upload",
				"expirationDateTime": "2025-12-31T23:59:59Z",
				"nextExpectedRanges": []string{fmt.Sprintf("%d-", resumeOffset)},
			}))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer initialSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(initialSrv.URL + "/upload")}

	item, err := client.UploadFromSession(t.Context(), session, bytes.NewReader(content), totalSize, nil)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "recovered-item", item.ID)
	assert.Equal(t, int32(1), initialPutCount.Load())
	assert.Equal(t, int32(1), resumedPutCount.Load())
	assert.Equal(t, UploadURL(resumedSrv.URL+"/upload"), session.UploadURL)
}

// Validates: R-5.6.1
func TestUploadFromSession_416RecoveryRejectsMalformedRanges(t *testing.T) {
	content := bytes.Repeat([]byte("B"), int(ChunkedUploadChunkSize+1024))
	sessionSrv := newUpload416RecoveryServer(t, "not-a-range")
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/upload")}

	_, err := client.UploadFromSession(t.Context(), session, bytes.NewReader(content), int64(len(content)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nextExpectedRanges")
}

// Validates: R-5.6.1
func TestUploadFromSession_416RecoveryRejectsNonForwardProgress(t *testing.T) {
	content := bytes.Repeat([]byte("C"), int(ChunkedUploadChunkSize+1024))
	sessionSrv := newUpload416RecoveryServer(t, "0-")
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/upload")}

	_, err := client.UploadFromSession(t.Context(), session, bytes.NewReader(content), int64(len(content)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not advance")
}

// Validates: R-5.6.2
func TestCreateUploadSessionForItem_NoConditionWithoutETag(t *testing.T) {
	// A caller with no preflight eTag sends no If-Match at all, rather than
	// an empty condition.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("If-Match"),
			"no eTag, no If-Match")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/no-if-match",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSessionForItem(
		t.Context(), driveid.New("d"), "item-123", "", 10485760, time.Time{},
	)
	require.NoError(t, err)
}

// Validates: R-5.6.3
func TestUpload_ChunkedCancelsSession_CanceledContext(t *testing.T) {
	// When the parent context is canceled during a chunked upload, the session
	// cancel request must still fire using context.Background(). This prevents
	// server-side quota leaks from orphaned upload sessions.
	fileSize := int64(4*1024*1024 + 1)
	content := bytes.Repeat([]byte("Z"), int(fileSize))

	ctx, cancel := context.WithCancel(t.Context())

	var sessionCanceled atomic.Int32

	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			sessionCanceled.Add(1)
			w.WriteHeader(http.StatusNoContent)

			return
		}

		// Cancel the parent context before responding to the chunk upload.
		// This simulates a context cancellation during upload.
		cancel()

		w.WriteHeader(http.StatusInternalServerError)
		writeTestResponse(t, w, `{"error":"server error"}`)
	}))
	defer chunkSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveCreateFile(t, w, r, "cancel-created") {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Upload(
		ctx, driveid.New("d"), "parent", "cancel-test.bin",
		bytes.NewReader(content), fileSize, time.Time{}, nil,
	)
	require.Error(t, err)
	assert.Equal(t, int32(1), sessionCanceled.Load(),
		"session cancel DELETE must fire even when parent context is canceled")
}

// Validates: R-5.6.7
func TestUpload_ZeroByteIsOneCreateByBody(t *testing.T) {
	// A zero-byte file is the empty item the create makes: no session (which
	// needs a non-empty fragment) and no content PUT (L-01).
	rec := &sessionUploadRecorder{}
	srv := newSessionUploadServer(t, rec, http.StatusOK)
	defer srv.Close()

	item, err := newTestClient(t, srv.URL).Upload(
		t.Context(), driveid.New("d"), "parent", "empty.txt",
		bytes.NewReader(nil), 0, time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "small-item", item.ID)
	assert.Len(t, rec.createBodies, 1)
	assert.Empty(t, rec.sessionBodies)
	assert.Zero(t, rec.simplePuts)
	assert.Zero(t, rec.patches)
	assert.Empty(t, rec.other)
}

// Validates: R-5.6.8, R-2.16.1
func TestUpload_NoPostUploadMetadataQuery(t *testing.T) {
	// After upload completion, the system must NOT re-query file metadata.
	// Server-side processing (virus scan, indexing) can temporarily show incorrect
	// values. The upload completion response itself contains the correct metadata.
	var requestCount atomic.Int32

	content := []byte{} // zero-byte: the create is the whole upload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)

		if r.Method == http.MethodGet {
			assert.Fail(t, "unexpected GET request after upload",
				"path=%s — must not re-query metadata after upload", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "no-requery-item",
			"name": "norequery.txt",
			"size": 0,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "norequery.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "no-requery-item", item.ID)
	assert.Equal(t, int32(1), requestCount.Load(),
		"only 1 request (the create) should be made — no GET for metadata")
}

func TestQueryUploadSession_RetriesOn429(t *testing.T) {
	var calls atomic.Int32

	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/retry",
			"expirationDateTime": "2024-12-31T23:59:59Z",
			"nextExpectedRanges": ["0-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/session")}

	status, err := client.QueryUploadSession(t.Context(), session)
	require.NoError(t, err)
	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/retry"), status.UploadURL)
	assert.Equal(t, int32(2), calls.Load())
}

// Regression: Graph's session status response has no uploadUrl (verified
// live: only expirationDateTime and nextExpectedRanges), and validating the
// absent field failed every resume with "upload URL must use https", so a
// crashed 300 MB upload with 280 MB already on OneDrive restarted from zero.
//
// Validates: R-5.6.11
func TestResumeUpload_StatusWithoutUploadURLResumesOnSessionURL(t *testing.T) {
	totalSize := int64(ChunkedUploadChunkSize + 1024)
	resumeOffset := int64(ChunkedUploadChunkSize)
	content := make([]byte, totalSize)

	var putPath string
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"expirationDateTime": "2025-12-31T23:59:59Z",
				"nextExpectedRanges": []string{fmt.Sprintf("%d-%d", resumeOffset, totalSize-1)},
			}))
		case http.MethodPut:
			putPath = r.URL.Path
			body, readErr := io.ReadAll(r.Body)
			assert.NoError(t, readErr)
			assert.Len(t, body, int(totalSize-resumeOffset), "only the missing bytes are sent")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			writeTestResponse(t, w, `{"id":"resumed-item","name":"big.bin","size":`+fmt.Sprintf("%d", totalSize)+`,
				"createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z",
				"parentReference":{"id":"parent","driveId":"d"},"file":{"mimeType":"application/octet-stream"}}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(chunkSrv.URL + "/upload/session")}

	item, err := client.ResumeUpload(t.Context(), session, bytes.NewReader(content), totalSize, nil)
	require.NoError(t, err)
	assert.Equal(t, "resumed-item", item.ID)
	assert.Equal(t, "/upload/session", putPath, "the remaining chunk goes to the original session URL")
}

// Validates: R-6.8.19, R-5.3.1
func TestSimpleUpload_RetryAfter503ResendsFullBody(t *testing.T) {
	content := "content that must survive a retry"
	// Production passes a SectionReader over the open file, a type
	// http.NewRequest cannot replay by itself (unlike *strings.Reader).
	source := func() *io.SectionReader {
		return io.NewSectionReader(strings.NewReader(content), 0, int64(len(content)))
	}

	tests := []struct {
		name   string
		upload func(*Client) (*Item, error)
	}{
		{name: "OverwriteByID", upload: func(c *Client) (*Item, error) {
			return c.SimpleUploadToItem(t.Context(), driveid.New("d"), "item-1", "", source(), int64(len(content)))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			var bodies []string
			var lengths []int64
			var chunked []bool

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					return
				}
				bodies = append(bodies, string(body))
				lengths = append(lengths, r.ContentLength)
				chunked = append(chunked, len(r.TransferEncoding) > 0)

				if attempts.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				writeTestResponsef(t, w, `{"id":"item-1","name":"a.txt","size":%d,"file":{}}`, len(content))
			}))
			defer srv.Close()

			item, err := tt.upload(newTestClient(t, srv.URL))
			require.NoError(t, err)
			assert.Equal(t, "item-1", item.ID)

			// The retried PUT carries every byte again: before the fix it
			// carried none, which replaced the online file with an empty one.
			assert.Equal(t, []string{content, content}, bodies)
			assert.Equal(t, []int64{int64(len(content)), int64(len(content))}, lengths)
			assert.Equal(t, []bool{false, false}, chunked)
		})
	}
}

// Validates: R-6.7.33
func TestMutationResponsesWithoutItem(t *testing.T) {
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	bodies := []struct {
		name   string
		status int
		body   string
	}{
		{name: "204", status: http.StatusNoContent, body: ""},
		{name: "empty 200", status: http.StatusOK, body: ""},
		{name: "201 error body", status: http.StatusCreated, body: `{"error":{"code":"generalException"}}`},
		{name: "item without id", status: http.StatusOK, body: `{"name":"a.txt"}`},
	}

	for _, tc := range bodies {
		t.Run("PATCH fileSystemInfo "+tc.name, func(t *testing.T) {
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPatch:
					w.WriteHeader(tc.status)
					writeTestResponse(t, w, tc.body)
				case http.MethodGet:
					gets.Add(1)
					assert.Equal(t, "/drives/000000000000000d/items/item-1", r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					writeTestResponse(t, w, `{"id":"item-1","name":"a.txt","lastModifiedDateTime":"2024-07-10T09:00:00Z","file":{}}`)
				default:
					assert.Failf(t, "unexpected request", "%s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			// A 204 was a JSON "EOF" decode failure, so a PATCH that had
			// succeeded became retry work.
			item, err := newTestClient(t, srv.URL).UpdateFileSystemInfo(t.Context(), driveid.New("d"), "item-1", mtime)
			require.NoError(t, err)
			assert.Equal(t, "item-1", item.ID)
			assert.Equal(t, int32(1), gets.Load(), "the item is read back by ID")
		})

		t.Run("final chunk "+tc.name, func(t *testing.T) {
			if tc.status == http.StatusNoContent {
				t.Skip("a 204 on a fragment is an unexpected status, not a completion")
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				writeTestResponse(t, w, tc.body)
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
				strings.NewReader("abc"), 0, 3, 3)

			// The content landed; an item without identity must never be
			// returned as the upload result.
			require.ErrorIs(t, err, ErrUploadCompletedWithoutItem)
		})
	}
}

// Validates: R-5.6.12
func TestUploadFromSession_RecoveredOffsetPastEndIsRefused(t *testing.T) {
	content := bytes.Repeat([]byte("C"), 1024)
	sessionSrv := newUpload416RecoveryServer(t, "99999-")
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: UploadURL(sessionSrv.URL + "/upload")}

	_, err := client.UploadFromSession(t.Context(), session, bytes.NewReader(content), int64(len(content)), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "past the end")
}

// Validates: R-5.6.12
func TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain(t *testing.T) {
	content := bytes.Repeat([]byte("F"), int(ChunkedUploadChunkSize+1024))

	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("fail fragment %d", failAt), func(t *testing.T) {
			var puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				assert.NoError(t, err)
				if int(puts.Add(1)) == failAt {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()

			client := newNoRetryTestClient(t, srv.URL)
			session := &UploadSession{UploadURL: UploadURL(srv.URL + "/upload")}

			_, err := client.UploadFromSession(t.Context(), session, bytes.NewReader(content), int64(len(content)), nil)
			require.Error(t, err)

			// Only a failed last fragment may have committed the file.
			assert.Equal(t, failAt == 2, IsFinalFragmentUncertain(err))
		})
	}
}
