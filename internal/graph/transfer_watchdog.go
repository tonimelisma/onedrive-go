package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

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
	timer   *time.Timer
	cancel  context.CancelCauseFunc
	// attempt counts retry waits. A body reader belongs to one attempt: the
	// transport can still be draining an abandoned attempt's body during a
	// retry wait, and those reads must not re-arm the watchdog.
	attempt atomic.Int64
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
	watchdog := &stallWatchdog{timeout: timeout, cancel: cancel}
	watchdog.timer = time.AfterFunc(timeout, func() { cancel(ErrTransferStalled) })
	watchdog.timer.Stop()

	return retry.WithBackoffObserver(stallCtx, watchdog.waitStarts), watchdog
}

// waitStarts runs before every retry or throttle wait: it disarms the
// watchdog and retires the current attempt's body reader.
func (w *stallWatchdog) waitStarts() {
	w.attempt.Add(1)
	w.disarm()
}

// arm (re)starts the stall timer: body bytes are expected now.
func (w *stallWatchdog) arm() { w.timer.Reset(w.timeout) }

// disarm stops the stall timer: nothing is expected to move.
func (w *stallWatchdog) disarm() { w.timer.Stop() }

func (w *stallWatchdog) stop() {
	w.timer.Stop()
	w.cancel(nil)
}

// reader wraps a body so that reading it arms the watchdog, every read that
// moves bytes re-arms it, and the end of the body disarms it.
func (w *stallWatchdog) reader(r io.Reader) io.Reader {
	return &progressReader{inner: r, watchdog: w, attempt: w.attempt.Load()}
}

// explain turns a failure caused by the watchdog into ErrTransferStalled
// alone. The inner error is kept as text only: it carries
// context.Canceled, which every caller reads as a shutdown, so a stalled
// transfer was dropped instead of retried.
func (w *stallWatchdog) explain(ctx context.Context, err error) error {
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
	current := p.watchdog.attempt.Load() == p.attempt
	if current {
		p.watchdog.arm()
	}

	n, err := p.inner.Read(b)
	if err != nil && current {
		// Drained or failed: no more body bytes are expected from here.
		p.watchdog.disarm()
	}

	return n, err //nolint:wrapcheck // io.EOF must reach io.Copy unwrapped; callers wrap real failures
}
