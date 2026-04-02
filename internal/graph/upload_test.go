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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// Validates: R-1.3, R-5.3
func TestSimpleUpload_Success(t *testing.T) {
	content := "simple upload file content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/parent:/upload.txt:/content", r.URL.Path)
		assert.Equal(t, "replace", r.URL.Query().Get("@microsoft.graph.conflictBehavior"))

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, content, string(body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "new-item-1",
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
	item, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "parent", "upload.txt",
		strings.NewReader(content), int64(len(content)),
	)
	require.NoError(t, err)

	assert.Equal(t, "new-item-1", item.ID)
	assert.Equal(t, "upload.txt", item.Name)
	assert.Equal(t, int64(len(content)), item.Size)
}

// Validates: R-5.6.9
func TestSimpleUpload_ConflictBehaviorReplace(t *testing.T) {
	// Verify that SimpleUpload passes conflictBehavior=replace as a query
	// parameter, matching CreateUploadSession which passes it in the JSON body.
	// Without this, SimpleUpload relies on undocumented API default behavior.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "replace", r.URL.Query().Get("@microsoft.graph.conflictBehavior"),
			"SimpleUpload must pass conflictBehavior=replace query param")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "cb-item",
			"name": "conflict.txt",
			"size": 4,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "conflict.txt",
		strings.NewReader("data"), 4,
	)
	require.NoError(t, err)
	assert.Equal(t, "cb-item", item.ID)
}

func TestSimpleUpload_ContentType(t *testing.T) {
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
	_, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "binary.dat",
		strings.NewReader("data"), 4,
	)
	require.NoError(t, err)
}

func TestSimpleUpload_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-upload-err")
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "forbidden.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestSimpleUpload_TokenError(t *testing.T) {
	client := MustNewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")

	_, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestSimpleUpload_NetworkError(t *testing.T) {
	client := MustNewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")

	_, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw upload request failed")
}

