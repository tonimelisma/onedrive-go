package graph

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
)

// maxAbandonedLocalCalls bounds how many stuck local file calls the process
// leaves behind at once. Each one is a goroutine parked in a read or write of
// a hung mount, with the buffer it owns; past the bound a stalled transfer
// waits for its call as if nothing could be abandoned, so a mount that hangs
// every call cannot grow the process without limit.
const maxAbandonedLocalCalls = 8

// AbandonableWriter is a download's local file that a transfer may abandon
// while a write to it is stuck (a hung network mount, a FUSE folder). A
// download writes through the transfer's local I/O goroutine only when its
// writer is one: abandoning a write to any other writer would let the retry
// write beside the stuck write, so such a download waits for its write as
// the transfer always did.
type AbandonableWriter interface {
	io.Writer
	// Name names the local file, for the record of abandoned calls.
	Name() string
	// Abandon is called once, on the transfer's side, when the write stuck
	// in the file is abandoned: from here the file is no longer the
	// transfer's, and nothing may write to it, resume it or reuse its name
	// until AbandonedWriteReturned. It must not block.
	Abandon()
	// AbandonedWriteReturned is called once, on the abandoned call's
	// goroutine, after the stuck write has returned: the file can be closed
	// and discarded now. It may block; it holds no transfer.
	AbandonedWriteReturned()
}

// localSide says which local call a transfer abandoned.
type localSide string

const (
	// localSideRead is an upload reading its local file.
	localSideRead localSide = "read"
	// localSideWrite is a download writing its local file.
	localSideWrite localSide = "write"
)

// errLocalCallAbandoned is what a stuck local call returns to the transfer
// that abandoned it. The transfer's own error is the stall's
// (ErrTransferStalled, ErrLocalSideStalled), which explain puts in front;
// this one is kept as its text.
var errLocalCallAbandoned = errors.New("graph: abandoned a local file call that stopped moving")

// errLocalIOEnded is what a local call returns once its transfer has ended
// or has abandoned its I/O goroutine: nothing runs it any more.
var errLocalIOEnded = errors.New("graph: the transfer's local file calls have ended")

// abandonedLocalCall is the record of one abandoned call.
type abandonedLocalCall struct {
	path  string
	side  localSide
	since time.Time
}

// abandonedLocalCalls owns every local I/O goroutine a transfer abandoned:
// each stays in it from the moment its transfer walks away until its stuck
// call returns, when it finishes its file and leaves. It is bounded by
// limit; adopt refuses past it.
type abandonedLocalCalls struct {
	limit int
	now   func() time.Time
	// released, when set, is told each time a goroutine leaves: a test's
	// view of a stuck call returning. Nil in production.
	released func()

	// mu guards calls.
	mu    sync.Mutex
	calls map[*localIO]abandonedLocalCall
}

func newAbandonedLocalCalls(limit int) *abandonedLocalCalls {
	return &abandonedLocalCalls{limit: limit, now: time.Now, calls: make(map[*localIO]abandonedLocalCall)}
}

// processAbandonedLocalCalls is the process's one record of abandoned local
// calls. It is process-wide on purpose: the bound is on what the process
// leaves behind, whatever client or drive a transfer belongs to. Tests give a
// client its own (Client.abandonedLocal). A goroutine still in it when the
// process exits ends with the process; its file is closed by the exit.
//
//nolint:gochecknoglobals // the process-wide owner of abandoned local I/O goroutines, see above
var processAbandonedLocalCalls = newAbandonedLocalCalls(maxAbandonedLocalCalls)

// AbandonedLocalCallCount reports how many stuck local file calls the
// process has abandoned and that have not returned yet.
func AbandonedLocalCallCount() int {
	return processAbandonedLocalCalls.count()
}

// adopt takes owner's goroutine over, recording call, unless the bound is
// reached. It reports whether it did.
func (r *abandonedLocalCalls) adopt(owner *localIO, call abandonedLocalCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.calls) >= r.limit {
		return false
	}
	call.since = r.now()
	r.calls[owner] = call

	return true
}

// release forgets owner's goroutine, whose stuck call has returned, and
// returns its record.
func (r *abandonedLocalCalls) release(owner *localIO) abandonedLocalCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	call := r.calls[owner]
	delete(r.calls, owner)

	return call
}

func (r *abandonedLocalCalls) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.calls)
}

// abandonedLocal is the record a client's transfers hand stuck calls to.
func (c *Client) abandonedLocalRecord() *abandonedLocalCalls {
	if c.abandonedLocal != nil {
		return c.abandonedLocal
	}

	return processAbandonedLocalCalls
}

