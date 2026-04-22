// Package sync owns the single-drive runtime, including the debounced dirty
// scheduler that turns local and remote observations into "refresh/replan now"
// signals. It does not buffer planner semantics or preserve event history.
package sync

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type debounceTimer interface {
	Chan() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type realDebounceTimer struct {
	timer *time.Timer
}

func (t *realDebounceTimer) Chan() <-chan time.Time {
	if t == nil || t.timer == nil {
		return nil
	}

	return t.timer.C
}

func (t *realDebounceTimer) Stop() bool {
	if t == nil || t.timer == nil {
		return false
	}

	return t.timer.Stop()
}

func (t *realDebounceTimer) Reset(delay time.Duration) bool {
	if t == nil || t.timer == nil {
		return false
	}

	return t.timer.Reset(delay)
}

func newRealDebounceTimer(delay time.Duration) debounceTimer {
	return &realDebounceTimer{timer: time.NewTimer(delay)}
}

// DirtyBatch is a debounced scheduling signal for refresh/replan work. The
// signal itself means "replan from current truth now"; FullRefresh is the only
// additional scheduler hint the runtime preserves.
type DirtyBatch struct {
	FullRefresh bool
}

// DirtyBuffer coalesces dirty observations into debounced replan signals for
// the snapshot-first runtime. It deliberately does not preserve path subsets or
// event history because the runtime always replans from committed current truth.
type DirtyBuffer struct {
	mu          sync.Mutex // guards dirty, fullRefresh, and notify
	dirty       bool
	fullRefresh bool
	notify      chan struct{}
	logger      *slog.Logger
	newTimer    func(time.Duration) debounceTimer
}

func NewDirtyBuffer(logger *slog.Logger) *DirtyBuffer {
	return &DirtyBuffer{
		logger:   logger,
		newTimer: newRealDebounceTimer,
	}
}

func (b *DirtyBuffer) MarkPath(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if path != "" {
		b.dirty = true
	}
	b.signalNewLocked()
}

func (b *DirtyBuffer) MarkFullRefresh() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fullRefresh = true
	b.signalNewLocked()
}

func (b *DirtyBuffer) FlushImmediate() *DirtyBatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.dirty && !b.fullRefresh {
		return nil
	}

	batch := &DirtyBatch{
		FullRefresh: b.fullRefresh,
	}
	b.dirty = false
	b.fullRefresh = false

	return batch
}

func (b *DirtyBuffer) FlushDebounced(ctx context.Context, debounce time.Duration) <-chan DirtyBatch {
	out := make(chan DirtyBatch, 1)

	b.mu.Lock()
	if b.notify != nil {
		b.mu.Unlock()
		panic("sync: DirtyBuffer FlushDebounced called twice on the same DirtyBuffer")
	}

	b.notify = make(chan struct{}, 1)
	b.mu.Unlock()

	go b.debounceLoop(ctx, debounce, out)

	return out
}

func (b *DirtyBuffer) debounceLoop(ctx context.Context, debounce time.Duration, out chan<- DirtyBatch) {
	defer close(out)

	timer := b.newTimer(debounce)
	timer.Stop()
	defer timer.Stop()

	timerActive := false

	for {
		select {
		case <-ctx.Done():
			if batch := b.FlushImmediate(); batch != nil {
				select {
				case out <- *batch:
				default:
					b.logger.Warn("dirty buffer final drain discarded: output channel full",
						slog.Bool("full_refresh", batch.FullRefresh),
					)
				}
			}
			return

		case _, ok := <-b.notify:
			if !ok {
				return
			}

			if timerActive && !timer.Stop() {
				select {
				case <-timer.Chan():
				default:
				}
			}

			timer.Reset(debounce)
			timerActive = true

		case <-timer.Chan():
			timerActive = false

			if batch := b.FlushImmediate(); batch != nil {
				select {
				case out <- *batch:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (b *DirtyBuffer) signalNewLocked() {
	if b.notify == nil {
		return
	}

	select {
	case b.notify <- struct{}{}:
	default:
	}
}
