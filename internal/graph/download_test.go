package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestDownload_Success(t *testing.T) {
	fileContent := "Hello, this is the file content for download testing."

	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fileContent))
	}))
	defer downloadSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/drives/000000000000000d/items/item-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
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
	n, err := client.Download(context.Background(), driveid.New("d"), "item-1", &buf)
	require.NoError(t, err)
	assert.Equal(t, fileContent, buf.String())
	assert.Equal(t, int64(len(fileContent)), n)
}

func TestDownload_EmptyDownloadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
			"id": "folder-1",
			"name": "Documents",
			"createdDateTime": "2024-01-01T00:00:00Z",
			"lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "root", "driveId": "d"},
			"folder": {"childCount": 5}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	var buf bytes.Buffer
	_, err := client.Download(context.Background(), driveid.New("d"), "folder-1", &buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoDownloadURL)
}

func TestDownload_ItemNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("request-id", "req-dl-404")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"itemNotFound"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	var buf bytes.Buffer
	_, err := client.Download(context.Background(), driveid.New("d"), "nonexistent", &buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDownload_VerifyBytesWritten(t *testing.T) {
	content := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
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
	n, err := client.Download(context.Background(), driveid.New("d"), "item-2", &buf)
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
		fmt.Fprintf(w, `{
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
	_, err := client.Download(context.Background(), driveid.New("d"), "item-3", &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
}

func TestDownload_NetworkError(t *testing.T) {
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{
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
	_, err := client.Download(context.Background(), driveid.New("d"), "item-net", &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "download request failed")
}

func TestDownloadFromURL_WriterError(t *testing.T) {
	// Verify that downloadFromURL returns an error when the writer fails mid-stream.
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some data that will fail to write"))
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
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
	_, err := client.Download(context.Background(), driveid.New("d"), "item-ew", errorWriter{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streaming download content")
}

func TestDownload_NoAuthOnPreAuthURL(t *testing.T) {
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
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
	_, err := client.Download(context.Background(), driveid.New("d"), "item-4", &buf)
	require.NoError(t, err)
}
