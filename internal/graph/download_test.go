package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// errorWriter is an io.Writer that always returns an error.
// Used to test the io.Copy failure path in downloadFromURL.
type errorWriter struct{}

func (errorWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

const (
	staleDownloadPath = "/stale"
	freshDownloadPath = "/fresh"
)

func writeDownloadTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

func assertDownloadError(t *testing.T, srv *httptest.Server, itemID string, want error) {
	t.Helper()
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	client.policies.downloadMetadata = testRetryPolicy()
	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), itemID, &buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

// Validates: R-1.2
func TestDownload_Success(t *testing.T) {
	fileContent := "Hello, this is the file content for download testing."

	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, fileContent)
	}))
	defer downloadSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000000000000d/items/item-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-1",
			"name": "test.txt",
			"size": %d,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, len(fileContent), downloadSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-1", &buf)
	require.NoError(t, err)
	assert.Equal(t, fileContent, buf.String())
	assert.Equal(t, int64(len(fileContent)), n)
}

func TestDownload_EmptyDownloadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "folder-1",
			"name": "Documents",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "root", "driveId": "d"},
			"folder": {"childCount": 5}
		}`)
	}))
	assertDownloadError(t, srv, "folder-1", ErrNoDownloadURL)
}

func TestDownload_ItemNotFound(t *testing.T) {
	assertDownloadError(t, newGraphErrorServer(t, http.StatusNotFound, "req-dl-404", "itemNotFound", nil), "nonexistent", ErrNotFound)
}

func TestDownload_RetriesTransientMetadataNotFound(t *testing.T) {
	var itemCalls atomic.Int32

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, "retry after metadata")
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000000000000d/items/item-retry-metadata", r.URL.Path)

		call := itemCalls.Add(1)
		if call <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req-md-404")
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-retry-metadata", "name": "retry.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	client.policies.downloadMetadata = testRetryPolicy()

	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-retry-metadata", &buf)
	require.NoError(t, err)
	assert.Equal(t, "retry after metadata", buf.String())
	assert.Equal(t, int64(len("retry after metadata")), n)
	assert.Equal(t, int32(3), itemCalls.Load())
}

func TestDownload_VerifyBytesWritten(t *testing.T) {
	content := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, content)
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-2", "name": "data.bin", "size": %d,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"},
			"@microsoft.graph.downloadUrl": %q
		}`, len(content), dlSrv.URL+"/f")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-2", &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), n)
	assert.Equal(t, len(content), buf.Len())
}

func TestDownload_ServerError(t *testing.T) {
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-3", "name": "fail.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/fail")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "item-3", &buf)
	require.Error(t, err)
	// 500 is retryable; doPreAuthRetry exhausts retries, returns *GraphError.
	assert.ErrorIs(t, err, ErrServerError)
}

func TestDownload_NetworkError(t *testing.T) {
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "item-net", "name": "net.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": "http://127.0.0.1:1/unreachable"
		}`)
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "item-net", &buf)
	require.Error(t, err)
	// RetryTransport exhausts retries, then doPreAuth wraps the network error.
	assert.Contains(t, err.Error(), "download failed")
}

func TestDownloadFromURL_WriterError(t *testing.T) {
	// Verify that downloadFromURL returns an error when the writer fails mid-stream.
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, "some data that will fail to write")
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-ew", "name": "fail-write.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	_, err := client.Download(t.Context(), driveid.New("d"), "item-ew", errorWriter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streaming download content")
}

func TestDownload_NoAuthOnPreAuthURL(t *testing.T) {
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, "ok")
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-4", "name": "noauth.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/noauth")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "item-4", &buf)
	require.NoError(t, err)
}

func TestDownload_RefreshesPreAuthURLAfterUnauthorized(t *testing.T) {
	var metadataCalls atomic.Int32
	var staleDownloadCalls atomic.Int32
	var freshDownloadCalls atomic.Int32

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))

		switch r.URL.Path {
		case staleDownloadPath:
			staleDownloadCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req-stale-download-url")
			w.WriteHeader(http.StatusUnauthorized)
			writeTestResponse(t, w, `{"error":{"code":"unauthenticated","message":"expired pre-auth URL"}}`)
		case freshDownloadPath:
			freshDownloadCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			writeDownloadTestBody(t, w, "fresh content")
		default:
			http.NotFound(w, r)
		}
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000000000000d/items/item-refresh-url", r.URL.Path)

		call := metadataCalls.Add(1)
		downloadPath := staleDownloadPath
		if call > 1 {
			downloadPath = freshDownloadPath
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-refresh-url",
			"name": "refresh.txt",
			"size": 13,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+downloadPath)
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-refresh-url", &buf)
	require.NoError(t, err)

	assert.Equal(t, "fresh content", buf.String())
	assert.Equal(t, int64(len("fresh content")), n)
	assert.Equal(t, int32(2), metadataCalls.Load())
	assert.Equal(t, int32(1), staleDownloadCalls.Load())
	assert.Equal(t, int32(1), freshDownloadCalls.Load())
}

func TestDownload_DoesNotRefreshPreAuthURLAfterForbidden(t *testing.T) {
	var metadataCalls atomic.Int32

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-forbidden-download-url")
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"accessDenied","message":"download forbidden"}}`)
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		metadataCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-forbidden-url",
			"name": "forbidden.txt",
			"size": 9,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/forbidden")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "item-forbidden-url", &buf)
	require.Error(t, err)

	require.ErrorIs(t, err, ErrForbidden)
	assert.Equal(t, int32(1), metadataCalls.Load())
}

