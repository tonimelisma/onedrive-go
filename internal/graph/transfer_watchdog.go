package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// ErrTransferStalled reports a transfer body that made no progress for the
// stall timeout. The transports only bound dialing, TLS and the wait for
// response headers; a connection that delivered headers and then went quiet
// held a worker slot forever, and a one-shot run never ended (D-045). The
// error is retryable: a download resumes from its partial, an upload from
// its session.
var ErrTransferStalled = errors.New("graph: transfer stalled")

// defaultTransferStallTimeout is how long a transfer body may make no
// progress before it is abandoned.
const defaultTransferStallTimeout = 2 * time.Minute

// stallWatchdog cancels a transfer's request context when its body stops
// moving. It runs only around body I/O: while a request body is being
// written (from the first read until it is drained) and while a response
// body is being read. It never runs across a dial, the wait for headers
// (the transports' ResponseHeaderTimeout bounds that) or a retry wait, so a
// Retry-After longer than the stall timeout is not a stall.
type stallWatchdog struct {
	timeout time.Duration
	cancel  context.CancelCauseFunc
	logger  *slog.Logger

	// mu makes "is this reader's attempt still the current one" and the timer
	// change that follows from it one step. The transport reads a body on its
	// own goroutine, and the retry wait starts on the caller's. With the two
	// apart, a read that found its attempt current armed the timer just after
	// the wait had stopped it, and the timer ran through the wait: a
	// Retry-After longer than the stall timeout failed the fragment as stalled.
	mu    sync.Mutex
	timer *time.Timer
	// attempt counts retry waits, and the end of the transfer. A body reader
	// belongs to one attempt: the transport can still be reading an abandoned
	// attempt's body during a retry wait, during the next attempt and after
	// the transfer has ended, and those reads must not touch the timer.
	attempt int64
}

// watchTransfer returns a context that is canceled with ErrTransferStalled
// once the returned watchdog has been armed and not touched for the client's
// stall timeout. The watchdog starts disarmed; the caller must call stop.
// Retry and throttle waits inside the transport disarm it.
func (c *Client) watchTransfer(ctx context.Context) (context.Context, *stallWatchdog) {
	timeout := c.transferStallTimeout
	if timeout <= 0 {
		timeout = defaultTransferStallTimeout
	}

	stallCtx, cancel := context.WithCancelCause(ctx)
	watchdog := &stallWatchdog{timeout: timeout, cancel: cancel, logger: c.logger}
	watchdog.timer = time.AfterFunc(timeout, watchdog.stalled)
	watchdog.timer.Stop()

	return retry.WithBackoffObserver(stallCtx, watchdog.waitStarts), watchdog
}

// stalled is the stall timer's callback: it cancels the transfer with
// ErrTransferStalled. It runs on a goroutine of its own, so a panic in it is
// recovered here and fails this one transfer: the transfer is canceled with
// the panic's error, which explain returns.
func (w *stallWatchdog) stalled() {
	defer func() {
		if r := recover(); r != nil {
			w.cancel(panicguard.Recovered(w.logger, "in the transfer stall watchdog", r))
		}
	}()
	w.cancel(ErrTransferStalled)
}

// waitStarts runs before every retry or throttle wait: it disarms the
// watchdog and retires the current attempt's body reader.
func (w *stallWatchdog) waitStarts() {
	w.retireAttempt()
}

// stop ends the transfer: it disarms the watchdog for good and releases the
// request context.
func (w *stallWatchdog) stop() {
	w.retireAttempt()
	w.cancel(nil)
}

// retireAttempt stops the stall timer and makes every reader handed out so
// far a stale one, so none of them can start it again.
func (w *stallWatchdog) retireAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempt++
	w.timer.Stop()
}

// arm (re)starts the stall timer when attempt is still the current one: body
// bytes are expected now.
func (w *stallWatchdog) arm(attempt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if attempt == w.attempt {
		w.timer.Reset(w.timeout)
	}
}

// disarm stops the stall timer when attempt is still the current one: its
// body is drained or failed, so nothing is expected to move.
func (w *stallWatchdog) disarm(attempt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if attempt == w.attempt {
		w.timer.Stop()
	}
}

// reader wraps a body so that reading it arms the watchdog, every read that
// moves bytes re-arms it, and the end of the body disarms it. The reader
// belongs to the attempt that is current when it is made.
func (w *stallWatchdog) reader(r io.Reader) io.Reader {
	w.mu.Lock()
	defer w.mu.Unlock()

	return &progressReader{inner: r, watchdog: w, attempt: w.attempt}
}

// explain turns a failure caused by the watchdog into ErrTransferStalled
// alone. The inner error is kept as text only: it carries
// context.Canceled, which every caller reads as a shutdown, so a stalled
// transfer was dropped instead of retried.
func (w *stallWatchdog) explain(ctx context.Context, err error) error {
	// A panic in the stall timer's callback (stalled) fails the transfer
	// with the panic's error, the inner error kept as text for the same
	// reason.
	if cause := context.Cause(ctx); err != nil && errors.Is(cause, panicguard.ErrPanic) {
		//nolint:errorlint // the inner error is deliberately not wrapped, see above
		return fmt.Errorf("%w (%v)", cause, err)
	}
	if err == nil || !errors.Is(context.Cause(ctx), ErrTransferStalled) {
		return err
	}

	//nolint:errorlint // the inner error is deliberately not wrapped, see above
	return fmt.Errorf("%w: no data moved for %s (%v)", ErrTransferStalled, w.timeout, err)
}

type progressReader struct {
	inner    io.Reader
	watchdog *stallWatchdog
	attempt  int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	p.watchdog.arm(p.attempt)

	n, err := p.inner.Read(b)
	if err != nil {
		// Drained or failed: no more body bytes are expected from here.
		p.watchdog.disarm(p.attempt)
	}

	return n, err //nolint:wrapcheck // io.EOF must reach io.Copy unwrapped; callers wrap real failures
}
