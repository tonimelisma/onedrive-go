package sync

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
)

// syncClock is the engine's single time capability. Every time-dependent
// decision in the runtime -- now, delayed callbacks, tickers, sleeps, and
// retry jitter -- goes through one value.
//
// It is one interface rather than five injectable function fields because the
// five are not independent. A test that replaced only `now` kept a real
// ticker, and the pair described a state production can never reach: a clock
// that has jumped an hour while the tickers that pace the runtime have not
// moved. Bundling them makes that combination unrepresentable rather than
// merely discouraged.
type syncClock interface {
	Now() time.Time
	AfterFunc(delay time.Duration, fn func()) syncTimer
	NewTicker(interval time.Duration) syncTicker
	Sleep(ctx context.Context, delay time.Duration) error
	Jitter(maxDelay time.Duration) time.Duration
}

// realClock is the production wall-clock implementation of syncClock. The
// engine builds one, with its logger and its timer panics, and every timer the
// engine and its observers start runs on it (newEngine).
type realClock struct {
	logger *slog.Logger
	panics *timerPanics
}

func (realClock) Now() time.Time { return time.Now() }

// AfterFunc runs fn on a goroutine of its own once delay has passed, inside
// the clock's boundary (recoverTimerPanic).
func (c realClock) AfterFunc(delay time.Duration, fn func()) syncTimer {
	return &realSyncTimer{timer: time.AfterFunc(delay, func() {
		defer c.recoverTimerPanic()
		fn()
	})}
}

// recoverTimerPanic is the boundary of every timer callback the clock runs. A
// callback runs outside every run's call stack, so its panic cannot become
// anyone's return value: it is logged once with its stack and handed to the
// clock's timer panics, which end the run bound to them. A clock without timer
// panics, or one whose engine has no run bound, leaves the log line only.
func (c realClock) recoverTimerPanic() {
	r := recover()
	if r == nil {
		return
	}
	c.panics.record(panicguard.Recovered(c.logger, "in a sync timer callback", r))
}

// timerPanics carries a timer callback's panic to the run it ends. A real
// clock reports to one (recoverTimerPanic); whoever owns the clock binds each
// run to it. While a run is bound, the first panic is kept for the run's error
// and the run's context is canceled plainly, without a cause: the actions in
// flight then fail as canceled, which reads as a shutdown, so none is recorded
// as a failure and the next run redoes them. Later panics of the same run are
// only logged, and a panic while no run is bound is only logged.
type timerPanics struct {
	// mu guards cancel and err. Every holder releases it with defer: record
	// runs inside a recover block.
	mu     stdsync.Mutex
	cancel context.CancelFunc
	err    error
}

// bind makes cancel the bound run's.
func (p *timerPanics) bind(cancel context.CancelFunc) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel, p.err = cancel, nil
}

// unbind ends the bound run's binding and returns the panic that ended it, if
// one did.
func (p *timerPanics) unbind() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.err
	p.cancel, p.err = nil, nil

	return err
}

// record keeps err for the bound run, unless an earlier panic is kept, and
// cancels the run.
func (p *timerPanics) record(err error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		return
	}
	if p.err == nil {
		p.err = err
	}
	p.cancel()
}

func (realClock) NewTicker(interval time.Duration) syncTicker {
	return realNewTicker(interval)
}

func (realClock) Sleep(ctx context.Context, delay time.Duration) error {
	return realSleep(ctx, delay)
}

func (realClock) Jitter(maxDelay time.Duration) time.Duration {
	return realJitter(maxDelay)
}

// syncTimer is the engine-owned timer boundary. Production uses wall-clock
// timers; tests can inject deterministic implementations without changing the
// watch/runtime code paths.
type syncTimer interface {
	Stop() bool
}

// syncTicker is the engine-owned ticker boundary used by bootstrap and watch
// loops. It keeps ticker ownership explicit and injectable.
type syncTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type realSyncTimer struct {
	timer *time.Timer
}

func (t *realSyncTimer) Stop() bool {
	if t == nil || t.timer == nil {
		return false
	}

	return t.timer.Stop()
}

type realSyncTicker struct {
	ticker *time.Ticker
}

func (t *realSyncTicker) Chan() <-chan time.Time {
	if t == nil || t.ticker == nil {
		return nil
	}

	return t.ticker.C
}

func (t *realSyncTicker) Stop() {
	if t == nil || t.ticker == nil {
		return
	}

	t.ticker.Stop()
}

func realNewTicker(interval time.Duration) syncTicker {
	if interval <= 0 {
		return nil
	}

	return &realSyncTicker{ticker: time.NewTicker(interval)}
}

func realSleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sleep interrupted: %w", ctx.Err())
	}
}

func realJitter(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}

	return rand.N(maxDelay) //nolint:gosec // non-cryptographic jitter for I/O scheduling
}

func tickerChan(ticker syncTicker) <-chan time.Time {
	if ticker == nil {
		return nil
	}

	return ticker.Chan()
}

func stopTicker(ticker syncTicker) {
	if ticker == nil {
		return
	}

	ticker.Stop()
}
