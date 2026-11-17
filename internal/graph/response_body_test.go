package graph

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
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

func TestResponseBody_PassesEOFThrough(t *testing.T) {
	t.Parallel()

	body := newResponseBody(io.NopCloser(bytes.NewReader([]byte("x"))), time.Hour, slog.New(slog.DiscardHandler))
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "x", string(data))

	failing := newResponseBody(io.NopCloser(iotestErrReader{err: errors.New("boom")}), time.Hour, slog.New(slog.DiscardHandler))
	_, err = failing.Read(make([]byte, 1))
	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr)
	assert.Equal(t, "graph: reading the response: boom", err.Error())
}

// stalledBodySafetyNet ends a test whose request the stall window failed to
// end; the tests assert on the error, which names what ended the request.
const stalledBodySafetyNet = 10 * time.Second

// newStalledBodyServer answers every request with status and its headers,
// then sends nothing until the client gives up on the body.
func newStalledBodyServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	return startStalledBodyServer(t, status, false)
}

// newStalledHTTP2BodyServer is newStalledBodyServer over HTTP/2 and TLS; its
// handler fails a request of any other protocol. srv.Client() trusts it.
func newStalledHTTP2BodyServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	return startStalledBodyServer(t, status, true)
}

func startStalledBodyServer(t *testing.T, status int, overHTTP2 bool) *httptest.Server {
	t.Helper()

	wantProtoMajor := 1
	if overHTTP2 {
		wantProtoMajor = 2
	}
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wantProtoMajor, r.ProtoMajor, "the request's HTTP version")
		stallAfterHeaders(t, w, r, status, release)
	}))
	if overHTTP2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	// Cleanups run last-registered first: the handler is released before
	// Close waits for it.
	t.Cleanup(func() { close(release) })

	return srv
}

// stallAfterHeaders answers with status and its headers, then sends nothing
// until release closes or the client gives up on the request.
func stallAfterHeaders(t *testing.T, w http.ResponseWriter, r *http.Request, status int, release <-chan struct{}) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	flusher, ok := w.(http.Flusher)
	if assert.True(t, ok) {
		flusher.Flush()
	}
	select {
	case <-release:
	case <-r.Context().Done():
	}
}

// stalledBodyClient is a graph client without retries whose stall window is
// testStallTimeout, and a context that ends the test should the window not.
func stalledBodyClient(t *testing.T, url string) (*Client, context.Context) {
	t.Helper()

	client := newNoRetryTestClient(t, url)
	client.stallTimeout = testStallTimeout
	ctx, cancel := context.WithTimeout(t.Context(), stalledBodySafetyNet)
	t.Cleanup(cancel)

	return client, ctx
}

// A metadata answer whose headers arrive and whose body then brings nothing
// fails once the stall window passes, with a retryable error of its own, not
// a cancellation, and as a failure of the answer that had begun.
//
// Validates: R-6.8.20
func TestGetItem_AStalledBodyFailsWithinTheStallWindow(t *testing.T) {
	t.Parallel()

	client, ctx := stalledBodyClient(t, newStalledBodyServer(t, http.StatusOK).URL)

	_, err := client.GetItem(ctx, driveid.New("d"), "stalled-item")

	require.ErrorIs(t, err, ErrResponseStalled)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the window ended it, not the context")
	require.NotErrorIs(t, err, context.Canceled)
	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr, "the answer had begun")
}

// The same bound ends a body read over HTTP/2, the protocol OneDrive answers
// in, whose transport ends a pending read on Close by code of its own.
//
// Validates: R-6.8.20
func TestGetItem_AStalledBodyFailsWithinTheStallWindowOverHTTP2(t *testing.T) {
	t.Parallel()

	srv := newStalledHTTP2BodyServer(t, http.StatusOK)
	client := MustNewClient(srv.URL, srv.Client(), staticToken("t"), slog.New(slog.DiscardHandler), "test-agent")
	client.stallTimeout = testStallTimeout
	ctx, cancel := context.WithTimeout(t.Context(), stalledBodySafetyNet)
	defer cancel()

	_, err := client.GetItem(ctx, driveid.New("d"), "stalled-item")

	require.ErrorIs(t, err, ErrResponseStalled)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the window ended it, not the context")
	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr, "the answer had begun")
}

