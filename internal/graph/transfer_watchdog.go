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
// stall timeout. The transports bound only dialing, TLS and the wait for
// response headers; without this bound a connection that delivered headers
// and then went quiet would hold a worker slot, and a one-shot run, for good
// (D-045). The error is retryable: a download resumes from its partial, an
// upload from its session.
var ErrTransferStalled = errors.New("graph: transfer stalled")

// ErrLocalSideStalled marks a stall whose silence was on the local side of
// the transfer: an upload's read of the local file, or a download's write of
// the local file between two body reads, made no progress for the stall
// timeout. It always comes with ErrTransferStalled. A local disk that hangs
// (a network mount, a FUSE folder) is not the service's silence, so the
// sync classifier keeps such a stall out of the service window and retries
// the one file instead (R-6.8.20). The transfer does not wait for the stuck
// call: it is abandoned to the record of abandoned local calls
// (transfer_local_io.go) and the worker is free.
var ErrLocalSideStalled = errors.New("graph: the local file stopped moving")

// defaultStallTimeout is how long a body may make no progress before it is
// abandoned: a transfer body under the stall watchdog, and every other
// response body (responseBody). One window for both: a metadata answer is
// small, so a server that keeps it from moving for as long as a transfer may
// pause is not answering.
const defaultStallTimeout = 2 * time.Minute

// stallWindow is the client's stall window: stallTimeout, or
// defaultStallTimeout when that is unset.
func (c *Client) stallWindow() time.Duration {
	if c.stallTimeout > 0 {
		return c.stallTimeout
	}

	return defaultStallTimeout
}

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
	// bodyIsLocal says which side the current attempt's body reads from: the
	// local file (an upload's request body) or the network (a download's
	// response body). inRead is set while the transport or the caller is
	// inside a read of that body. Together they say on which side a stall
	// fell: inside a read of a local body, or between two reads of a network
	// body, where the caller writes the local file, it is the local side.
	bodyIsLocal bool
	inRead      bool
	// attempt counts retry waits, and the end of the transfer. A body reader
	// belongs to one attempt: the transport can still be reading an abandoned
	// attempt's body during a retry wait, during the next attempt and after
	// the transfer has ended, and those reads must not touch the timer.
	attempt int64

	// stallSignal is closed once the stall timer has fired and canceled the
	// transfer; hasStalled says it has been. A local file call that is still
	// stuck then is abandoned (local).
	stallSignal chan struct{}
	hasStalled  bool
	// local runs the transfer's local file calls on a goroutine the
	// transfer can walk away from.
	local *localIO
}

// watchTransfer returns a context that is canceled with ErrTransferStalled
// once the returned watchdog has been armed and not touched for the client's
// stall timeout. The watchdog starts disarmed; the caller must call stop.
// Retry and throttle waits inside the transport disarm it.
func (c *Client) watchTransfer(ctx context.Context) (context.Context, *stallWatchdog) {
	timeout := c.stallWindow()

	stallCtx, cancel := context.WithCancelCause(ctx)
	watchdog := &stallWatchdog{timeout: timeout, cancel: cancel, logger: c.logger, stallSignal: make(chan struct{})}
	watchdog.local = newLocalIO(watchdog.stallSignal, c.abandonedLocalRecord(), c.logger)
	watchdog.timer = time.AfterFunc(timeout, watchdog.stalled)
	watchdog.timer.Stop()

	return retry.WithBackoffObserver(stallCtx, watchdog.waitStarts), watchdog
}

// stalled is the stall timer's callback: it cancels the transfer with
// ErrTransferStalled, then signals the stall to a local file call that is
// stuck, which abandons it. The cancel comes first, so a transfer that
// returns because its local call was abandoned finds the stall as its
// context's cause. It runs on a goroutine of its own, so a panic in it is
// recovered here and fails this one transfer: the transfer is canceled with
// the panic's error, which explain returns.
func (w *stallWatchdog) stalled() {
	defer func() {
		if r := recover(); r != nil {
			w.cancel(panicguard.Recovered(w.logger, "in the transfer stall watchdog", r))
		}
	}()
	w.cancel(w.stallCause())
	w.signalStall()
}

// signalStall closes stallSignal, once: the timer can fire again after a
// late read of an abandoned attempt re-armed it.
func (w *stallWatchdog) signalStall() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.hasStalled || w.stallSignal == nil {
		return
	}
	w.hasStalled = true
	close(w.stallSignal)
}

// stallCause is the cause a stall cancels the transfer with: ErrTransferStalled,
// with ErrLocalSideStalled when the silence was on the local side.
func (w *stallWatchdog) stallCause() error {
	w.mu.Lock()
	localSide := w.inRead == w.bodyIsLocal
	w.mu.Unlock()
	if localSide {
		return fmt.Errorf("%w: %w", ErrTransferStalled, ErrLocalSideStalled)
	}

	return ErrTransferStalled
}