// Validates: R-1.2
func TestDownloadRange_Success(t *testing.T) {
	fullContent := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	offset := int64(10)

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		rangeHdr := r.Header.Get("Range")
		assert.Equal(t, fmt.Sprintf("bytes=%d-", offset), rangeHdr)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(fullContent)-1, len(fullContent)))
		w.WriteHeader(http.StatusPartialContent)
		writeDownloadTestBody(t, w, fullContent[offset:])
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-range", "name": "range.txt", "size": %d, "eTag": "v1",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, len(fullContent), dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.DownloadRange(t.Context(), driveid.New("d"), "item-range", &buf, offset, DownloadVersion{ETag: "v1"})
	require.NoError(t, err)
	assert.Equal(t, fullContent[offset:], buf.String())
	assert.Equal(t, int64(len(fullContent))-offset, n)
}

func TestDownloadRange_RefreshesPreAuthURLAfterUnauthorized(t *testing.T) {
	const fullContent = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const offset = int64(10)

	var metadataCalls atomic.Int32
	var staleDownloadCalls atomic.Int32
	var freshDownloadCalls atomic.Int32

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, fmt.Sprintf("bytes=%d-", offset), r.Header.Get("Range"))

		switch r.URL.Path {
		case staleDownloadPath:
			staleDownloadCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req-stale-range-url")
			w.WriteHeader(http.StatusUnauthorized)
			writeTestResponse(t, w, `{"error":{"code":"unauthenticated","message":"expired range URL"}}`)
		case freshDownloadPath:
			freshDownloadCalls.Add(1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(fullContent)-1, len(fullContent)))
			w.WriteHeader(http.StatusPartialContent)
			writeDownloadTestBody(t, w, fullContent[offset:])
		default:
			http.NotFound(w, r)
		}
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000000000000d/items/item-refresh-range-url", r.URL.Path)

		call := metadataCalls.Add(1)
		downloadPath := staleDownloadPath
		if call > 1 {
			downloadPath = freshDownloadPath
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-refresh-range-url",
			"eTag": "v1",
			"name": "range.txt",
			"size": %d,
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, len(fullContent), dlSrv.URL+downloadPath)
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.DownloadRange(t.Context(), driveid.New("d"), "item-refresh-range-url", &buf, offset, DownloadVersion{ETag: "v1"})
	require.NoError(t, err)

	assert.Equal(t, fullContent[offset:], buf.String())
	assert.Equal(t, int64(len(fullContent))-offset, n)
	assert.Equal(t, int32(2), metadataCalls.Load())
	assert.Equal(t, int32(1), staleDownloadCalls.Load())
	assert.Equal(t, int32(1), freshDownloadCalls.Load())
}