func TestSimpleUpload_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{not valid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUpload(
		t.Context(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding simple upload response")
}

func TestCreateUploadSession_Success(t *testing.T) {
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
	session, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "large-file.bin", 10485760, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/abc123"), session.UploadURL)
	assert.Equal(t, 2024, session.ExpirationTime.Year())
	assert.Equal(t, 12, int(session.ExpirationTime.Month()))
	assert.Equal(t, 31, session.ExpirationTime.Day())
}

func TestCreateUploadSession_InvalidExpiration(t *testing.T) {
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
	session, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/xyz"), session.UploadURL)
	assert.True(t, session.ExpirationTime.IsZero())
}

func TestCreateUploadSession_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-session-err")
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestCreateUploadSession_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{invalid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
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

// Validates: R-5.3, R-6.7.6
func TestChunkAlignment(t *testing.T) {
	assert.Equal(t, 327680, ChunkAlignment)
}

func TestSimpleUploadMaxSize(t *testing.T) {
	assert.Equal(t, 4194304, SimpleUploadMaxSize)
}

// Validates: R-5.6.4
func TestCreateUploadSession_WithFileSystemInfo(t *testing.T) {
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
	session, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "timestamped.bin", 5242880, mtime,
	)
	require.NoError(t, err)
	assert.Equal(t, UploadURL("https://uploads.contoso.sharepoint.com/session/fsi"), session.UploadURL)
}

// Validates: R-5.6.4
func TestCreateUploadSession_WithoutFileSystemInfo(t *testing.T) {
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
	session, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "no-timestamp.bin", 5242880, time.Time{},
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
	assert.Contains(t, err.Error(), "decoding final chunk response")
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

func TestUpload_SimpleForSmallFile(t *testing.T) {
	// Files <= 4 MiB should use simple upload (single PUT), not create a session.
	content := []byte("small file content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simple upload goes to /drives/{driveID}/items/{parentID}:/{name}:/content
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Contains(t, r.URL.Path, "/content")
		assert.NotContains(t, r.URL.Path, "createUploadSession")

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, content, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "simple-item-id",
			"name": "small.txt",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`, len(content))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "small.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "simple-item-id", item.ID)
	assert.Equal(t, "small.txt", item.Name)
}

// Validates: R-5.6.5
func TestUpload_SimplePreservesMtime(t *testing.T) {
	// When mtime is non-zero, Upload() should call UpdateFileSystemInfo
	// after the simple upload to preserve local mtime on the server.
	content := []byte("small mtime file")
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	var patchCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPut {
			// Simple upload — return item.
			w.WriteHeader(http.StatusCreated)
			writeTestResponsef(t, w, `{
				"id": "mtime-item",
				"name": "mtime.txt",
				"size": %d,
				"createdDateTime": "2024-06-01T12:00:00Z",
				"lastModifiedDateTime": "2024-06-01T12:00:00Z",
				"parentReference": {"id": "parent", "driveId": "d"},
				"file": {"mimeType": "text/plain"}
			}`, len(content))

			return
		}

		if r.Method == http.MethodPatch {
			// UpdateFileSystemInfo PATCH.
			patchCalled = true

			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				return
			}
			assert.Contains(t, string(body), `"lastModifiedDateTime":"2024-07-10T09:00:00Z"`)
			assert.Equal(t, "/drives/000000000000000d/items/mtime-item", r.URL.Path)

			w.WriteHeader(http.StatusOK)
			writeTestResponsef(t, w, `{
				"id": "mtime-item",
				"name": "mtime.txt",
				"size": %d,
				"createdDateTime": "2024-06-01T12:00:00Z",
				"lastModifiedDateTime": "2024-07-10T09:00:00Z",
				"parentReference": {"id": "parent", "driveId": "d"},
				"file": {"mimeType": "text/plain"}
			}`, len(content))

			return
		}

		assert.Failf(t, "unexpected method", "got %s", r.Method)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "mtime.txt",
		bytes.NewReader(content), int64(len(content)), mtime, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "mtime-item", item.ID)
	assert.True(t, patchCalled, "UpdateFileSystemInfo should be called for non-zero mtime")
	assert.Equal(t, 2024, item.ModifiedAt.Year())
	assert.Equal(t, time.July, item.ModifiedAt.Month())
}

func TestUpload_SimpleSkipsPatchForZeroMtime(t *testing.T) {
	// When mtime is zero, Upload() should NOT call UpdateFileSystemInfo.
	content := []byte("no mtime file")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			assert.Fail(t, "PATCH should not be called when mtime is zero")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "no-mtime-item",
			"name": "no-mtime.txt",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`, len(content))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "no-mtime.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "no-mtime-item", item.ID)
}

func TestUpload_SimpleMtimePatchFailure(t *testing.T) {
	// When the PATCH fails after simple upload, Upload() should return an error.
	content := []byte("patch-fail")
	mtime := time.Date(2024, 7, 10, 9, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated)
			writeTestResponsef(t, w, `{
				"id": "patch-fail-item",
				"name": "fail.txt",
				"size": %d,
				"createdDateTime": "2024-06-01T12:00:00Z",
				"lastModifiedDateTime": "2024-06-01T12:00:00Z",
				"parentReference": {"id": "parent", "driveId": "d"},
				"file": {"mimeType": "text/plain"}
			}`, len(content))

			return
		}

		if r.Method == http.MethodPatch {
			w.Header().Set("request-id", "req-patch-fail")
			w.WriteHeader(http.StatusInternalServerError)
			writeTestResponse(t, w, `{"error":"server error"}`)

			return
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "fail.txt",
		bytes.NewReader(content), int64(len(content)), mtime, nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "setting mtime after simple upload")
}

func TestUpload_ChunkedForLargeFile(t *testing.T) {
	// Files > 4 MiB should create a session and upload in chunks.
	fileSize := int64(SimpleUploadMaxSize + 1) // 4 MiB + 1 byte
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

func TestUpload_ChunkedCancelsSessionOnError(t *testing.T) {
	// When a chunk upload fails, the session should be canceled.
	fileSize := int64(SimpleUploadMaxSize + 1)
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	// Verify that nil progress callback doesn't panic.
	content := []byte("nil-progress-file")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "nil-prog-item",
			"name": "nilprog.txt",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`, len(content))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	// Should not panic with nil progress.
	item, err := client.Upload(
		t.Context(), driveid.New("d"), "parent", "nilprog.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "nil-prog-item", item.ID)
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

// Validates: R-5.6.2
func TestCreateUploadSession_NoIfMatchHeader(t *testing.T) {
	// Upload session creation must NOT include an If-Match header.
	// The eTag can change during session creation itself (server-side race),
	// causing an immediate 412 Precondition Failed that cascades into 416 errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("If-Match"),
			"CreateUploadSession must not send If-Match header")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"uploadUrl": "https://uploads.contoso.sharepoint.com/session/no-if-match",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSession(
		t.Context(), driveid.New("d"), "parent", "file.bin", 10485760, time.Time{},
	)
	require.NoError(t, err)
}

// Validates: R-5.6.3
func TestUpload_ChunkedCancelsSession_CanceledContext(t *testing.T) {
	// When the parent context is canceled during a chunked upload, the session
	// cancel request must still fire using context.Background(). This prevents
	// server-side quota leaks from orphaned upload sessions.
	fileSize := int64(SimpleUploadMaxSize + 1)
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
func TestUpload_ZeroByte_UsesSimple(t *testing.T) {
	// Zero-byte files must use simple PUT upload, not create an upload session.
	// The upload session API requires at least one non-empty fragment.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method,
			"zero-byte upload should use PUT (simple upload)")
		assert.Contains(t, r.URL.Path, "/content",
			"zero-byte upload should target /content endpoint")
		assert.NotContains(t, r.URL.Path, "createUploadSession",
			"zero-byte upload must not create an upload session")

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		assert.Empty(t, body, "zero-byte upload body should be empty")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{
			"id": "zero-byte-item",
			"name": "empty.txt",
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
		t.Context(), driveid.New("d"), "parent", "empty.txt",
		bytes.NewReader(nil), 0, time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "zero-byte-item", item.ID)
	assert.Equal(t, int64(0), item.Size)
}

// Validates: R-5.6.8
func TestUpload_NoPostUploadMetadataQuery(t *testing.T) {
	// After upload completion, the system must NOT re-query file metadata.
	// Server-side processing (virus scan, indexing) can temporarily show incorrect
	// values. The upload completion response itself contains the correct metadata.
	var requestCount atomic.Int32

	content := []byte("no-requery-content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)

		if r.Method == http.MethodGet {
			assert.Fail(t, "unexpected GET request after upload",
				"path=%s — must not re-query metadata after upload", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{
			"id": "no-requery-item",
			"name": "norequery.txt",
			"size": %d,
			"createdDateTime": "2024-06-01T12:00:00Z",
			"lastModifiedDateTime": "2024-06-01T12:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"}
		}`, len(content))
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
		"only 1 request (the PUT upload) should be made — no GET for metadata")
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
