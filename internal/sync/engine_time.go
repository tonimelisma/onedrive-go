package sync

import "time"

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