// An answer the retrying transport retries is never read below the client: a
// 503 whose body stops after its headers is sent again at once, and the last
// one comes back as its answer within the stall window.
//
// Validates: R-6.8.20
func TestGetItem_ARetriedAnswerWhoseBodyStallsIsNotWaitedOn(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, newStalledBodyServer(t, http.StatusServiceUnavailable).URL)
	client.stallTimeout = testStallTimeout
	ctx, cancel := context.WithTimeout(t.Context(), stalledBodySafetyNet)
	defer cancel()

	start := time.Now()
	_, err := client.GetItem(ctx, driveid.New("d"), "stalled-retried-item")

	require.Less(t, time.Since(start), stalledBodySafetyNet/2, "a retried answer's body held the request")
	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, http.StatusServiceUnavailable, graphErr.StatusCode)
}

// A redirect whose body stops after its headers is followed at once: net/http
// reads up to 2 KB of a redirect's body before it follows it, where no bound
// of the client reaches, so the client closes that body unread first.
//
// Validates: R-6.8.20
func TestGetItem_ARedirectWhoseBodyStallsIsFollowed(t *testing.T) {
	t.Parallel()

	const movedPath = "/moved"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != movedPath {
			w.Header().Set("Location", movedPath)
			stallAfterHeaders(t, w, r, http.StatusFound, release)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{"id":"moved-item","name":"moved.txt","size":1,"file":{}}`)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client, ctx := stalledBodyClient(t, srv.URL)

	got, err := client.GetItem(ctx, driveid.New("d"), "redirecting-item")

	require.NoError(t, err)
	assert.Equal(t, "moved-item", got.ID)
}

// countedBody is an empty response body that counts the calls it receives.
type countedBody struct{ reads, closes atomic.Int32 }

func (b *countedBody) Read([]byte) (int, error) {
	b.reads.Add(1)

	return 0, io.EOF
}

func (b *countedBody) Close() error {
	b.closes.Add(1)

	return nil
}

// redirectTarget is where redirectingTransport sends every other request.
const redirectTarget = "/moved"

// redirectingTransport answers a request for redirectTarget with an item and
// every other request with a redirect there whose body is redirectBody.
type redirectingTransport struct{ redirectBody *countedBody }

func (rt redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != redirectTarget {
		return &http.Response{
			StatusCode: http.StatusFound, Header: http.Header{"Location": {redirectTarget}},
			ContentLength: -1, Body: rt.redirectBody, Request: req,
		}, nil
	}

	item := `{"id":"moved-item","name":"moved.txt","size":1,"file":{}}`

	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		ContentLength: int64(len(item)), Body: io.NopCloser(bytes.NewReader([]byte(item))), Request: req,
	}, nil
}

// A redirect's body is closed once and never read: what net/http reads of it
// and closes after the client's close is an empty body. Over HTTP/2 a second
// close of the same body would return its buffered bytes to the connection's
// receive window again.
//
// Validates: R-6.8.20
func TestGetItem_TheBodyOfARedirectIsClosedOnceAndNeverRead(t *testing.T) {
	t.Parallel()

	redirectBody := &countedBody{}
	transport := &http.Client{Transport: redirectingTransport{redirectBody: redirectBody}}
	client := MustNewClient("http://localhost", transport, staticToken("t"), slog.New(slog.DiscardHandler), "test-agent")

	got, err := client.GetItem(t.Context(), driveid.New("d"), "redirecting-item")

	require.NoError(t, err)
	assert.Equal(t, "moved-item", got.ID)
	assert.Equal(t, int32(1), redirectBody.closes.Load(), "closes of the redirect's body")
	assert.Equal(t, int32(0), redirectBody.reads.Load(), "reads of the redirect's body")
}

// Closing redirects keeps net/http's limit for a client without a redirect
// policy of its own.
func TestGetItem_RedirectsStopAtTheLimitOfNetHTTP(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := newNoRetryTestClient(t, srv.URL).GetItem(t.Context(), driveid.New("d"), "looping-item")

	require.ErrorContains(t, err, "stopped after 10 redirects")
	assert.Equal(t, int32(maxRedirects), requests.Load())
}

// The bound is on silence, not on time: a body that brings one byte at a time,
// each well within the window, completes though the whole takes longer than
// the window.
//
// Validates: R-6.8.20
func TestGetItem_ABodyThatKeepsMovingIsNotFailed(t *testing.T) {
	t.Parallel()

	const (
		window = 400 * time.Millisecond
		gap    = window / 20
		item   = `{"id":"slow-item","name":"slow.txt","size":1,"file":{}}`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok) {
			return
		}
		for i := range len(item) {
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
			writeTestResponse(t, w, item[i:i+1])
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client, ctx := stalledBodyClient(t, srv.URL)
	client.stallTimeout = window

	start := time.Now()
	got, err := client.GetItem(ctx, driveid.New("d"), "slow-item")

	require.NoError(t, err)
	assert.Equal(t, "slow-item", got.ID)
	assert.Greater(t, time.Since(start), window, "the body took longer than the window as a whole")
}

// An error answer whose body stalls is returned as that answer within the
// window: its status says what OneDrive answered, and the unread body leaves
// the placeholder message.
//
// Validates: R-6.8.20
func TestGetItem_AStalledErrorBodyReturnsItsAnswer(t *testing.T) {
	t.Parallel()

	client, ctx := stalledBodyClient(t, newStalledBodyServer(t, http.StatusServiceUnavailable).URL)
	logBuf := captureDebugLog(client)

	start := time.Now()
	_, err := client.GetItem(ctx, driveid.New("d"), "stalled-error-item")

	// The answer's error does not carry its body's read failure: the record
	// of the unreadable body names what ended the read, and the time shows
	// it was not the safety net.
	assert.Less(t, time.Since(start), stalledBodySafetyNet/2, "the window ended the read, not the context")
	records, _ := loggedRecords(t, logBuf)
	unreadable := records[recordIndex(t, records, "failed to read response body")]
	assert.Contains(t, unreadable.Error, ErrResponseStalled.Error())
	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, http.StatusServiceUnavailable, graphErr.StatusCode)
	assert.Equal(t, unreadableErrorBody, graphErr.Message)
	require.ErrorIs(t, err, ErrServerError)
}

// A drain of an answer the caller does not read is bounded the same way.
//
// Validates: R-6.8.20
func TestDeleteItem_AStalledDrainFailsWithinTheStallWindow(t *testing.T) {
	t.Parallel()

	client, ctx := stalledBodyClient(t, newStalledBodyServer(t, http.StatusOK).URL)

	err := client.DeleteItem(ctx, driveid.New("d"), "stalled-drain-item")

	require.ErrorIs(t, err, ErrResponseStalled)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the window ended it, not the context")
}

// closePanickingBody brings nothing until it is closed, and its Close panics
// once it has ended the pending read.
type closePanickingBody struct{ closed chan struct{} }

func (b closePanickingBody) Read([]byte) (int, error) {
	<-b.closed

	return 0, io.ErrClosedPipe
}

func (b closePanickingBody) Close() error {
	close(b.closed)
	panic("closing the body")
}

// A stall timer whose close of the body panics only logs the panic: the read
// the close ended reports the stall the timer fired for.
//
// Validates: R-6.8.20
func TestResponseBody_AStallWhoseClosePanicsReportsTheStall(t *testing.T) {
	t.Parallel()

	body := newResponseBody(closePanickingBody{closed: make(chan struct{})}, time.Millisecond, slog.New(slog.DiscardHandler))
	_, err := body.Read(make([]byte, 1))

	require.ErrorIs(t, err, ErrResponseStalled)
	assert.NotErrorIs(t, err, panicguard.ErrPanic)
}

var errDiskFull = errors.New("no space left on device")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errDiskFull }

type iotestErrReader struct{ err error }

func (r iotestErrReader) Read([]byte) (int, error) { return 0, r.err }
