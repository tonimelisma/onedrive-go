package graph

import (
	"bytes"
	"context"
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

func TestSimpleUpload_Success(t *testing.T) {
	content := "simple upload file content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/drives/000000000000000d/items/parent:/upload.txt:/content", r.URL.Path)

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, content, string(body))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
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
		context.Background(), driveid.New("d"), "parent", "upload.txt",
		strings.NewReader(content), int64(len(content)),
	)
	require.NoError(t, err)

	assert.Equal(t, "new-item-1", item.ID)
	assert.Equal(t, "upload.txt", item.Name)
	assert.Equal(t, int64(len(content)), item.Size)
}

func TestSimpleUpload_ContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{
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
		context.Background(), driveid.New("d"), "p", "binary.dat",
		strings.NewReader("data"), 4,
	)
	require.NoError(t, err)
}

func TestSimpleUpload_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-upload-err")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUpload(
		context.Background(), driveid.New("d"), "p", "forbidden.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestSimpleUpload_TokenError(t *testing.T) {
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	_, err := client.SimpleUpload(
		context.Background(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestSimpleUpload_NetworkError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	_, err := client.SimpleUpload(
		context.Background(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw upload request failed")
}

func TestSimpleUpload_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.SimpleUpload(
		context.Background(), driveid.New("d"), "p", "file.txt",
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
		require.NoError(t, err)
		assert.Contains(t, string(body), `"@microsoft.graph.conflictBehavior":"replace"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/abc123",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "large-file.bin", 10485760, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, "https://upload.example.com/session/abc123", session.UploadURL)
	assert.Equal(t, 2024, session.ExpirationTime.Year())
	assert.Equal(t, 12, int(session.ExpirationTime.Month()))
	assert.Equal(t, 31, session.ExpirationTime.Day())
}

func TestCreateUploadSession_InvalidExpiration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/xyz",
			"expirationDateTime": "not-a-date"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
	)
	require.NoError(t, err)

	assert.Equal(t, "https://upload.example.com/session/xyz", session.UploadURL)
	assert.True(t, session.ExpirationTime.IsZero())
}

func TestCreateUploadSession_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-session-err")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"accessDenied"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestCreateUploadSession_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{invalid json`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "file.bin", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding upload session response")
}

func TestUploadChunk_Intermediate(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
		assert.Empty(t, r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"nextExpectedRanges":["327680-"]}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	chunkData := bytes.Repeat([]byte("A"), ChunkAlignment)
	item, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(chunkData),
		0, int64(ChunkAlignment), 2*int64(ChunkAlignment),
	)
	require.NoError(t, err)
	assert.Nil(t, item, "intermediate chunk should return nil item")
}

func TestUploadChunk_Final(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{
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
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	chunkData := bytes.Repeat([]byte("B"), ChunkAlignment)
	totalSize := 2 * int64(ChunkAlignment)
	item, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(chunkData),
		int64(ChunkAlignment), int64(ChunkAlignment), totalSize,
	)
	require.NoError(t, err)
	require.NotNil(t, item, "final chunk should return completed item")

	assert.Equal(t, "completed-item", item.ID)
	assert.Equal(t, "large-file.bin", item.Name)
	assert.Equal(t, "aGFzaA==", item.QuickXorHash)
}

func TestUploadChunk_FinalWith200(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
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
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	item, err := client.UploadChunk(
		context.Background(), session,
		strings.NewReader("final-data"),
		0, 10, 10,
	)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "updated-item", item.ID)
}

func TestUploadChunk_ContentRange(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentRange := r.Header.Get("Content-Range")
		assert.Equal(t, "bytes 327680-655359/983040", contentRange)

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	offset := int64(ChunkAlignment)
	length := int64(ChunkAlignment)
	total := int64(3 * ChunkAlignment)

	_, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(make([]byte, ChunkAlignment)),
		offset, length, total,
	)
	require.NoError(t, err)
}

func TestUploadChunk_ServerError(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"internal"}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	_, err := client.UploadChunk(
		context.Background(), session,
		strings.NewReader("data"),
		0, 4, 4,
	)
	require.Error(t, err)
	// 500 is retryable; after exhausting retries, doPreAuthRetry returns *GraphError.
	assert.ErrorIs(t, err, ErrServerError)
}

