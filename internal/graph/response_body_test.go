package graph

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// A connection that drops mid-download is marked as a response read failure,
// so the CLI can call it a dropped connection; a failure writing the local
// file during the same io.Copy is not.
func TestDownloadRange_DroppedConnectionIsAResponseReadError(t *testing.T) {
	var hangUp atomic.Bool
	hangUp.Store(true)
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 1-99/100")
		if !hangUp.Load() {
			w.WriteHeader(http.StatusPartialContent)
			writeDownloadTestBody(t, w, "complete")
			return
		}
		// Promise more than is sent, then hang up.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusPartialContent)
		writeDownloadTestBody(t, w, "partial")
		hj, ok := w.(http.Hijacker)
		if !assert.True(t, ok) {
			return
		}
		conn, _, err := hj.Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, conn.Close())
		}
	}))
	defer dlSrv.Close()

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{
			"id": "item-range", "name": "range.txt", "size": 100, "eTag": "v1",
			"parentReference": {"id": "p", "driveId": "d"},
			"file": {"mimeType": "text/plain"},
			"@microsoft.graph.downloadUrl": %q
		}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newTestClient(t, graphSrv.URL)
	_, err := client.DownloadRange(t.Context(), driveid.New("d"), "item-range", &bytes.Buffer{}, 1, DownloadVersion{ETag: "v1"})
	require.Error(t, err)

	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// The local side failing is not a response read failure.
	hangUp.Store(false)
	_, err = client.DownloadRange(t.Context(), driveid.New("d"), "item-range", failingWriter{}, 1, DownloadVersion{ETag: "v1"})
	require.ErrorIs(t, err, errDiskFull)
	assert.NotErrorAs(t, err, &readErr, "a local write failure must not read as a dropped connection: %v", err)
}

func TestMarkedResponseBody_PassesEOFThrough(t *testing.T) {
	t.Parallel()

	body := markedResponseBody{ReadCloser: io.NopCloser(bytes.NewReader([]byte("x")))}
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))

	failing := markedResponseBody{ReadCloser: io.NopCloser(iotestErrReader{err: fmt.Errorf("boom")})}
	_, err = failing.Read(make([]byte, 1))
	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr)
	assert.Equal(t, "graph: reading the response: boom", err.Error())
}

var errDiskFull = errors.New("no space left on device")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errDiskFull }

type iotestErrReader struct{ err error }

func (r iotestErrReader) Read([]byte) (int, error) { return 0, r.err }
