package graph

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
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
	client.transferStallTimeout = testStallTimeout

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
	client.transferStallTimeout = testStallTimeout

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
	client.transferStallTimeout = stallTimeout

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

// newTestStallWatchdog returns a watchdog whose timer never fires by itself,
// so a test reads whether it is armed without racing the clock.
func newTestStallWatchdog(t *testing.T) *stallWatchdog {
	t.Helper()

	client := &Client{transferStallTimeout: time.Hour}
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
		reader := watchdog.reader(body)

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
	client := &Client{transferStallTimeout: time.Hour}
	_, watchdog := client.watchTransfer(context.Background())

	return watchdog
}

// Validates: R-6.8.20
func TestStallWatchdog_AnAbandonedAttemptsReadCannotDisarmTheNextAttempt(t *testing.T) {
	watchdog := newTestStallWatchdog(t)

	// The first attempt's body is inside a read when its request is abandoned.
	abandoned := &gatedReader{entered: make(chan struct{}), release: make(chan struct{}), err: io.EOF}
	first := watchdog.reader(abandoned)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := first.Read(make([]byte, 1))
		assert.ErrorIs(t, err, io.EOF)
	}()
	<-abandoned.entered

	// The retry wait starts and ends, and the next attempt's body moves.
	watchdog.waitStarts()
	second := watchdog.reader(bytes.NewReader([]byte("uu")))
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
	reader := watchdog.reader(bytes.NewReader([]byte("uu")))

	// The transport can still be reading the request body after the call
	// that owns the transfer has returned.
	watchdog.stop()
	_, err := reader.Read(make([]byte, 1))
	require.NoError(t, err)

	assert.False(t, watchdog.timer.Stop(), "a read after the transfer ended left a timer running")
}
