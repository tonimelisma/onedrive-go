package sync

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

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

func realAfterFunc(delay time.Duration, fn func()) syncTimer {
	return &realSyncTimer{timer: time.AfterFunc(delay, fn)}
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
