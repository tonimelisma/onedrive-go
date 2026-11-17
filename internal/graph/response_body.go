package graph

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
)

// ErrResponseStalled reports a response body that brought no byte for the
// stall window (defaultStallTimeout) while it was being read: the server sent
// its headers and went quiet. It is retryable; sync reads it as a request
// OneDrive never answered.
var ErrResponseStalled = errors.New("graph: response stalled")

// ErrConnectionLost reports a request, or a response body being read, on an
// HTTP/2 connection that net/http closed because the connection's
// health-check ping went unanswered. Sync reads it as a request OneDrive
// never answered.
var ErrConnectionLost = errors.New("graph: connection lost")

// http2ConnectionLostText is the text of the error net/http's HTTP/2 client
// fails every request and every response body on a connection with once the
// connection's health-check ping went unanswered (closeForLostPing in the
// bundled HTTP/2 transport). The library makes it with errors.New and gives
// it no type, no Timeout and no Is, so its text is all there is to know it
// by; markLostConnection is the one reader of it.
const http2ConnectionLostText = "http2: client connection lost"

// markLostConnection gives err ErrConnectionLost when its innermost error is
// the HTTP/2 client's lost connection, and returns any other err unchanged.
// Every failure of a round trip and of a response body read passes through
// it (dispatchRequestWith, responseBody.Read).
func markLostConnection(err error) error {
	inner := err
	for next := errors.Unwrap(inner); next != nil; next = errors.Unwrap(inner) {
		inner = next
	}
	if inner == nil || inner.Error() != http2ConnectionLostText {
		return err
	}

	return fmt.Errorf("%w: %w", ErrConnectionLost, err)
}

// ResponseReadError marks a failure while reading a response body: the
// connection dropped, timed out or was reset after OneDrive had started
// answering. net/http reports those without the *url.Error it uses for
// failures before the response, so without this mark a caller could not tell
// a dropped download from a local disk error that io.Copy returned for the
// same call. Unwrap keeps the cause (io.ErrUnexpectedEOF, a timeout, an HTTP/2
// stream reset, context cancellation) reachable through errors.Is/As.
type ResponseReadError struct {
	Err error
}

func (e *ResponseReadError) Error() string {
	return "graph: reading the response: " + e.Err.Error()
}

func (e *ResponseReadError) Unwrap() error { return e.Err }

// responseBody is every response body the client hands back
// (dispatchRequestWith). A read failure other than io.EOF carries
// *ResponseReadError, and one on a connection the HTTP/2 health check closed
// carries ErrConnectionLost too. A body that brings no byte for window while
// it is read is abandoned: each Read starts a timer and its return stops it,
// and a timer that fires closes the body, which ends the pending read
// (net/http's bodies end a Read on Close), so the read fails with
// ErrResponseStalled. Only the time spent waiting in a Read counts, never the
// caller's work between reads: a body that keeps moving, however slowly, is
// never failed. A body handed to the transfer stall watchdog
// (stallWatchdog.reader) starts no timer of its own.
type responseBody struct {
	body   io.ReadCloser
	window time.Duration
	logger *slog.Logger

	// closeOnce closes body once, for the caller's Close or the timer,
	// whichever comes first; Close returns closeErr.
	closeOnce sync.Once
	closeErr  error

	// mu guards the fields below: a read runs on the caller's goroutine, a
	// timer's callback on its own.
	mu sync.Mutex
	// reads counts the reads that started a timer, and reading is true while
	// the latest of them waits: a callback of an earlier read, or one that
	// fires as its read returns, changes nothing.
	reads   uint64
	reading bool
	timer   *time.Timer
	// abandoned is why a timer closed the body: ErrResponseStalled. Nil while
	// none has.
	abandoned error
	// watchedByTransfer is set once the transfer stall watchdog bounds the
	// body instead: its window is the same, and the two would race.
	watchedByTransfer bool
}

// newResponseBody wraps body with the client's stall window.
func newResponseBody(body io.ReadCloser, window time.Duration, logger *slog.Logger) *responseBody {
	return &responseBody{body: body, window: window, logger: logger}
}

func (b *responseBody) Read(p []byte) (int, error) {
	b.startRead()
	n, err := b.body.Read(p)
	abandoned := b.endRead()

	switch {
	case err == nil || errors.Is(err, io.EOF):
		// Bytes that arrived, and the body's normal end, stand even when a
		// timer fired as they did; the next read finds the body closed.
		return n, err //nolint:wrapcheck // io.EOF must reach io.Copy and decoders unwrapped
	case abandoned != nil:
		return n, &ResponseReadError{Err: explainStall(abandoned, b.window, err)}
	default:
		return n, &ResponseReadError{Err: markLostConnection(err)}
	}
}

// Close closes the wrapped body, unless a timer already has, and returns that
// close's error.
func (b *responseBody) Close() error {
	b.closeUnderlying()

	return b.closeErr
}

func (b *responseBody) closeUnderlying() {
	b.closeOnce.Do(func() { b.closeErr = b.body.Close() })
}

// startRead starts the timer of a read, unless the transfer watchdog bounds
// the body or a timer has already abandoned it.
func (b *responseBody) startRead() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.watchedByTransfer || b.abandoned != nil {
		return
	}

	b.reads++
	b.reading = true
	read := b.reads
	b.timer = time.AfterFunc(b.window, func() {
		// The callback runs on a goroutine of its own, so a panic in it (in
		// the wrapped body's Close) is recovered here and logged once. The
		// body is abandoned for its stall before the close is tried, so a
		// read the close did end reports that stall. A close that panicked
		// counts as done: the caller's Close does not try it again.
		defer func() {
			if r := recover(); r != nil {
				//nolint:errcheck // The read reports the stall it was abandoned for; the error is only logged.
				panicguard.Recovered(b.logger, "in a response body's stall timer", r)
			}
		}()
		b.abandon(read)
	})
}

// endRead stops the read's timer and returns why a timer abandoned the body,
// or nil.
func (b *responseBody) endRead() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.reading = false
	if b.timer != nil {
		b.timer.Stop()
	}

	return b.abandoned
}

// abandon closes the body for its stall when read is the latest read, still
// waiting, and nothing has abandoned the body yet.
func (b *responseBody) abandon(read uint64) {
	b.mu.Lock()
	current := b.reading && read == b.reads && b.abandoned == nil
	if current {
		b.abandoned = ErrResponseStalled
	}
	b.mu.Unlock()

	if current {
		b.closeUnderlying()
	}
}

// leaveBoundToTransfer makes the transfer stall watchdog the body's only
// bound: no later read starts a timer of the body's own.
func (b *responseBody) leaveBoundToTransfer() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.watchedByTransfer = true
}