// waitStarts runs before every retry or throttle wait: it disarms the
// watchdog and retires the current attempt's body reader.
func (w *stallWatchdog) waitStarts() {
	w.retireAttempt()
}

// stop ends the transfer: it disarms the watchdog for good, releases the
// request context and ends the transfer's local I/O goroutine (one it
// abandoned is no longer the transfer's).
func (w *stallWatchdog) stop() {
	w.retireAttempt()
	w.cancel(nil)
	if w.local != nil {
		w.local.end()
	}
}

// retireAttempt stops the stall timer and makes every reader handed out so
// far a stale one, so none of them can start it again.
func (w *stallWatchdog) retireAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempt++
	w.timer.Stop()
}

// arm (re)starts the stall timer when attempt is still the current one, as a
// read of its body begins: body bytes are expected now.
func (w *stallWatchdog) arm(attempt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if attempt == w.attempt {
		w.inRead = true
		w.timer.Reset(w.timeout)
	}
}

// readReturned records that a read of attempt's body returned; the timer
// keeps running until the next read re-arms it, so the time the other side
// takes between two reads counts too.
func (w *stallWatchdog) readReturned(attempt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if attempt == w.attempt {
		w.inRead = false
	}
}

// disarm stops the stall timer when attempt is still the current one: its
// body is drained or failed, so nothing is expected to move.
func (w *stallWatchdog) disarm(attempt int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if attempt == w.attempt {
		w.inRead = false
		w.timer.Stop()
	}
}

// localBody wraps an upload's request body, which reads the local file
// named path: its reads run on the transfer's local I/O goroutine, so a read
// that hangs past the stall timeout is abandoned instead of holding the
// transport, and with it the worker.
func (w *stallWatchdog) localBody(r io.Reader, path string) io.Reader {
	return w.reader(&localReader{inner: r, local: w.local, path: path}, true)
}

// localWriter wraps a download's local file. Writes to an AbandonableWriter
// run on the transfer's local I/O goroutine, so a write that hangs past the
// stall timeout is abandoned; any other writer is written directly, and a
// write that hangs holds the transfer until it returns.
func (w *stallWatchdog) localWriter(dst io.Writer) io.Writer {
	if abandonable, ok := dst.(AbandonableWriter); ok {
		return &localWriter{inner: abandonable, local: w.local}
	}

	return dst
}

// networkBody wraps a download's response body, which reads the network;
// the caller writes the local file between two reads.
func (w *stallWatchdog) networkBody(r io.Reader) io.Reader {
	return w.reader(r, false)
}

// reader wraps a body so that reading it arms the watchdog, every read that
// moves bytes re-arms it, and the end of the body disarms it. The reader
// belongs to the attempt that is current when it is made; isLocal says which
// side the body reads from. A response body (responseBody) is left to this
// watchdog alone: its own timer has the same window and would race it, and a
// download that failed as a stalled response rather than a stalled transfer
// would lose its partial.
func (w *stallWatchdog) reader(r io.Reader, isLocal bool) io.Reader {
	if body, ok := r.(*responseBody); ok {
		body.leaveBoundToTransfer()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.bodyIsLocal = isLocal

	return &progressReader{inner: r, watchdog: w, attempt: w.attempt}
}

// explain turns a failure caused by the watchdog into ErrTransferStalled
// alone. The inner error is kept as text only: it carries
// context.Canceled, which every caller reads as a shutdown, so a stalled
// transfer was dropped instead of retried.
func (w *stallWatchdog) explain(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	if err == nil || (!errors.Is(cause, panicguard.ErrPanic) && !errors.Is(cause, ErrTransferStalled)) {
		return err
	}

	if w.local != nil && w.local.hasAbandoned() {
		cause = fmt.Errorf("%w: %w", cause, ErrLocalCallAbandoned)
	}

	return explainStall(cause, w.timeout, err)
}

// explainStall is how a body abandoned for its silence explains the failure
// it ended in, err: cause is the stall's sentinel (ErrTransferStalled,
// ErrResponseStalled), or, for the transfer watchdog, the error of a panic in
// its timer's callback, which fails the transfer instead. err is kept as text
// only: it carries context.Canceled or a closed body's error, which a caller
// would read as a shutdown or a dropped connection.
func explainStall(cause error, timeout time.Duration, err error) error {
	if errors.Is(cause, panicguard.ErrPanic) {
		//nolint:errorlint // the inner error is deliberately not wrapped, see above
		return fmt.Errorf("%w (%v)", cause, err)
	}

	//nolint:errorlint // the inner error is deliberately not wrapped, see above
	return fmt.Errorf("%w: no data moved for %s (%v)", cause, timeout, err)
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
	} else {
		p.watchdog.readReturned(p.attempt)
	}

	return n, err //nolint:wrapcheck // io.EOF must reach io.Copy unwrapped; callers wrap real failures
}
