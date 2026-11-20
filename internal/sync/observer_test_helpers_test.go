package sync

import (
	"context"
	"time"
)

// observerTestClock is the observer-side counterpart to manualClock: one clock
// value supplying whatever a test needs to control, with the real clock behind
// it for everything else. Tests set only the hooks they care about, so a test
// that controls the safety-scan tick still gets coherent timers and sleeps
// rather than a mix of controlled and wall-clock time.
type observerTestClock struct {
	SyncClock

	// tickCh, when non-nil, backs every ticker this clock hands out.
	tickCh <-chan time.Time
	// sleep, when non-nil, replaces the real sleep.
	sleep func(context.Context, time.Duration) error
	// afterFunc, when non-nil, replaces the real delayed callback.
	afterFunc func(time.Duration, func()) SyncTimer
}

func newObserverTestClock() *observerTestClock {
	return &observerTestClock{SyncClock: RealClock{}}
}

// fixedTicker delivers on a channel the test owns and ignores its interval.
type fixedTicker struct {
	ch <-chan time.Time
}

func (t *fixedTicker) Chan() <-chan time.Time { return t.ch }

func (t *fixedTicker) Stop() {}

func (c *observerTestClock) NewTicker(interval time.Duration) SyncTicker {
	if c.tickCh != nil {
		return &fixedTicker{ch: c.tickCh}
	}

	return c.SyncClock.NewTicker(interval)
}

func (c *observerTestClock) Sleep(ctx context.Context, delay time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, delay)
	}

	//nolint:wrapcheck // decorator delegates to the wrapped clock; adding context here would attribute the failure to the test seam rather than the clock that produced it
	return c.SyncClock.Sleep(ctx, delay)
}

func (c *observerTestClock) AfterFunc(delay time.Duration, fn func()) SyncTimer {
	if c.afterFunc != nil {
		return c.afterFunc(delay, fn)
	}

	return c.SyncClock.AfterFunc(delay, fn)
}