// localIO runs one transfer's local file calls (an upload's reads, a
// download's writes) on one goroutine of its own, started with the first
// call, so that the transfer can walk away from a call that does not return.
// Go cannot interrupt a blocked read or write of a file; without this a
// transfer whose local disk hung held its worker until the call returned.
//
// The goroutine belongs to the transfer until the transfer ends (end), or,
// when its call is stuck past the stall timeout, to the record of abandoned
// calls (abandonedLocalCalls), which owns it until the call returns.
//
// One call runs at a time (turn). The buffer a call reads into or writes from
// is the goroutine's, never the caller's: an abandoned read must not fill a
// buffer the transport has handed to the next attempt, and an abandoned write
// must not read one that io.Copy has filled again. The turn holder copies
// into it before the call and out of it after; an abandoned call keeps it.
type localIO struct {
	stalled <-chan struct{}
	record  *abandonedLocalCalls
	logger  *slog.Logger

	// turn holds one token while a call is anywhere between being handed
	// over and its result being taken. An abandoned call never gives it
	// back: the goroutine is gone, and every later call sees the stall.
	turn     chan struct{}
	requests chan *localCall
	// buf is the goroutine's buffer. Between calls only the turn holder
	// touches it; during a call only the goroutine does.
	buf []byte

	// mu guards started, ended, inCall and abandoned: "is the call still
	// inside the file" and "the transfer has walked away from it" change in
	// one step, so a call is either answered or abandoned, never both.
	mu        sync.Mutex
	started   bool
	ended     bool
	inCall    bool
	abandoned bool
}

// localCall is one read or write handed to the goroutine.
type localCall struct {
	run func(buf []byte) (int, error)
	buf []byte
	// path and side describe the call to the record of abandoned calls.
	path string
	side localSide
	// onAbandon runs on the caller's side, as the call is abandoned;
	// onReturned on the goroutine, once an abandoned call has returned.
	// Either may be nil.
	onAbandon  func()
	onReturned func()
	reply      chan localResult
}

type localResult struct {
	n   int
	err error
}

func newLocalIO(stalled <-chan struct{}, record *abandonedLocalCalls, logger *slog.Logger) *localIO {
	if logger == nil {
		logger = slog.Default()
	}

	return &localIO{
		stalled:  stalled,
		record:   record,
		logger:   logger,
		turn:     make(chan struct{}, 1),
		requests: make(chan *localCall, 1),
	}
}

// read reads r into p on the goroutine.
func (l *localIO) read(r io.Reader, p []byte, path string) (int, error) {
	call := &localCall{path: path, side: localSideRead}
	call.run = func(buf []byte) (int, error) {
		return r.Read(buf)
	}

	return l.do(call, len(p), nil, func(buf []byte, n int) { copy(p, buf[:n]) })
}

// write writes p to w on the goroutine. An abandoned write tells w so
// (Abandon), and tells it again once the stuck write has returned.
func (l *localIO) write(w AbandonableWriter, p []byte) (int, error) {
	call := &localCall{
		path: w.Name(), side: localSideWrite,
		onAbandon: w.Abandon, onReturned: w.AbandonedWriteReturned,
	}
	call.run = func(buf []byte) (int, error) {
		return w.Write(buf)
	}

	return l.do(call, len(p), func(buf []byte) { copy(buf, p) }, nil)
}

// do hands call to the goroutine and waits for its result, or, once the
// transfer has stalled, abandons it. fill runs before the call with the
// goroutine's buffer, drain after a call that was answered.
//
// Only a stall abandons a call, never a plain cancellation: a local call
// returns at once unless its disk hangs, and the stall timer keeps running
// through a shutdown, so a call that hangs then is abandoned when the timer
// fires. Abandoning at every cancellation would disown the partial of every
// download that was writing when sync stopped.
func (l *localIO) do(call *localCall, size int, fill func([]byte), drain func([]byte, int)) (int, error) {
	// A free goroutine is taken even after the stall: the call then runs,
	// and is abandoned unless it returns at once.
	select {
	case l.turn <- struct{}{}:
	default:
		select {
		case l.turn <- struct{}{}:
		case <-l.stalled:
			// Another call holds the goroutine, stuck: this one cannot run.
			return 0, errLocalCallAbandoned
		}
	}

	if cap(l.buf) < size {
		l.buf = make([]byte, size)
	}
	call.buf = l.buf[:size]
	if fill != nil {
		fill(call.buf)
	}
	call.reply = make(chan localResult, 1)

	if err := l.handOver(call); err != nil {
		<-l.turn
		return 0, err
	}

	select {
	case res := <-call.reply:
		return l.answered(call, res, drain)
	case <-l.stalled:
	}

	if l.abandon(call) {
		return 0, errLocalCallAbandoned
	}

	return l.answered(call, <-call.reply, drain)
}

