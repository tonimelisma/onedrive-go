package graph

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// namedBlockingChunk is a blockingChunk that names its file, as an
// *os.File does.
type namedBlockingChunk struct {
	blockingChunk
	name string
}

func (c *namedBlockingChunk) Name() string { return c.name }

// signalingHandler is a log handler that closes signal once a record with
// message msg is logged: a test's view of the moment a transfer decided.
type signalingHandler struct {
	msg    string
	signal chan struct{}
	once   *sync.Once
}

func newSignalingLogger(msg string) (*slog.Logger, <-chan struct{}) {
	signal := make(chan struct{})

	return slog.New(signalingHandler{msg: msg, signal: signal, once: &sync.Once{}}), signal
}

func (h signalingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler fixes the by-value Record parameter.
func (h signalingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == h.msg {
		h.once.Do(func() { close(h.signal) })
	}

	return nil
}

func (h signalingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h signalingHandler) WithGroup(string) slog.Handler { return h }

// newTestAbandonedLocalCalls is a record of abandoned calls of the
// production bound whose released channel receives once per goroutine that
// leaves it.
func newTestAbandonedLocalCalls(limit int) (*abandonedLocalCalls, <-chan struct{}) {
	record := newAbandonedLocalCalls(limit)
	released := make(chan struct{}, limit)
	record.released = func() { released <- struct{}{} }

	return record, released
}

func abandonedPaths(record *abandonedLocalCalls) []string {
	record.mu.Lock()
	defer record.mu.Unlock()

	paths := make([]string, 0, len(record.calls))
	for _, call := range record.calls {
		paths = append(paths, call.path)
	}

	return paths
}

// newDrainingUploadServer reads everything a fragment sends; the client
// never finishes the body.
func newDrainingUploadServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // the body is cut off, which is the point
	}))
	t.Cleanup(srv.Close)

	return srv
}

// Validates: R-6.8.20
//
// An upload whose read of the local file hangs past the stall timeout
// returns as a local-side stall while the read is still blocked: the record
// of abandoned calls holds the read, and the worker is free. Once the read
// returns, its goroutine leaves the record.
func TestUploadChunk_AStuckLocalReadIsAbandonedWhileItIsStillBlocked(t *testing.T) {
	srv := newDrainingUploadServer(t)

	client := newNoRetryTestClient(t, srv.URL)
	client.stallTimeout = testStallTimeout
	record, released := newTestAbandonedLocalCalls(maxAbandonedLocalCalls)
	client.abandonedLocal = record

	chunk := &namedBlockingChunk{
		blockingChunk: blockingChunk{readable: 1024, release: make(chan struct{})},
		name:          "/mnt/hung/a.bin",
	}
	_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
		io.NewSectionReader(chunk, 0, ChunkedUploadChunkSize), 0, ChunkedUploadChunkSize, ChunkedUploadChunkSize)

	// The read is still blocked: chunk.release is not closed yet.
	require.ErrorIs(t, err, ErrTransferStalled)
	require.ErrorIs(t, err, ErrLocalSideStalled)
	require.NotErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrLocalCallAbandoned, "the caller is told the call was left behind")
	assert.Equal(t, []string{"/mnt/hung/a.bin"}, abandonedPaths(record),
		"the stuck read is the record's, named after its file")

	close(chunk.release)
	<-released
	assert.Zero(t, record.count(), "a read that returned leaves the record")
}

// Validates: R-6.8.20
//
// With the record of abandoned calls full, a transfer whose local read
// hangs does not abandon it: it waits for the read, as it did before
// anything could be abandoned, and returns only once the read has.
func TestUploadChunk_AFullRecordOfAbandonedCallsMakesTheTransferWait(t *testing.T) {
	srv := newDrainingUploadServer(t)

	logger, waiting := newSignalingLogger(
		"a local file stopped moving and too many stuck calls are already left behind; waiting for it")
	client := MustNewClient(normalizeTestBaseURL(srv.URL), http.DefaultClient, staticToken("t"), logger, "test-agent")
	client.stallTimeout = testStallTimeout
	record, _ := newTestAbandonedLocalCalls(1)
	require.True(t, record.adopt(&localIO{}, abandonedLocalCall{path: "elsewhere", side: localSideRead}))
	client.abandonedLocal = record

	chunk := &blockingChunk{readable: 1024, release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, _, err := client.UploadChunk(t.Context(), &UploadSession{UploadURL: UploadURL(srv.URL + "/s")},
			chunk, 0, ChunkedUploadChunkSize, ChunkedUploadChunkSize)
		done <- err
	}()

	// The stall has fired and the transfer found no room to abandon the read.
	<-waiting
	select {
	case err := <-done:
		require.Failf(t, "the transfer returned while its read was still blocked", "error: %v", err)
	default:
	}

	close(chunk.release)
	err := <-done
	require.ErrorIs(t, err, ErrTransferStalled, "the read returned and the transfer failed as stalled")
	require.NotErrorIs(t, err, ErrLocalCallAbandoned, "a call waited for was not left behind")
	assert.Equal(t, []string{"elsewhere"}, abandonedPaths(record), "nothing more was abandoned")
}

