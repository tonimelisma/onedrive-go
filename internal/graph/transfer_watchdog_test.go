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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

const testStallTimeout = 150 * time.Millisecond

// Validates: R-6.8.20
func TestDownload_StalledBodyIsAbandoned(t *testing.T) {
	release := make(chan struct{})

	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		writeDownloadTestBody(t, w, string(bytes.Repeat([]byte("x"), 1024)))
		flusher, ok := w.(http.Flusher)
		if assert.True(t, ok) {
			flusher.Flush()
		}
		// Headers and 1 KiB delivered, then silence.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer dlSrv.Close()
	defer close(release) // before Close, which waits for the blocked handler

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{"id":"i","name":"a.bin","size":4096,"file":{"mimeType":"application/octet-stream"},
			"@microsoft.graph.downloadUrl":%q}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	client := newNoRetryTestClient(t, graphSrv.URL)
	client.stallTimeout = testStallTimeout

	var buf bytes.Buffer
	start := time.Now()
	_, err := client.Download(t.Context(), driveid.New("d"), "i", &buf)

	// Without the watchdog this blocked until the server gave up.
	require.ErrorIs(t, err, ErrTransferStalled)
	// A stall is retryable work; context.Canceled reads as a shutdown.
	require.NotErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1024, buf.Len(), "the bytes that did arrive were written")
}

// Validates: R-6.8.20
func TestUploadChunk_StalledBodyIsAbandoned(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Read 1 KiB of the fragment, then stop reading.
		buf := make([]byte, 1024)
		_, readErr := r.Body.Read(buf)
		assert.NoError(t, readErr)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release) // before Close, which waits for the blocked handler

	client := newNoRetryTestClient(t, srv.URL)
	client.stallTimeout = testStallTimeout

	chunk := bytes.Repeat([]byte("u"), ChunkedUploadChunkSize)
	start := time.Now()
	_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
		bytes.NewReader(chunk), 0, int64(len(chunk)), int64(len(chunk)))

	require.ErrorIs(t, err, ErrTransferStalled)
	require.NotErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// Validates: R-6.8.20