func TestUploadChunk_ContextCanceled(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/upload"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.UploadChunk(
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
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/abc"}

	err := client.CancelUploadSession(context.Background(), session)
	require.NoError(t, err)
}

func TestCancelUploadSession_Error(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/gone"}

	err := client.CancelUploadSession(context.Background(), session)
	require.Error(t, err)
	// 404 is non-retryable; doPreAuthRetry returns *GraphError with ErrNotFound.
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCancelUploadSession_ContextCanceled(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/session"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.CancelUploadSession(ctx, session)
	require.Error(t, err)
}

func TestChunkAlignment(t *testing.T) {
	assert.Equal(t, 327680, ChunkAlignment)
}

func TestSimpleUploadMaxSize(t *testing.T) {
	assert.Equal(t, 4194304, SimpleUploadMaxSize)
}

func TestCreateUploadSession_WithFileSystemInfo(t *testing.T) {
	mtime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		// Verify fileSystemInfo is included with the correct timestamp.
		bodyStr := string(body)
		assert.Contains(t, bodyStr, `"lastModifiedDateTime":"2024-06-15T10:30:00Z"`)
		assert.Contains(t, bodyStr, `"fileSystemInfo"`)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/fsi",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "timestamped.bin", 5242880, mtime,
	)
	require.NoError(t, err)
	assert.Equal(t, "https://upload.example.com/session/fsi", session.UploadURL)
}

func TestCreateUploadSession_WithoutFileSystemInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		// Verify fileSystemInfo is NOT included when mtime is zero.
		bodyStr := string(body)
		assert.NotContains(t, bodyStr, "fileSystemInfo")
		assert.NotContains(t, bodyStr, "lastModifiedDateTime")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/nofsi",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), driveid.New("d"), "parent", "no-timestamp.bin", 5242880, time.Time{},
	)
	require.NoError(t, err)
	assert.Equal(t, "https://upload.example.com/session/nofsi", session.UploadURL)
}