// stuckPartial is a download's local file whose first write blocks until
// release is closed, and which records being abandoned.
type stuckPartial struct {
	release   chan struct{}
	abandoned chan struct{}
	returned  chan struct{}
	written   int
}

func newStuckPartial() *stuckPartial {
	return &stuckPartial{
		release:   make(chan struct{}),
		abandoned: make(chan struct{}),
		returned:  make(chan struct{}),
	}
}

func (p *stuckPartial) Write(b []byte) (int, error) {
	<-p.release
	p.written += len(b)

	return len(b), nil
}

func (p *stuckPartial) Name() string { return "/mnt/hung/.onedrive-go.a.bin.partial" }

func (p *stuckPartial) Abandon() { close(p.abandoned) }

func (p *stuckPartial) AbandonedWriteReturned() { close(p.returned) }

// Validates: R-6.8.20
//
// A download whose write of the local file hangs past the stall timeout
// returns as a local-side stall while the write is still blocked, and tells
// the file it was abandoned, so its owner disowns the partial. Once the
// write returns, the file is told so on the write's goroutine, which then
// leaves the record.
func TestDownload_AStuckLocalWriteIsAbandonedAndItsFileFinishedWhenItReturns(t *testing.T) {
	client := newNoRetryTestClient(t, newQuietDownload(t))
	client.stallTimeout = testStallTimeout
	record, released := newTestAbandonedLocalCalls(maxAbandonedLocalCalls)
	client.abandonedLocal = record

	partial := newStuckPartial()
	_, err := client.Download(t.Context(), driveid.New("d"), "i", partial)

	require.ErrorIs(t, err, ErrTransferStalled)
	require.ErrorIs(t, err, ErrLocalSideStalled)
	require.NotErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, ErrLocalCallAbandoned)
	select {
	case <-partial.abandoned:
	default:
		require.Fail(t, "the file was not told its write was abandoned")
	}
	select {
	case <-partial.returned:
		require.Fail(t, "the file was told the write returned while it was still blocked")
	default:
	}
	assert.Equal(t, []string{partial.Name()}, abandonedPaths(record))

	close(partial.release)
	<-partial.returned
	<-released
	assert.Zero(t, record.count())
	assert.Positive(t, partial.written, "the stuck write landed its bytes once it returned")
}

// Validates: R-6.8.20
//
// An abandoned read fills the I/O goroutine's own buffer, never the
// caller's: the transport hands the caller's buffer to the next attempt,
// which a late read must not write into.
func TestLocalIO_AnAbandonedReadNeverFillsTheCallersBuffer(t *testing.T) {
	record, released := newTestAbandonedLocalCalls(maxAbandonedLocalCalls)
	stalled := make(chan struct{})
	local := newLocalIO(stalled, record, slog.New(slog.DiscardHandler))
	t.Cleanup(local.end)

	reader := &fillingReader{entered: make(chan struct{}), release: make(chan struct{})}
	callers := make([]byte, 8)
	done := make(chan error, 1)
	go func() {
		_, err := local.read(reader, callers, "a.bin")
		done <- err
	}()

	<-reader.entered
	close(stalled)
	require.ErrorIs(t, <-done, ErrLocalCallAbandoned)

	close(reader.release)
	<-released
	assert.Equal(t, make([]byte, 8), callers, "the late read wrote into the caller's buffer")
}

// fillingReader blocks its one Read until release is closed, then fills
// the buffer it was given.
type fillingReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r *fillingReader) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	for i := range p {
		p[i] = 'x'
	}

	return len(p), nil
}