func TestDownload_HeaderPhaseStallIsATimeoutNotAShutdown(t *testing.T) {
	release := make(chan struct{})
	dlSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Accept the request, never answer it.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer dlSrv.Close()
	defer close(release)

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{"id":"i","name":"a.bin","size":4,"file":{},"@microsoft.graph.downloadUrl":%q}`, dlSrv.URL+"/dl")
	}))
	defer graphSrv.Close()

	// The wait for headers belongs to the transport's ResponseHeaderTimeout,
	// as in production; the stall watchdog only watches body I/O.
	httpClient := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: testStallTimeout}}
	client := MustNewClient(normalizeTestBaseURL(graphSrv.URL), httpClient, staticToken("t"), slog.Default(), "test-agent")

	_, err := client.Download(t.Context(), driveid.New("d"), "i", &bytes.Buffer{})
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled, "a header timeout is not a shutdown")
}

// Validates: R-6.8.20
func TestUploadChunk_RetryAfterLongerThanStallTimeoutIsNotAStall(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Throttled before the fragment is read: the transport stops
			// writing the body mid-way and waits out Retry-After.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeTestResponse(t, w, `{"id":"done","name":"a.bin","file":{}}`)
	}))
	defer srv.Close()

	// The retried fragment is a whole upload chunk sent over loopback, so a
	// stall timeout as short as testStallTimeout fired on a loaded 2-core
	// machine whenever the copy was descheduled for 150 ms (4 runs in 10). The
	// property needs only a wait longer than the stall timeout, not a short one.
	const stallTimeout = time.Second
	sleepLongerThanStall := func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(3 * stallTimeout):
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	httpClient := &http.Client{Transport: &retry.RetryTransport{
		Inner: http.DefaultTransport, Policy: testRetryPolicy(), Logger: slog.Default(), Sleep: sleepLongerThanStall,
	}}
	client := MustNewClient(normalizeTestBaseURL(srv.URL), httpClient, staticToken("t"), slog.Default(), "test-agent")
	client.stallTimeout = stallTimeout

	chunk := bytes.Repeat([]byte("u"), ChunkedUploadChunkSize)
	item, complete, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
		bytes.NewReader(chunk), 0, int64(len(chunk)), int64(len(chunk)))

	// The wait is the transport obeying the server, not a stalled body.
	require.NoError(t, err)
	assert.True(t, complete)
	assert.Equal(t, "done", item.ID)
}

// gatedReader holds its one Read open until release is closed, then answers
// err: a body read that is still running when its attempt is abandoned.
type gatedReader struct {
	entered chan struct{}
	release chan struct{}
	err     error
}

func (g *gatedReader) Read([]byte) (int, error) {
	close(g.entered)
	<-g.release

	return 0, g.err
}

// armedLocalBody is a local body's arming without its local I/O goroutine:
// the tests of when the timer runs read several attempts' bodies at once,
// which the goroutine would run one after the other.
func armedLocalBody(watchdog *stallWatchdog, r io.Reader) io.Reader {
	return watchdog.reader(r, true)
}

// newTestStallWatchdog returns a watchdog whose timer never fires by itself,
// so a test reads whether it is armed without racing the clock.
func newTestStallWatchdog(t *testing.T) *stallWatchdog {
	t.Helper()

	client := &Client{stallTimeout: time.Hour}
	_, watchdog := client.watchTransfer(t.Context())
	t.Cleanup(watchdog.stop)

	return watchdog
}

// Validates: R-6.8.20
func TestStallWatchdog_AWaitThatStartsDuringAReadLeavesTheTimerStopped(t *testing.T) {
	// The transport reads an abandoned attempt's body on its own goroutine
	// while the retry wait starts on the caller's. Whichever comes first, once
	// both have returned nothing is expected to move, so the timer must be
	// stopped. A read that found its attempt current and armed the timer after
	// the wait had stopped it left the timer running through the wait: a
	// Retry-After longer than the stall timeout failed the upload as stalled.
	const rounds = 20000

	one := make([]byte, 1)
	body := bytes.NewReader(bytes.Repeat([]byte("u"), rounds))
	for round := range rounds {
		watchdog := newRoundStallWatchdog()
		reader := armedLocalBody(watchdog, body)

		start := make(chan struct{})
		var both sync.WaitGroup
		both.Go(func() {
			<-start
			_, err := reader.Read(one)
			assert.NoError(t, err)
		})
		both.Go(func() {
			<-start
			watchdog.waitStarts()
		})
		close(start)
		both.Wait()

		armed := watchdog.timer.Stop()
		watchdog.stop()
		require.Falsef(t, armed, "round %d: the timer runs although the retry wait has started", round)
	}
}

// newRoundStallWatchdog is newTestStallWatchdog for a test that makes
// thousands: the caller stops each one itself instead of queuing a cleanup.
func newRoundStallWatchdog() *stallWatchdog {
	client := &Client{stallTimeout: time.Hour}
	_, watchdog := client.watchTransfer(context.Background())

	return watchdog
}

// Validates: R-6.8.20
func TestStallWatchdog_AnAbandonedAttemptsReadCannotDisarmTheNextAttempt(t *testing.T) {
	watchdog := newTestStallWatchdog(t)

	// The first attempt's body is inside a read when its request is abandoned.
	abandoned := &gatedReader{entered: make(chan struct{}), release: make(chan struct{}), err: io.EOF}
	first := armedLocalBody(watchdog, abandoned)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := first.Read(make([]byte, 1))
		assert.ErrorIs(t, err, io.EOF)
	}()
	<-abandoned.entered

	// The retry wait starts and ends, and the next attempt's body moves.
	watchdog.waitStarts()
	second := armedLocalBody(watchdog, bytes.NewReader([]byte("uu")))
	_, err := second.Read(make([]byte, 1))
	require.NoError(t, err)

	// Only now does the abandoned read end.
	close(abandoned.release)
	<-firstDone

	// The next attempt is mid-body. Were the watchdog disarmed here, a
	// connection that went quiet now would hold its worker for good.
	assert.True(t, watchdog.timer.Stop(),
		"the abandoned attempt's read disarmed the watchdog of the attempt that replaced it")
}

// Validates: R-6.8.20
func TestStallWatchdog_NothingArmsItOnceTheTransferHasEnded(t *testing.T) {
	watchdog := newTestStallWatchdog(t)
	reader := armedLocalBody(watchdog, bytes.NewReader([]byte("uu")))

	// The transport can still be reading the request body after the call
	// that owns the transfer has returned.
	watchdog.stop()
	_, err := reader.Read(make([]byte, 1))
	require.NoError(t, err)

	assert.False(t, watchdog.timer.Stop(), "a read after the transfer ended left a timer running")
}

// A download's response body comes back bounded by the client's stall window
// like every response body. Read through the watchdog, it starts no timer of
// its own: the two windows are the same, and a download failed as a stalled
// response instead of a stalled transfer would lose its partial.
//
// Validates: R-6.8.20
func TestStallWatchdog_ABodyItReadsStartsNoTimerOfItsOwn(t *testing.T) {
	t.Parallel()

	body := newResponseBody(io.NopCloser(bytes.NewReader([]byte("content"))), time.Hour, slog.New(slog.DiscardHandler))
	data, err := io.ReadAll(newTestStallWatchdog(t).networkBody(body))
	require.NoError(t, err)
	assert.Equal(t, "content", string(data))

	body.mu.Lock()
	reads := body.reads
	body.mu.Unlock()
	assert.Zero(t, reads, "the body's own stall timer ran beside the watchdog's")
}

// Validates: R-6.3.10
//
// The stall timer's callback runs on a goroutine of its own. A panic in it
// fails the one transfer it watches: the boundary logs it once with its stack
// and cancels the transfer with the panic's error, and explain returns that
// error, the transfer's own error kept as text, so the failure is neither a
// shutdown nor a stall to retry.
func TestStallWatchdog_ACallbackPanicFailsTheTransferWithIt(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	ctx, cancel := context.WithCancelCause(t.Context())
	watchdog := &stallWatchdog{timeout: testStallTimeout, logger: logger, cancel: func(cause error) {
		if errors.Is(cause, ErrTransferStalled) {
			panic("stall cancel exploded")
		}
		cancel(cause)
	}}

	require.NotPanics(t, watchdog.stalled, "the callback's boundary keeps the panic off the process")

	require.ErrorIs(t, context.Cause(ctx), panicguard.ErrPanic, "the transfer is canceled with the panic's error")
	err := watchdog.explain(ctx, fmt.Errorf("graph: streaming download content: %w", context.Canceled))
	require.ErrorIs(t, err, panicguard.ErrPanic)
	require.NotErrorIs(t, err, context.Canceled, "a panic is not a shutdown")
	require.NotErrorIs(t, err, ErrTransferStalled, "nor a stall")
	assert.Contains(t, err.Error(), "streaming download content", "the transfer's own error is kept as text")

	logged := capture.AtLeast(slog.LevelError)
	require.Len(t, logged, 1, "logged once")
	assert.Equal(t, "stall cancel exploded", logged[0].Attrs["panic"])
	assert.Contains(t, logged[0].Attrs["stack"], "TestStallWatchdog_ACallbackPanicFailsTheTransferWithIt")
}

// blockingChunk is an upload's local file whose reads past its first
// readable bytes block until release is closed: a local disk that hangs.
type blockingChunk struct {
	readable int64
	release  chan struct{}
}

func (c *blockingChunk) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.readable {
		<-c.release
		return 0, io.ErrUnexpectedEOF
	}
	n := min(int64(len(p)), c.readable-off)
	for i := range n {
		p[i] = 'u'
	}

	return int(n), nil
}

// Validates: R-6.8.20
//
// An upload whose read of the local file blocks past the stall timeout is
// abandoned as stalled, and the error says the local side stalled: a hung
// local disk is not the service's silence.
func TestUploadChunk_AStallInTheLocalReadSaysTheLocalSideStalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server reads everything the client sends; the client never
		// finishes the body.
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // the body is cut off, which is the point
	}))
	defer srv.Close()

	client := newNoRetryTestClient(t, srv.URL)
	client.stallTimeout = testStallTimeout
	client.abandonedLocal = newAbandonedLocalCalls(maxAbandonedLocalCalls)

	// The local read hangs until the test ends; the transfer abandons it
	// (TestUploadChunk_AStuckLocalReadIsAbandonedWhileItIsStillBlocked).
	chunk := &blockingChunk{readable: 1024, release: make(chan struct{})}
	defer close(chunk.release)
	_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
		chunk, 0, ChunkedUploadChunkSize, ChunkedUploadChunkSize)

	require.ErrorIs(t, err, ErrTransferStalled)
	require.ErrorIs(t, err, ErrLocalSideStalled)
	require.NotErrorIs(t, err, context.Canceled)
}

// Validates: R-6.8.20
//
// A fragment whose network side goes quiet (the server stops reading) is a
// stall of the service, not of the local side.
func TestUploadChunk_AStallOnTheNetworkSideIsNotALocalStall(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		_, readErr := r.Body.Read(buf)
		assert.NoError(t, readErr)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release) // before Close, which waits for the blocked handler

	client := newNoRetryTestClient(t, srv.URL)
	client.stallTimeout = testStallTimeout

	chunk := bytes.Repeat([]byte("u"), ChunkedUploadChunkSize)
	_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
		bytes.NewReader(chunk), 0, int64(len(chunk)), int64(len(chunk)))

	require.ErrorIs(t, err, ErrTransferStalled)
	assert.NotErrorIs(t, err, ErrLocalSideStalled)
}

// blockingWriter is a download's local file whose writes block until
// release is closed: a local disk that hangs between two reads of the body.
type blockingWriter struct {
	release <-chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release

	return len(p), nil
}

// newQuietDownload serves an item whose download body delivers 1 KiB and then
// goes quiet, and returns the Graph server's URL.
func newQuietDownload(t *testing.T) string {
	t.Helper()

	release := make(chan struct{})
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !assert.True(t, ok) {
			return
		}
		writeDownloadTestBody(t, w, strings.Repeat("x", 1024))
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{"id":"i","name":"a.bin","size":4096,"file":{"mimeType":"application/octet-stream"},
			"@microsoft.graph.downloadUrl":%q}`, dlSrv.URL+"/dl")
	}))
	t.Cleanup(func() {
		graphSrv.Close()
		close(release) // before Close, which waits for the blocked handler
		dlSrv.Close()
	})

	return graphSrv.URL
}

