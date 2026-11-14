package sync

import (
	"log/slog"
	"sort"
	"time"
)

// failureSummaryEntry is one failure of a pass that was kept as retry work,
// as the end-of-pass summary groups it.
type failureSummaryEntry struct {
	// group is the summary group and the condition_type log value: the
	// decision's condition, or noConditionSummaryGroup.
	group  string
	path   string
	errMsg string
}

// noConditionSummaryGroup is the summary group, and the condition_type log
// value, of retry work whose failure names no condition; the store keeps such
// a row as a transient failure. It is a log value, not an IssueType: no
// member has it and no store holds it.
const noConditionSummaryGroup = "transient_failure"

// overdueTrialDelay is the wait armed for a trial that is already due: it
// fires at once, through the same timer and channel as every other trial.
const overdueTrialDelay = time.Millisecond

// armRetryTimer arms the retry timer for the next held-retry release. It
// queries the earliest held retry deadline from the current runtime and sets
// the timer. If the retry timer channel is already signaled (non-blocking send
// to buffered(1) channel), the next owning loop iteration processes it.
func (rt *watchRuntime) armRetryTimer() {
	earliest, ok := rt.retries.earliestHeldRetryAt()
	if !ok {
		rt.resetRetryTimer(nil)
		return
	}

	delay := earliest.Sub(rt.deps.now())
	if delay <= 0 {
		rt.kickRetryHeldReleaseNow()
		return
	}

	rt.deps.emit(engineDebugEvent{
		Type:  engineDebugEventRetryTimerArmed,
		Delay: delay,
	})
	rt.resetRetryTimer(rt.deps.clock.AfterFunc(delay, func() {
		rt.deps.emit(engineDebugEvent{Type: engineDebugEventRetryTimerFired})
		rt.kickRetryHeldReleaseNow()
	}))
}

func (rt *watchRuntime) armHeldTimers() {
	rt.armRetryTimer()
	rt.armTrialTimer()
}

func (rt *watchRuntime) stopRetryTimer() {
	rt.resetRetryTimer(nil)
}

// kickRetryHeldReleaseNow is the single immediate wakeup path for the
// watch-mode held-retry release loop. Centralizing the non-blocking send keeps
// retry timer ownership explicit and avoids scattering direct channel writes
// across the engine.
func (rt *watchRuntime) kickRetryHeldReleaseNow() {
	select {
	case rt.timers.retryTimerCh <- struct{}{}:
		rt.deps.emit(engineDebugEvent{Type: engineDebugEventRetryKicked})
	default:
	}
}

// retryTimerChan returns the retry timer notification channel. Returns a nil
// channel when retryTimerCh is not initialized (one-shot mode), which blocks
// forever in a select — effectively disabling the case.
func (rt *watchRuntime) retryTimerChan() <-chan struct{} {
	return rt.timers.retryTimerCh
}

