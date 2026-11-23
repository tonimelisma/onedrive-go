package syncdomain

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// SyncClock is the engine's single time and scheduling capability. Every
// time-dependent decision in the runtime -- now, delayed callbacks, tickers,
// sleeps, and retry jitter -- goes through one value, and so does every
// goroutine the sync tree starts (Go): a test that owns the clock owns when
// work runs, not only what time it reads.
//
// It is one interface rather than five injectable function fields because the
// five are not independent. A test that replaced only `now` kept a real
// ticker, and the pair described a state production can never reach: a clock
// that has jumped an hour while the tickers that pace the runtime have not
// moved. Bundling them makes that combination unrepresentable rather than
// merely discouraged.
type SyncClock interface {
	Now() time.Time
	AfterFunc(delay time.Duration, fn func()) SyncTimer
	NewTicker(interval time.Duration) SyncTicker
	Sleep(ctx context.Context, delay time.Duration) error
	Jitter(maxDelay time.Duration) time.Duration
	// Go runs fn on a goroutine of its own. It is the one place the sync
	// tree starts a goroutine; fn keeps whatever panic boundary it had.
	Go(fn func())
}

// RealClock is the production wall-clock implementation of SyncClock. The
// engine builds one, with its logger and its timer panics, and every timer the
// engine and its observers start runs on it (newEngine).
type RealClock struct {
	logger *slog.Logger
	panics *TimerPanics
	// jitter is the source Jitter draws from: seeded once when the engine
	// builds its clock (NewRealClock). A clock built without one, as an
	// observer a test builds by hand has, draws no jitter.
	jitter *retry.Jitter
}

// NewRealClock is the wall clock whose timer callbacks log their panics to
// logger and hand them to panics, and whose jitter comes from a source seeded
// once, now.
func NewRealClock(logger *slog.Logger, panics *TimerPanics) RealClock {
	return RealClock{logger: logger, panics: panics, jitter: retry.NewEntropyJitter()}
}

// Panics is the timer panics the clock's callbacks report to; nil for a
// clock built without them.
func (c RealClock) Panics() *TimerPanics {
	return c.panics
}

func (RealClock) Now() time.Time { return time.Now() }

// WallNow is the wall clock, for what must run on real time whatever clock
// the engine was given: a context deadline (the runtime enforces it on wall
// time, so one computed from a test's clock would expire at once or never)
// and a measured duration. Every other reading of time goes through the
// engine's SyncClock.
func WallNow() time.Time { return time.Now() }

// WallSince is the wall-clock time elapsed since start (WallNow).
func WallSince(start time.Time) time.Duration { return time.Since(start) }

// AfterFunc runs fn on a goroutine of its own once delay has passed, inside
// the clock's boundary (RecoverTimerPanic).
func (c RealClock) AfterFunc(delay time.Duration, fn func()) SyncTimer {
	return &realSyncTimer{timer: time.AfterFunc(delay, func() {
		defer c.RecoverTimerPanic()
		fn()
	})}
}

// RecoverTimerPanic is the boundary of every timer callback the clock runs. A
// callback runs outside every run's call stack, so its panic cannot become
// anyone's return value: it is logged once with its stack and handed to the
// clock's timer panics, which end the run bound to them. A clock without timer
// panics, or one whose engine has no run bound, leaves the log line only.
func (c RealClock) RecoverTimerPanic() {
	r := recover()
	if r == nil {
		return
	}
	c.panics.record(panicguard.Recovered(c.logger, "in a sync timer callback", r))
}

// TimerPanics carries a timer callback's panic to the run it ends. A real
// clock reports to one (RecoverTimerPanic); whoever owns the clock binds each
// run to it. While a run is bound, the first panic is kept for the run's error
// and the run's context is canceled plainly, without a cause: the run has
// ended, so the actions in flight, which fail as canceled, end as a shutdown.
// None is recorded as a failure and the next run redoes them. Later panics of
// the same run are only logged, and a panic while no run is bound is only
// logged.
type TimerPanics struct {
	// mu guards cancel and err. Every holder releases it with defer: record
	// runs inside a recover block.
	mu     stdsync.Mutex
	cancel context.CancelFunc
	err    error
}

// bind makes cancel the bound run's.
func (p *TimerPanics) Bind(cancel context.CancelFunc) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel, p.err = cancel, nil
}

// unbind ends the bound run's binding and returns the panic that ended it, if
// one did.
func (p *TimerPanics) Unbind() error {
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
func (p *TimerPanics) record(err error) {
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

func (RealClock) NewTicker(interval time.Duration) SyncTicker {
	return realNewTicker(interval)
}

func (RealClock) Sleep(ctx context.Context, delay time.Duration) error {
	return realSleep(ctx, delay)
}

// Jitter draws a duration in [0, maxDelay) from the clock's own source; a
// clock without one draws none.
func (c RealClock) Jitter(maxDelay time.Duration) time.Duration {
	if c.jitter == nil {
		return 0
	}

	return c.jitter.Jitter(maxDelay)
}

// Go starts fn on a new goroutine.
func (RealClock) Go(fn func()) {
	go fn()
}

// SyncTimer is the engine-owned timer boundary. Production uses wall-clock
// timers; tests can inject deterministic implementations without changing the
// watch/runtime code paths.
type SyncTimer interface {
	Stop() bool
}

// SyncTicker is the engine-owned ticker boundary used by bootstrap and watch
// loops. It keeps ticker ownership explicit and injectable.
type SyncTicker interface {
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

func realNewTicker(interval time.Duration) SyncTicker {
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

func TickerChan(ticker SyncTicker) <-chan time.Time {
	if ticker == nil {
		return nil
	}

	return ticker.Chan()
}

func StopTicker(ticker SyncTicker) {
	if ticker == nil {
		return
	}

	ticker.Stop()
}