// Validates: R-6.8.20
//
// A download whose write of the local file blocks between two reads of the
// body, past the stall timeout, is abandoned as stalled, and the error says
// the local side stalled.
func TestDownload_AStallInTheLocalWriteSaysTheLocalSideStalled(t *testing.T) {
	client := newNoRetryTestClient(t, newQuietDownload(t))
	client.stallTimeout = testStallTimeout

	// The local write hangs for longer than the stall timeout, then returns:
	// a hung write holds the copy whatever the watchdog does.
	release := make(chan struct{})
	unblock := time.AfterFunc(4*testStallTimeout, func() { close(release) })
	defer unblock.Stop()
	_, err := client.Download(t.Context(), driveid.New("d"), "i", &blockingWriter{release: release})

	require.ErrorIs(t, err, ErrTransferStalled)
	require.ErrorIs(t, err, ErrLocalSideStalled)
	require.NotErrorIs(t, err, context.Canceled)
}

// Validates: R-6.8.20
//
// A download whose body goes quiet while the client waits for it is a stall
// of the service, not of the local side.
func TestDownload_AStallOnTheNetworkSideIsNotALocalStall(t *testing.T) {
	client := newNoRetryTestClient(t, newQuietDownload(t))
	client.stallTimeout = testStallTimeout

	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "i", &buf)

	require.ErrorIs(t, err, ErrTransferStalled)
	assert.NotErrorIs(t, err, ErrLocalSideStalled)
}