func TestUploadChunk_416_RangeNotSatisfiable(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		fmt.Fprint(w, `{"error":"range not satisfiable"}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	_, err := client.UploadChunk(
		context.Background(), session,
		strings.NewReader("data"),
		0, 4, 4,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRangeNotSatisfiable)
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
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/resume",
			"expirationDateTime": "2024-12-31T23:59:59Z",
			"nextExpectedRanges": ["327680-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/session"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)

	assert.Equal(t, "https://upload.example.com/session/resume", status.UploadURL)
	assert.Equal(t, 2024, status.ExpirationTime.Year())
	assert.Equal(t, []string{"327680-"}, status.NextExpectedRanges)
}

func TestQueryUploadSession_Expired(t *testing.T) {
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"session not found"}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/session"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestQueryUploadSession_NetworkError(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/session"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	// doPreAuthRetry retries network errors, then returns "failed after N retries".
	assert.Contains(t, err.Error(), "failed after 5 retries")
}

func TestHandleChunkResponse_FinalDecodeError(t *testing.T) {
	// Verify that handleChunkResponse returns a decode error when the final
	// chunk response (201) contains invalid JSON.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	_, err := client.UploadChunk(
		context.Background(), session,
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

	_, err := client.handleChunkResponse(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "draining chunk response body")
}

func TestQueryUploadSession_DecodeError(t *testing.T) {
	// Verify that QueryUploadSession returns a decode error when the server
	// returns 200 with invalid JSON.
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/session"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding session status response")
}

func TestQueryUploadSession_InvalidExpiration(t *testing.T) {
	// Verify that QueryUploadSession handles an unparseable expirationDateTime
	// gracefully by using zero time (matching CreateUploadSession behavior).
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/inv-exp",
			"expirationDateTime": "not-a-date",
			"nextExpectedRanges": ["0-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/session"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)

	assert.Equal(t, "https://upload.example.com/session/inv-exp", status.UploadURL)
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
		require.NoError(t, err)
		assert.Equal(t, content, body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
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
		context.Background(), driveid.New("d"), "parent", "small.txt",
		bytes.NewReader(content), int64(len(content)), time.Time{}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "simple-item-id", item.ID)
	assert.Equal(t, "small.txt", item.Name)
}

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
			fmt.Fprintf(w, `{
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
			require.NoError(t, err)
			assert.Contains(t, string(body), `"lastModifiedDateTime":"2024-07-10T09:00:00Z"`)
			assert.Equal(t, "/drives/000000000000000d/items/mtime-item", r.URL.Path)

			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{
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

		t.Errorf("unexpected method: %s", r.Method)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		context.Background(), driveid.New("d"), "parent", "mtime.txt",
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
			t.Error("PATCH should not be called when mtime is zero")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
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
		context.Background(), driveid.New("d"), "parent", "no-mtime.txt",
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
			fmt.Fprintf(w, `{
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
			fmt.Fprint(w, `{"error":"server error"}`)

			return
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Upload(
		context.Background(), driveid.New("d"), "parent", "fail.txt",
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
			t.Error("unexpected session cancel")
			w.WriteHeader(http.StatusNoContent)

			return
		}

		chunksReceived++

		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))

		// Drain the body.
		n, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		assert.Greater(t, n, int64(0))

		// Final chunk — return completed item.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
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
		fmt.Fprintf(w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	item, err := client.Upload(
		context.Background(), driveid.New("d"), "parent", "large.bin",
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
		fmt.Fprint(w, `{"error":"server error"}`)
	}))
	defer chunkSrv.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"uploadUrl": "%s/upload",
			"expirationDateTime": "2024-12-31T23:59:59Z"
		}`, chunkSrv.URL)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Upload(
		context.Background(), driveid.New("d"), "parent", "fail.bin",
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
		_, _ = io.Copy(io.Discard, r.Body)

		if chunkCount < 2 {
			// Intermediate chunk.
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{}`)

			return
		}

		// Final chunk.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{
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
		fmt.Fprintf(w, `{
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
		context.Background(), driveid.New("d"), "parent", "progress.bin",
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
		fmt.Fprintf(w, `{
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
		context.Background(), driveid.New("d"), "parent", "nilprog.txt",
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
		require.NoError(t, err)
		assert.Equal(t, 4, len(body), "retry attempt %d should have full body", n)

		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload"}

	item, err := client.UploadChunk(
		context.Background(), session,
		strings.NewReader("data"),
		0, 4, 8,
	)
	require.NoError(t, err)
	assert.Nil(t, item, "intermediate chunk should return nil item")
	assert.Equal(t, int32(3), calls.Load())
}

func TestCancelUploadSession_Unexpected2xx(t *testing.T) {
	// doPreAuthRetry passes through any 2xx, but CancelUploadSession expects 204.
	// If the server returns 200, the explicit 204 check should fail.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected":"response"}`))
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session"}

	err := client.CancelUploadSession(context.Background(), session)
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
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session"}

	err := client.CancelUploadSession(context.Background(), session)
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
			fmt.Fprintf(w, `{
				"uploadUrl": %q,
				"expirationDateTime": "2025-12-31T23:59:59Z",
				"nextExpectedRanges": ["%d-"]
			}`, "http://"+r.Host+r.URL.Path, resumeOffset)

		case http.MethodPut:
			// Chunk upload — final chunk.
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			expectedLen := totalSize - resumeOffset
			assert.Equal(t, int(expectedLen), len(body))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{
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
	session := &UploadSession{UploadURL: chunkSrv.URL + "/upload/session"}
	reader := bytes.NewReader(content)

	item, err := client.ResumeUpload(context.Background(), session, reader, totalSize, nil)
	require.NoError(t, err)
	require.NotNil(t, item)
	assert.Equal(t, "resumed-item", item.ID)
}

func TestResumeUpload_SessionExpired(t *testing.T) {
	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/expired"}

	_, err := client.ResumeUpload(context.Background(), session, bytes.NewReader(nil), 1024, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUploadSessionExpired)
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
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/retry",
			"expirationDateTime": "2024-12-31T23:59:59Z",
			"nextExpectedRanges": ["0-"]
		}`)
	}))
	defer sessionSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: sessionSrv.URL + "/session"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)
	assert.Equal(t, "https://upload.example.com/session/retry", status.UploadURL)
	assert.Equal(t, int32(2), calls.Load())
}
