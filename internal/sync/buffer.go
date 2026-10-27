// Package sync owns the single mounted content-root runtime, including the debounced dirty
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

// dirtyBatch is a debounced scheduling signal for refresh/replan work. The
// signal itself means "replan from current truth now"; FullRefresh is the only
// additional scheduler hint the runtime preserves.
type dirtyBatch struct {
	FullRefresh bool
}

// dirtyBuffer coalesces dirty observations into debounced replan signals for
// the snapshot-first runtime. It deliberately does not preserve path subsets or
// event history because the runtime always replans from committed current truth.
type dirtyBuffer struct {
	mu          sync.Mutex // guards dirty, fullRefresh, and notify
	dirty       bool
	fullRefresh bool
	notify      chan struct{}
	logger      *slog.Logger
	newTimer    func(time.Duration) debounceTimer
	// newDeadlineTimer builds the max-wait timer; a separate seam so tests
	// can drive the quiet-period timer and the deadline independently.
	newDeadlineTimer func(time.Duration) debounceTimer
}

// minDirtyMaxWait is the floor of the longest a dirty signal waits for a
// replan while new observations keep arriving (see dirtyMaxWait).
const minDirtyMaxWait = 30 * time.Second

// dirtyMaxWaitDebounceFactor scales the max wait with a long configured
// debounce, so the deadline never undercuts the quiet period it bounds.
const dirtyMaxWaitDebounceFactor = 6

// dirtyMaxWait bounds how long continuous observations can postpone a
// replan. The debounce alone is trailing: each observation restarted it, so
// sustained local churn (a log written every two seconds, a long build)
// postponed every replan indefinitely, and with it every remote change the
// poller had already committed and every other local change (R-2.8.20).
func dirtyMaxWait(debounce time.Duration) time.Duration {
	return max(minDirtyMaxWait, dirtyMaxWaitDebounceFactor*debounce)
}

func newDirtyBuffer(logger *slog.Logger) *dirtyBuffer {
	return &dirtyBuffer{
		logger:           logger,
		newTimer:         newRealDebounceTimer,
		newDeadlineTimer: newRealDebounceTimer,
	}
}

func (b *dirtyBuffer) MarkDirty() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.dirty = true
	b.signalNewLocked()
}

func (b *dirtyBuffer) MarkFullRefresh() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fullRefresh = true
	b.signalNewLocked()
}

func (b *dirtyBuffer) FlushImmediate() *dirtyBatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.dirty && !b.fullRefresh {
		return nil
	}

	batch := &dirtyBatch{
		FullRefresh: b.fullRefresh,
	}
	b.dirty = false
	b.fullRefresh = false

	return batch
}

func (b *dirtyBuffer) FlushDebounced(ctx context.Context, debounce time.Duration) <-chan dirtyBatch {
	out := make(chan dirtyBatch, 1)

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

func (b *dirtyBuffer) debounceLoop(ctx context.Context, debounce time.Duration, out chan<- dirtyBatch) {
	defer close(out)

	timer := b.newTimer(debounce)
	timer.Stop()
	defer timer.Stop()

	maxWait := dirtyMaxWait(debounce)
	newDeadline := b.newDeadlineTimer
	if newDeadline == nil {
		newDeadline = newRealDebounceTimer
	}
	deadline := newDeadline(maxWait)
	deadline.Stop()
	defer deadline.Stop()

	timerActive := false
	deadlineActive := false
	stopTimer := func(t debounceTimer, active *bool) {
		if *active && !t.Stop() {
			select {
			case <-t.Chan():
			default:
			}
		}
		*active = false
	}
	flush := func() bool {
		if batch := b.FlushImmediate(); batch != nil {
			select {
			case out <- *batch:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}

	for {
		select {
		case <-ctx.Done():
			if batch := b.FlushImmediate(); batch != nil {
				select {
				case out <- *batch:
				default:
					// Debug: only reachable on shutdown; the next start
					// rescans, so the dropped batch loses nothing.
					b.logger.Debug("dirty buffer final drain discarded: output channel full",
						slog.Bool("full_refresh", batch.FullRefresh),
					)
				}
			}
			return

		case _, ok := <-b.notify:
			if !ok {
				return
			}

			stopTimer(timer, &timerActive)
			timer.Reset(debounce)
			timerActive = true

			// The deadline starts with the first signal of a batch and is
			// not restarted by later ones.
			if !deadlineActive {
				deadline.Reset(maxWait)
				deadlineActive = true
			}

		case <-timer.Chan():
			timerActive = false
			stopTimer(deadline, &deadlineActive)
			if !flush() {
				return
			}

		case <-deadline.Chan():
			deadlineActive = false
			stopTimer(timer, &timerActive)
			if !flush() {
				return
			}
		}
	}
}

func (b *dirtyBuffer) signalNewLocked() {
	if b.notify == nil {
		return
	}

	select {
	case b.notify <- struct{}{}:
	default:
	}
}