// handOver starts the goroutine if need be and gives it call.
func (l *localIO) handOver(call *localCall) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.ended || l.abandoned {
		return errLocalIOEnded
	}
	if !l.started {
		l.started = true
		go l.run()
	}
	l.inCall = true
	// Never blocks: requests holds one call and only the turn holder sends.
	l.requests <- call

	return nil
}

// answered takes an answered call's result and gives the turn back.
func (l *localIO) answered(call *localCall, res localResult, drain func([]byte, int)) (int, error) {
	if drain != nil && res.n > 0 {
		drain(call.buf, res.n)
	}
	<-l.turn

	return res.n, res.err
}

// abandon hands the goroutine, stuck in call, to the record of abandoned
// calls. It reports false when the call has already returned (its answer
// is on its way) or when the record is full, and the caller then waits for
// the answer as a transfer always did.
func (l *localIO) abandon(call *localCall) bool {
	adopted, full := l.tryAbandon(call)
	switch {
	case adopted:
		l.logger.Warn("a local file stopped moving; the transfer gives up on it and goes on",
			slog.String("path", call.path),
			slog.String("call", string(call.side)),
		)
	case full:
		l.logger.Warn("a local file stopped moving and too many stuck calls are already left behind; waiting for it",
			slog.String("path", call.path),
			slog.String("call", string(call.side)),
			slog.Int("abandoned_calls", l.record.limit),
		)
	}

	return adopted
}

func (l *localIO) tryAbandon(call *localCall) (adopted, full bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.inCall {
		return false, false
	}
	if !l.record.adopt(l, abandonedLocalCall{path: call.path, side: call.side}) {
		return false, true
	}
	l.abandoned = true
	if call.onAbandon != nil {
		// Under mu, so it runs before the goroutine can see the call
		// return and run onReturned.
		call.onAbandon()
	}

	return true, false
}

// end ends the transfer's use of the goroutine: an idle one exits, one
// that is answering a last call exits after it, and an abandoned one is
// the record's.
func (l *localIO) end() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.ended {
		return
	}
	l.ended = true
	close(l.requests)
}

// run is the goroutine: it runs each call it is handed and answers it,
// until the transfer ends or abandons it.
func (l *localIO) run() {
	for call := range l.requests {
		res := l.perform(call)

		l.mu.Lock()
		abandoned := l.abandoned
		l.inCall = false
		l.mu.Unlock()

		if abandoned {
			l.returnedAfterAbandon(call)
			return
		}
		call.reply <- res
	}
}

// perform runs one call. A panic in it is this goroutine's: it is logged
// and answered as the call's error.
func (l *localIO) perform(call *localCall) (res localResult) {
	defer func() {
		if r := recover(); r != nil {
			res = localResult{err: panicguard.Recovered(l.logger, "in a transfer's local file call", r)}
		}
	}()

	n, err := call.run(call.buf)

	return localResult{n: n, err: err}
}

// returnedAfterAbandon finishes an abandoned call once it has returned:
// its file's owner finishes the file, and the goroutine leaves the record.
func (l *localIO) returnedAfterAbandon(call *localCall) {
	defer func() {
		record := l.record.release(l)
		l.logger.Info("an abandoned local file call returned",
			slog.String("path", record.path),
			slog.String("call", string(record.side)),
			slog.Duration("stuck_for", l.record.now().Sub(record.since)),
		)
		if l.record.released != nil {
			l.record.released()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			//nolint:errcheck // logged by Recovered; nobody is left to return it to
			_ = panicguard.Recovered(l.logger, "finishing an abandoned local file call", r)
		}
	}()

	if call.onReturned != nil {
		call.onReturned()
	}
}

// localReader reads an upload's local file through its transfer's
// goroutine.
type localReader struct {
	inner io.Reader
	local *localIO
	path  string
}

func (r *localReader) Read(p []byte) (int, error) {
	return r.local.read(r.inner, p, r.path)
}

// localWriter writes a download's local file through its transfer's
// goroutine.
type localWriter struct {
	inner AbandonableWriter
	local *localIO
}

func (w *localWriter) Write(p []byte) (int, error) {
	return w.local.write(w.inner, p)
}

// namedLocalFile names the local file behind an upload's content, for the
// record of abandoned calls: the content is an *os.File, possibly inside
// section readers. Unnamed content is recorded without a path.
func namedLocalFile(content io.ReaderAt) string {
	for {
		switch r := content.(type) {
		case interface{ Name() string }:
			return r.Name()
		case *io.SectionReader:
			outer, _, _ := r.Outer()
			content = outer
		default:
			return ""
		}
	}
}
