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
func (rt *watchRuntime) armRetryTimer(ctx context.Context) {
	earliest, err := rt.engine.baseline.EarliestSyncFailureRetryAt(ctx, rt.engine.nowFunc())
	if err != nil || earliest.IsZero() {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		rt.kickRetrySweepNow()
		return
	}

	if rt.retryTimer != nil {
		rt.retryTimer.Stop()
	}
	rt.retryTimer = time.AfterFunc(delay, func() {
		rt.kickRetrySweepNow()
	})
}

func (rt *watchRuntime) stopRetryTimer() {
	if rt.retryTimer != nil {
		rt.retryTimer.Stop()
		rt.retryTimer = nil
	}
}

// kickRetrySweepNow is the single immediate wakeup path for the watch-mode
// retrier. Centralizing the non-blocking send keeps retry timer ownership
// explicit and avoids scattering direct channel writes across the engine.
func (rt *watchRuntime) kickRetrySweepNow() {
	select {
	case rt.retryTimerCh <- struct{}{}:
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetryKicked})
	default:
	}
}

// retryTimerChan returns the retry timer notification channel. Returns a nil
// channel when retryTimerCh is not initialized (one-shot mode), which blocks
// forever in a select — effectively disabling the case.
func (rt *watchRuntime) retryTimerChan() <-chan struct{} {
	return rt.retryTimerCh
}

// recordError increments the failed counter and appends the error to the
// diagnostic error list.
func (f *engineFlow) recordError(r *synctypes.WorkerResult) {
	f.failed++
	if r.Err != nil {
		f.syncErrors = append(f.syncErrors, r.Err)
	}
}

// logFailureSummary logs an aggregated summary of sync errors from the
// current pass. Groups errors by message prefix (first 80 chars) and logs
// one WARN per group with count + sample paths when count > 10, or per-item
// WARN otherwise. Mirrors the scanner aggregation pattern in
// recordSkippedItems (R-6.6.12). Resets syncErrors after logging.
func (f *engineFlow) logFailureSummary() {
	errs := f.syncErrors
	f.syncErrors = nil

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
			f.engine.logger.Warn("sync failures (aggregated)",
				slog.String("error_prefix", key),
				slog.Int("count", g.count),
				slog.Any("samples", g.msgs),
			)
		} else {
			for _, msg := range g.msgs {
				f.engine.logger.Warn("sync failure",
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
func (f *engineFlow) resultStats() (succeeded, failed int, errs []error) {
	errs = make([]error, len(f.syncErrors))
	copy(errs, f.syncErrors)
	return f.succeeded, f.failed, errs
}

// resetResultStats resets the engine-owned counters for a new pass.
func (f *engineFlow) resetResultStats() {
	f.succeeded = 0
	f.failed = 0
	f.syncErrors = nil
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all scope blocks. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the watch loop's select
// watches the old timer's channel after replacement. Called after scope blocks
// are created, trials dispatched, or trial results processed (R-2.10.5).
func (rt *watchRuntime) armTrialTimer() {
	if rt.trialTimer != nil {
		rt.trialTimer.Stop()
		rt.trialTimer = nil
	}

	earliest, ok := syncdispatch.EarliestTrialAt(rt.activeScopes)
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
	rt.trialTimer = time.AfterFunc(delay, func() {
		select {
		case rt.trialCh <- struct{}{}:
		default:
		}
	})
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (rt *watchRuntime) trialTimerChan() <-chan struct{} {
	return rt.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (rt *watchRuntime) stopTrialTimer() {
	if rt.trialTimer != nil {
		rt.trialTimer.Stop()
		rt.trialTimer = nil
	}
}
