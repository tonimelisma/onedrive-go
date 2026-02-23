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
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))

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
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default())
	client.sleepFunc = noopSleep

	_, err := client.SimpleUpload(
		context.Background(), driveid.New("d"), "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestSimpleUpload_NetworkError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default())
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

	chunkData := bytes.Repeat([]byte("A"), chunkAlignment)
	item, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(chunkData),
		0, int64(chunkAlignment), 2*int64(chunkAlignment),
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

	chunkData := bytes.Repeat([]byte("B"), chunkAlignment)
	totalSize := 2 * int64(chunkAlignment)
	item, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(chunkData),
		int64(chunkAlignment), int64(chunkAlignment), totalSize,
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

	offset := int64(chunkAlignment)
	length := int64(chunkAlignment)
	total := int64(3 * chunkAlignment)

	_, err := client.UploadChunk(
		context.Background(), session,
		bytes.NewReader(make([]byte, chunkAlignment)),
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
	assert.Contains(t, err.Error(), "status 500")
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
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))

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
	assert.Contains(t, err.Error(), "status 404")
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
	assert.Equal(t, 327680, chunkAlignment)
}

func TestSimpleUploadMaxSize(t *testing.T) {
	assert.Equal(t, 4194304, simpleUploadMaxSize)
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
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))

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
	assert.Contains(t, err.Error(), "query upload session request failed")
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

func TestHandleChunkResponse_416DrainError(t *testing.T) {
	// Verify that handleChunkResponse returns a drain error when the 416
	// response body fails to read during drain.
	client := newTestClient(t, "http://unused")

	resp := &http.Response{
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Body:       errorReadCloser{},
	}

	_, err := client.handleChunkResponse(resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "draining 416 response body")
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