// recordError increments the failed counter, and the kept-for-retry counter
// for a failure the engine retries by itself (failureKeptForRetry), preserves
// the raw error for the final SyncReport, and records a failure kept as retry
// work for the end-of-pass summary (logFailureSummary).
func (f *engineFlow) recordError(decision *resultDecision, r *actionCompletion) {
	f.results.failed++
	if decision != nil && failureKeptForRetry(decision.Class) {
		f.results.keptForRetry++
	}
	if r == nil {
		return
	}

	if r.Err != nil {
		f.results.errors = append(f.results.errors, r.Err)
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

	// The summary groups log lines by condition, so it keeps the condition as
	// the log value it becomes, with a group of its own for a failure that has
	// none.
	group := string(decision.ConditionType)
	if group == "" {
		group = noConditionSummaryGroup
	}

	f.results.summaries = append(f.results.summaries, failureSummaryEntry{
		group:  group,
		path:   r.Path,
		errMsg: errMsg,
	})
}

// logFailureSummary logs an aggregated summary of the current pass's failures
// kept as retry work. Groups failures by condition_type: >10 items produce one
// INFO summary plus per-item DEBUG detail, otherwise each item is logged at
// DEBUG. The user-facing report of each failed file is the sync console's
// activity line (R-6.6.18), which shows failures that need the user at
// default verbosity and retried ones only with --verbose; a WARN here would
// print the same failure twice and expose retries the console hides.
func (f *engineFlow) logFailureSummary() {
	summaries := f.results.summaries
	f.results.summaries = nil

	if len(summaries) == 0 {
		return
	}

	type summaryGroup struct {
		items       []failureSummaryEntry
		samplePaths []string
	}
	const sampleCount = 3
	groups := make(map[string]*summaryGroup, len(summaries))
	for _, summary := range summaries {
		g, ok := groups[summary.group]
		if !ok {
			g = &summaryGroup{}
			groups[summary.group] = g
		}
		g.items = append(g.items, summary)
		if summary.path != "" && len(g.samplePaths) < sampleCount {
			g.samplePaths = append(g.samplePaths, summary.path)
		}
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	const aggregateThreshold = 10
	for _, name := range names {
		g := groups[name]
		if len(g.items) > aggregateThreshold {
			f.engine.logger.Info("Some files will be retried on the next sync",
				slog.String("condition_type", name),
				slog.Int("count", len(g.items)),
				slog.Any("sample_paths", g.samplePaths),
			)
		}
		for _, item := range g.items {
			f.engine.logger.Debug("sync retry work",
				slog.String("condition_type", name),
				slog.String("path", item.path),
				slog.String("error", item.errMsg),
			)
		}
	}
}

// nowFunc returns the current time from the engine's clock capability.
// Always set by newEngine; tests install a manual clock.
func (e *Engine) nowFunc() time.Time {
	if e == nil || e.clock == nil {
		panic("sync: engine clock is not initialized")
	}

	return e.clock.Now()
}

func (e *Engine) since(start time.Time) time.Duration {
	return e.nowFunc().Sub(start)
}

// workerPoolSize is how many workers this engine's pools run: the configured
// transfer workers with the pool's floor, or one when serialized (D-090). It
// is the ceiling a fresh plan's slow start doubles toward (R-6.8.25).
func (e *Engine) workerPoolSize() int {
	return poolWorkerCount(e.transferWorkers, e.serializeWorkers)
}

// resultStats returns the engine-owned counters and error list.
func (f *engineFlow) resultStats() (succeeded, failed int, errs []error) {
	errs = make([]error, len(f.results.errors))
	copy(errs, f.results.errors)
	return f.results.succeeded, f.results.failed, errs
}

// recordPassCounts fills a pass's report from the engine-owned counters.
// Every end of a one-shot pass reports through it (or, for a plan that never
// ran, through Report.setPassCounts directly), so each one holds the same
// LeftForNextSync: the planned actions neither completed nor failed.
func (f *engineFlow) recordPassCounts(report *Report, actions int) {
	succeeded, failed, errs := f.resultStats()
	report.setPassCounts(actions, passCounts{
		succeeded:    succeeded,
		failed:       failed,
		keptForRetry: f.results.keptForRetry,
		superseded:   f.supersededCount(),
		errors:       errs,
	})
}

// supersededCount is how many actions this pass gave up on as superseded.
func (f *engineFlow) supersededCount() int {
	return f.results.superseded
}

// resetResultStats resets the engine-owned counters for a new pass.
func (f *engineFlow) resetResultStats() {
	f.results.succeeded = 0
	f.results.failed = 0
	f.results.keptForRetry = 0
	f.results.superseded = 0
	f.results.errors = nil
	f.results.summaries = nil
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

	delay := earliest.Sub(rt.deps.now())
	if delay <= 0 {
		delay = overdueTrialDelay
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// the watch loop calls DueTrials, so even if a second AfterFunc
	// fires while a signal is pending, all due scopes are still processed
	// on the next loop iteration.
	rt.deps.emit(engineDebugEvent{
		Type:  engineDebugEventTrialTimerArmed,
		Delay: delay,
	})
	rt.resetTrialTimer(rt.deps.clock.AfterFunc(delay, func() {
		rt.deps.emit(engineDebugEvent{Type: engineDebugEventTrialTimerFired})
		select {
		case rt.timers.trialCh <- struct{}{}:
		default:
		}
	}))
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (rt *watchRuntime) trialTimerChan() <-chan struct{} {
	return rt.timers.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (rt *watchRuntime) stopTrialTimer() {
	rt.resetTrialTimer(nil)
}
