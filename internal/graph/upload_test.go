package graph

import (
	"bytes"
	"context"
	"encoding/json"
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
)

func TestSimpleUpload_Success(t *testing.T) {
	content := "simple upload file content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/drives/d/items/parent:/upload.txt:/content", r.URL.Path)

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
		context.Background(), "d", "parent", "upload.txt",
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
		context.Background(), "d", "p", "binary.dat",
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
		context.Background(), "d", "p", "forbidden.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestSimpleUpload_TokenError(t *testing.T) {
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default())
	client.sleepFunc = noopSleep

	_, err := client.SimpleUpload(
		context.Background(), "d", "p", "file.txt",
		strings.NewReader("data"), 4,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token")
}

func TestSimpleUpload_NetworkError(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default())
	client.sleepFunc = noopSleep

	_, err := client.SimpleUpload(
		context.Background(), "d", "p", "file.txt",
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
		context.Background(), "d", "p", "file.txt",
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
		context.Background(), "d", "parent", "large-file.bin", 10485760, time.Time{},
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
		context.Background(), "d", "parent", "file.bin", 1024, time.Time{},
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
		context.Background(), "d", "parent", "file.bin", 1024, time.Time{},
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
		context.Background(), "d", "parent", "file.bin", 1024, time.Time{},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding upload session response")
}

func TestCreateUploadSession_WithFileSystemInfo(t *testing.T) {
	// Verify that when mtime is non-zero, the request body includes fileSystemInfo.
	mtime := time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &parsed))

		item, ok := parsed["item"].(map[string]interface{})
		require.True(t, ok, "item field should exist")

		fsi, ok := item["fileSystemInfo"].(map[string]interface{})
		require.True(t, ok, "fileSystemInfo field should exist when mtime is non-zero")

		expectedTS := "2025-03-15T10:30:00Z"
		assert.Equal(t, expectedTS, fsi["createdDateTime"])
		assert.Equal(t, expectedTS, fsi["lastModifiedDateTime"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/fsi",
			"expirationDateTime": "2025-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), "d", "parent", "timestamped.bin", 1024, mtime,
	)
	require.NoError(t, err)
	assert.Equal(t, "https://upload.example.com/session/fsi", session.UploadURL)
}

func TestCreateUploadSession_WithoutFileSystemInfo(t *testing.T) {
	// Verify that when mtime is zero, fileSystemInfo is omitted from the request body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &parsed))

		item, ok := parsed["item"].(map[string]interface{})
		require.True(t, ok, "item field should exist")

		_, hasFSI := item["fileSystemInfo"]
		assert.False(t, hasFSI, "fileSystemInfo should be omitted when mtime is zero")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/nofsi",
			"expirationDateTime": "2025-12-31T23:59:59Z"
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	session, err := client.CreateUploadSession(
		context.Background(), "d", "parent", "no-timestamp.bin", 1024, time.Time{},
	)
	require.NoError(t, err)
	assert.Equal(t, "https://upload.example.com/session/nofsi", session.UploadURL)
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

func TestUploadChunk_416_RangeNotSatisfiable(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		fmt.Fprint(w, `{"error":{"code":"invalidRange"}}`)
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

func TestQueryUploadSession_Success(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"), "pre-authenticated URL should not send auth")
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/abc123",
			"expirationDateTime": "2025-06-01T12:00:00Z",
			"nextExpectedRanges": ["327680-"]
		}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/abc123"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)

	assert.Equal(t, "https://upload.example.com/session/abc123", status.UploadURL)
	assert.Equal(t, 2025, status.ExpirationTime.Year())
	assert.Equal(t, time.June, status.ExpirationTime.Month())
	require.Len(t, status.NextExpectedRanges, 1)
	assert.Equal(t, "327680-", status.NextExpectedRanges[0])
}

func TestQueryUploadSession_Expired(t *testing.T) {
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/expired"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "expired or not found")
}

func TestQueryUploadSession_NetworkError(t *testing.T) {
	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: "http://127.0.0.1:1/session"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query upload session request failed")
}

func TestErrRangeNotSatisfiable_Classification(t *testing.T) {
	sentinel := classifyStatus(http.StatusRequestedRangeNotSatisfiable)
	assert.Equal(t, ErrRangeNotSatisfiable, sentinel)
}

func TestQueryUploadSession_OtherError(t *testing.T) {
	// Test non-200, non-404 status code path.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"server error"}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/err"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestQueryUploadSession_DecodeError(t *testing.T) {
	// Test JSON decode error on 200 response.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{invalid json`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/bad"}

	_, err := client.QueryUploadSession(context.Background(), session)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding upload session status")
}

func TestQueryUploadSession_InvalidExpiration(t *testing.T) {
	// Test invalid expiration time parsing — should succeed with zero time.
	chunkSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"uploadUrl": "https://upload.example.com/session/inv",
			"expirationDateTime": "not-valid",
			"nextExpectedRanges": ["0-"]
		}`)
	}))
	defer chunkSrv.Close()

	client := newTestClient(t, "http://unused")
	session := &UploadSession{UploadURL: chunkSrv.URL + "/session/inv"}

	status, err := client.QueryUploadSession(context.Background(), session)
	require.NoError(t, err)
	assert.True(t, status.ExpirationTime.IsZero())
	assert.Equal(t, "https://upload.example.com/session/inv", status.UploadURL)
}
