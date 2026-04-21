package sync

import (
	"log/slog"
	"sort"
	"time"
)

type failureSummaryEntry struct {
	issueType string
	path      string
	errMsg    string
}

const fallbackFailureSummaryIssueType = "transient_failure"

// armRetryTimer arms the retry timer for the next retrier sweep. Queries
// the earliest held retry deadline from the current runtime and sets the timer. If the
// retry timer channel is already signaled (non-blocking send to buffered(1)
// channel), the next owning loop iteration processes it.
func (rt *watchRuntime) armRetryTimer() {
	earliest, ok := rt.earliestHeldRetryAt()
	if !ok {
		rt.resetRetryTimer(nil)
		return
	}

	delay := earliest.Sub(rt.engine.nowFunc())
	if delay <= 0 {
		rt.kickRetrySweepNow()
		return
	}

	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventRetryTimerArmed,
		Delay: delay,
	})
	rt.resetRetryTimer(rt.engine.afterFunc(delay, func() {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetryTimerFired})
		rt.kickRetrySweepNow()
	}))
}

func (rt *watchRuntime) armHeldTimers() {
	rt.armRetryTimer()
	rt.armTrialTimer()
}

func (flow *engineFlow) earliestHeldRetryAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, held := range flow.heldByKey {
		if held == nil || held.Reason != heldReasonRetry || held.NextRetry.IsZero() {
			continue
		}
		if !found || held.NextRetry.Before(earliest) {
			earliest = held.NextRetry
			found = true
		}
	}

	return earliest, found
}

func (rt *watchRuntime) earliestHeldRetryAt() (time.Time, bool) {
	return rt.engineFlow.earliestHeldRetryAt()
}

func (rt *watchRuntime) stopRetryTimer() {
	rt.resetRetryTimer(nil)
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

// recordError increments the failed counter, preserves the raw error for the
// final SyncReport, and records the classified transient-failure shape needed
// for end-of-pass WARN aggregation.
func (f *engineFlow) recordError(decision *ResultDecision, r *ActionCompletion) {
	f.failed++
	if r == nil {
		return
	}

	if r.Err != nil {
		f.syncErrors = append(f.syncErrors, r.Err)
	}

	if decision == nil || decision.Persistence != persistRetryWork {
		return
	}

	errMsg := r.ErrMsg
	if errMsg == "" && r.Err != nil {
		errMsg = r.Err.Error()
	}
	if errMsg == "" {
		return
	}

	conditionType := decision.ConditionType
	if conditionType == "" {
		conditionType = fallbackFailureSummaryIssueType
	}

	f.summaries = append(f.summaries, failureSummaryEntry{
		issueType: conditionType,
		path:      r.Path,
		errMsg:    errMsg,
	})
}

// logFailureSummary logs an aggregated summary of transient execution failures
// from the current pass. Groups failures by condition_type and mirrors the
// scanner-time aggregation rule: >10 items produce one WARN summary plus
// per-item DEBUG detail, otherwise each item gets its own WARN.
func (f *engineFlow) logFailureSummary() {
	summaries := f.summaries
	f.summaries = nil

	if len(summaries) == 0 {
		return
	}

	type group struct {
		items []failureSummaryEntry
		count int
		paths []string
	}
	groups := make(map[string]*group, len(summaries))
	for _, summary := range summaries {
		key := summary.issueType
		if key == "" {
			key = fallbackFailureSummaryIssueType
		}
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
		}
		g.count++
		g.items = append(g.items, summary)
		if summary.path != "" {
			const sampleCount = 3
			if len(g.paths) < sampleCount {
				g.paths = append(g.paths, summary.path)
			}
		}
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	const aggregateThreshold = 10
	for _, key := range keys {
		g := groups[key]
		if g.count > aggregateThreshold {
			f.engine.logger.Warn("sync retry work (aggregated)",
				slog.String("condition_type", key),
				slog.Int("count", g.count),
				slog.Any("sample_paths", g.paths),
			)
			for _, item := range g.items {
				f.engine.logger.Debug("sync retry work",
					slog.String("condition_type", key),
					slog.String("path", item.path),
					slog.String("error", item.errMsg),
				)
			}
		} else {
			for _, item := range g.items {
				f.engine.logger.Warn("sync retry work",
					slog.String("condition_type", key),
					slog.String("path", item.path),
					slog.String("error", item.errMsg),
				)
			}
		}
	}
}

// nowFunc returns the current time from the engine's injectable clock.
// Always set by NewEngine; tests overwrite with a controllable clock.
func (e *Engine) nowFunc() time.Time {
	if e == nil || e.nowFn == nil {
		panic("sync: engine clock is not initialized")
	}

	return e.nowFn()
}

func (e *Engine) since(start time.Time) time.Duration {
	return e.nowFunc().Sub(start)
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
	f.summaries = nil
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all block scopes. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the watch loop's select
// watches the old timer's channel after replacement. Called after block scopes
// are created, trials dispatched, or trial results processed (R-2.10.5).
func (rt *watchRuntime) armTrialTimer() {
	earliest, ok := rt.earliestTrialAt()
	if !ok {
		rt.resetTrialTimer(nil)
		return
	}

	delay := earliest.Sub(rt.engine.nowFunc())
	if delay <= 0 {
		delay = 1 * time.Millisecond // fire immediately
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// the watch loop calls DueTrials, so even if a second AfterFunc
	// fires while a signal is pending, all due scopes are still processed
	// on the next loop iteration.
	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventTrialTimerArmed,
		Delay: delay,
	})
	rt.resetTrialTimer(rt.engine.afterFunc(delay, func() {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventTrialTimerFired})
		select {
		case rt.trialCh <- struct{}{}:
		default:
		}
	}))
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (rt *watchRuntime) trialTimerChan() <-chan struct{} {
	return rt.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (rt *watchRuntime) stopTrialTimer() {
	rt.resetTrialTimer(nil)
}
