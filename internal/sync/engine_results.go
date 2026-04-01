package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// armRetryTimer arms the retry timer for the next retrier sweep. Queries
// the earliest next_retry_at from sync_failures and sets the timer. If the
// retry timer channel is already signaled (non-blocking send to buffered(1)
// channel), the next owning loop iteration processes it.
func (e *Engine) armRetryTimer(ctx context.Context) {
	if e.watch == nil {
		return
	}

	earliest, err := e.baseline.EarliestSyncFailureRetryAt(ctx, e.nowFunc())
	if err != nil || earliest.IsZero() {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		e.kickRetrySweepNow()
		return
	}

	if e.watch.retryTimer != nil {
		e.watch.retryTimer.Stop()
	}
	e.watch.retryTimer = time.AfterFunc(delay, func() {
		e.kickRetrySweepNow()
	})
}

func (e *Engine) stopRetryTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.retryTimer != nil {
		e.watch.retryTimer.Stop()
		e.watch.retryTimer = nil
	}
}

// kickRetrySweepNow is the single immediate wakeup path for the watch-mode
// retrier. Centralizing the non-blocking send keeps retry timer ownership
// explicit and avoids scattering direct channel writes across the engine.
func (e *Engine) kickRetrySweepNow() {
	if e.watch == nil {
		return
	}

	select {
	case e.watch.retryTimerCh <- struct{}{}:
		e.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetryKicked})
	default:
	}
}

// retryTimerChan returns the retry timer notification channel. Returns a nil
// channel when retryTimerCh is not initialized (one-shot mode), which blocks
// forever in a select — effectively disabling the case.
func (e *Engine) retryTimerChan() <-chan struct{} {
	if e.watch == nil {
		return nil // nil channel blocks in select — disables retry case
	}

	return e.watch.retryTimerCh
}

// recordError increments the failed counter and appends the error to the
// diagnostic error list.
func (e *Engine) recordError(r *synctypes.WorkerResult) {
	e.failed++
	if r.Err != nil {
		e.syncErrors = append(e.syncErrors, r.Err)
	}
}

// logFailureSummary logs an aggregated summary of sync errors from the
// current pass. Groups errors by message prefix (first 80 chars) and logs
// one WARN per group with count + sample paths when count > 10, or per-item
// WARN otherwise. Mirrors the scanner aggregation pattern in
// recordSkippedItems (R-6.6.12). Resets syncErrors after logging.
func (e *Engine) logFailureSummary() {
	errs := e.syncErrors
	e.syncErrors = nil

	if len(errs) == 0 {
		return
	}

	// Group by error message for aggregation. Use the first errorGroupKeyLen
	// chars of the error message as the group key — detailed enough to
	// distinguish issue types without creating too many groups.
	const errorGroupKeyLen = 80
	type group struct {
		msgs  []string
		count int
	}
	groups := make(map[string]*group)
	for _, err := range errs {
		msg := err.Error()
		key := msg
		if len(key) > errorGroupKeyLen {
			key = key[:errorGroupKeyLen]
		}
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
		}
		g.count++
		// Keep first 3 unique messages as samples.
		const sampleCount = 3
		if len(g.msgs) < sampleCount {
			g.msgs = append(g.msgs, msg)
		}
	}

	const aggregateThreshold = 10
	for key, g := range groups {
		if g.count > aggregateThreshold {
			e.logger.Warn("sync failures (aggregated)",
				slog.String("error_prefix", key),
				slog.Int("count", g.count),
				slog.Any("samples", g.msgs),
			)
		} else {
			for _, msg := range g.msgs {
				e.logger.Warn("sync failure",
					slog.String("error", msg),
				)
			}
		}
	}
}

// nowFunc returns the current time from the engine's injectable clock.
// Always set by NewEngine; tests overwrite with a controllable clock.
func (e *Engine) nowFunc() time.Time {
	return e.nowFn()
}

// resultStats returns the engine-owned counters and error list.
func (e *Engine) resultStats() (succeeded, failed int, errs []error) {
	errs = make([]error, len(e.syncErrors))
	copy(errs, e.syncErrors)
	return e.succeeded, e.failed, errs
}

// resetResultStats resets the engine-owned counters for a new pass.
func (e *Engine) resetResultStats() {
	e.succeeded = 0
	e.failed = 0
	e.syncErrors = nil
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all scope blocks. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the watch loop's select
// watches the old timer's channel after replacement. Called after scope blocks
// are created, trials dispatched, or trial results processed (R-2.10.5).
func (e *Engine) armTrialTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}

	earliest, ok := syncdispatch.EarliestTrialAt(e.watch.activeScopes)
	if !ok {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		delay = 1 * time.Millisecond // fire immediately
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// the watch loop calls DueTrials, so even if a second AfterFunc
	// fires while a signal is pending, all due scopes are still processed
	// on the next loop iteration.
	e.watch.trialTimer = time.AfterFunc(delay, func() {
		select {
		case e.trialCh <- struct{}{}:
		default:
		}
	})
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (e *Engine) trialTimerChan() <-chan struct{} {
	return e.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (e *Engine) stopTrialTimer() {
	if e.watch == nil {
		return
	}

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}
}