func TestDownloadRange_NoDownloadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponse(t, w, `{
			"id": "folder-r", "name": "Folder",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "root", "driveId": "d"},
			"folder": {"childCount": 0}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	var buf bytes.Buffer
	_, err := client.DownloadRange(t.Context(), driveid.New("d"), "folder-r", &buf, 100, DownloadVersion{ETag: "v1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoDownloadURL)
}

func TestDownload_RetriesOn503(t *testing.T) {
	var dlCalls atomic.Int32

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := dlCalls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, "retry success")
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeTestResponsef(t, w, `{
			"id": "item-retry", "name": "retry.txt",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-retry", &buf)
	require.NoError(t, err)
	assert.Equal(t, "retry success", buf.String())
	assert.Equal(t, int64(len("retry success")), n)
	assert.Equal(t, int32(3), dlCalls.Load())
}

// cancelOnWrite cancels a context on the first write it receives, modeling
// Ctrl-C arriving while a transfer is streaming.
type cancelOnWrite struct {
	cancel context.CancelFunc
}

func (w cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()

	return len(p), nil
}

// Validates: R-6.6.21, R-6.6.22
//
// A transfer interrupted by shutdown returns its error to the caller, which
// treats cancellation as shutdown. The graph client used to log the failure
// at ERROR as well, so a clean Ctrl-C printed an error line per active
// transfer, even under --quiet.
func TestTransfersCanceledMidFlightLogNothingAboveDebug(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Length", "1048576")
			w.WriteHeader(http.StatusOK)
			_, writeErr := w.Write(bytes.Repeat([]byte("x"), 1024))
			assert.NoError(t, writeErr)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		<-r.Context().Done()
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	client := newTestClient(t, srv.URL)
	client.logger = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	downloadCtx, cancelDownload := context.WithCancel(t.Context())
	defer cancelDownload()

	_, err := client.downloadFromURL(downloadCtx, &Item{DownloadURL: DownloadURL(srv.URL + "/content")}, cancelOnWrite{cancel: cancelDownload})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	uploadCtx, cancelUpload := context.WithCancel(t.Context())
	cancelUpload()

	_, err = client.SimpleUploadToItem(uploadCtx, driveid.New("d1"), "item-1", "", strings.NewReader("data"), 4)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	assert.Empty(t, logBuf.String(), "a canceled transfer must not log at Warn or Error")
}

func newRangeTestServers(t *testing.T, etag string, size int, serve http.HandlerFunc) (*httptest.Server, *httptest.Server) {
	t.Helper()

	dlSrv := httptest.NewServer(serve)
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{"id":"item-range","name":"range.bin","size":%d,"eTag":%q,
			"file":{"mimeType":"application/octet-stream"},"@microsoft.graph.downloadUrl":%q}`,
			size, etag, dlSrv.URL+"/dl")
	}))

	return dlSrv, graphSrv
}

// Validates: R-5.2.2, R-5.5.1
func TestDownloadRange_RejectsUnboundOrMisalignedAnswers(t *testing.T) {
	const content = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const offset = 10

	tests := []struct {
		name    string
		version DownloadVersion
		serve   http.HandlerFunc
	}{
		{
			name:    "server ignored the Range",
			version: DownloadVersion{ETag: "v1"},
			serve: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				writeDownloadTestBody(t, w, content)
			},
		},
		{
			name:    "Content-Range starts elsewhere",
			version: DownloadVersion{ETag: "v1"},
			serve: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(content)-1, len(content)))
				w.WriteHeader(http.StatusPartialContent)
				writeDownloadTestBody(t, w, content)
			},
		},
		{
			name:    "item changed since the partial started",
			version: DownloadVersion{ETag: "v0"},
			serve: func(w http.ResponseWriter, _ *http.Request) {
				assert.Fail(t, "a changed item must not be fetched for resume")
				w.WriteHeader(http.StatusPartialContent)
			},
		},
		{
			name:    "no version to bind to",
			version: DownloadVersion{},
			serve: func(w http.ResponseWriter, _ *http.Request) {
				assert.Fail(t, "an unbound partial must not be resumed")
				w.WriteHeader(http.StatusPartialContent)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dlSrv, graphSrv := newRangeTestServers(t, "v1", len(content), tt.serve)
			defer dlSrv.Close()
			defer graphSrv.Close()

			var buf bytes.Buffer
			_, err := newTestClient(t, graphSrv.URL).DownloadRange(
				t.Context(), driveid.New("d"), "item-range", &buf, offset, tt.version)

			// Appending any of these to the partial spliced two copies or
			// two versions of the file, silently for a hashless item.
			require.ErrorIs(t, err, ErrRangeResumeRejected)
			assert.Zero(t, buf.Len(), "nothing may be appended")
		})
	}
}

// Validates: R-5.5.1
func TestDownload_ErrorDocumentWithStatus200IsRejected(t *testing.T) {
	const errorBody = `{"error":{"code":"generalException"}}`

	tests := []struct {
		name        string
		mimeType    string
		served      string
		size        int
		quickXor    string
		wantRefused bool
	}{
		// Hashless, and the length is not the item's: an error document.
		{name: "html page for a binary file", mimeType: "application/octet-stream", served: "text/html; charset=utf-8", size: 4096, wantRefused: true},
		{name: "json error for a photo", mimeType: "image/jpeg", served: "application/json", size: 4096, wantRefused: true},
		{name: "a real html file", mimeType: "text/html", served: "text/html", size: len(errorBody)},
		{name: "a real json file", mimeType: "application/json", served: "application/json; charset=utf-8", size: len(errorBody)},
		// A .json file OneDrive types as octet-stream but serves as JSON:
		// the hash decides, never the type.
		{
			name: "hashed json file typed octet-stream", mimeType: "application/octet-stream", served: "application/json",
			size: len(errorBody), quickXor: "any-hash",
		},
		// Hashless, served as JSON, length matches the item: the file.
		{name: "hashless json file of the right size", mimeType: "application/octet-stream", served: "application/json", size: len(errorBody)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.served)
				writeDownloadTestBody(t, w, errorBody)
			}))
			defer dlSrv.Close()

			hashes := ""
			if tt.quickXor != "" {
				hashes = fmt.Sprintf(`,"hashes":{"quickXorHash":%q}`, tt.quickXor)
			}
			graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeTestResponsef(t, w, `{"id":"i","name":"f","size":%d,"file":{"mimeType":%q%s},
					"@microsoft.graph.downloadUrl":%q}`, tt.size, tt.mimeType, hashes, dlSrv.URL+"/dl")
			}))
			defer graphSrv.Close()

			var buf bytes.Buffer
			_, err := newTestClient(t, graphSrv.URL).Download(t.Context(), driveid.New("d"), "i", &buf)
			if tt.wantRefused {
				require.ErrorIs(t, err, ErrUnexpectedDownloadBody)
				assert.Zero(t, buf.Len())
			} else {
				require.NoError(t, err)
				assert.Equal(t, len(errorBody), buf.Len())
			}
		})
	}
}

// Validates: R-6.8.22
//
// @microsoft.graph.downloadUrl is server-supplied. The client used to dispatch
// it as given, so a spoofed or hostile metadata answer could point the
// unauthenticated download request anywhere. The default validator holds it
// to HTTPS on the Microsoft content hosts, the refusal happens before any
// request is sent, and its text never echoes the URL, whose query carries
// the credential.
func TestDownload_RejectsUntrustedDownloadURL(t *testing.T) {
	var contentRequests atomic.Int32
	contentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer contentSrv.Close()

	contentHost := strings.TrimPrefix(contentSrv.URL, "http://")

	tests := []struct {
		name        string
		downloadURL string
		wantReason  string
	}{
		{name: "http scheme", downloadURL: "http://" + contentHost + "/dl?tempauth=SECRETTOKEN", wantReason: "must use https"},
		{name: "IP literal", downloadURL: "https://" + contentHost + "/dl?tempauth=SECRETTOKEN", wantReason: "IP literal"},
		{name: "non-Microsoft host", downloadURL: "https://evil.example.com/dl?tempauth=SECRETTOKEN", wantReason: "is not allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeTestResponsef(t, w, `{
					"id": "item-1", "name": "a.txt", "size": 2, "eTag": "v1",
					"createdDateTime": "2024-01-01T00:00:00Z",
					"lastModifiedDateTime": "2024-01-01T00:00:00Z",
					"parentReference": {"id": "p", "driveId": "d"},
					"file": {"mimeType": "text/plain"},
					"@microsoft.graph.downloadUrl": %q
				}`, tt.downloadURL)
			}))
			defer graphSrv.Close()

			// The production validator, not the loopback-tolerant one a
			// loopback base URL installs: this test is about the default.
			client := newTestClient(t, graphSrv.URL)
			client.validators.downloadURL = validateDownloadURL

			var buf bytes.Buffer
			_, err := client.Download(t.Context(), driveid.New("d"), "item-1", &buf)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantReason)
			assert.NotContains(t, err.Error(), "SECRETTOKEN")
			assert.NotContains(t, err.Error(), tt.downloadURL)

			_, err = client.DownloadRange(t.Context(), driveid.New("d"), "item-1", &buf, 1, DownloadVersion{ETag: "v1"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantReason)
			assert.NotContains(t, err.Error(), "SECRETTOKEN")
			assert.NotContains(t, err.Error(), tt.downloadURL)

			assert.Zero(t, contentRequests.Load(), "a refused download URL must never be requested")
			assert.Empty(t, buf.Bytes())
		})
	}
}
