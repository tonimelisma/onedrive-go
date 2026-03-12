// scope.go — Scope-level failure detection and blocking for the sync engine.
//
// ScopeState maintains sliding windows for scope escalation and active scope
// blocks. The engine calls updateScope after each worker result; when a
// threshold is crossed, the engine creates a ScopeBlock and tells the tracker
// to hold affected actions.
//
// Scope types and their detection thresholds (failure-redesign.md §7.3.1):
//   - throttle:account (429) — immediate, single response triggers block
//   - service (5xx, 503+Retry-After, 400+outage) — 5 unique paths in 30s
//   - quota:own (507, own-drive) — 3 unique paths in 10s
//   - quota:shortcut:$key (507, shortcut) — 3 unique paths in 10s
//
// Trial timing constants (R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.14):
//   - rate_limited: starts at Retry-After, 2× backoff, max 10min
//   - quota_exceeded: 5min start, 2× backoff, max 1h
//   - service_outage: 60s start (or Retry-After), 2× backoff, max 10min
package sync

import (
	"log/slog"
	"net/http"
	"time"
)

// Scope key constants used throughout scope detection and classification.
const (
	scopeKeyThrottleAccount = "throttle:account"
	scopeKeyQuotaOwn        = "quota:own"
	scopeKeyQuotaShortcut   = "quota:shortcut:"
	scopeKeyService         = "service"
)

// Scope detection thresholds.
const (
	// quotaWindowThreshold is the number of unique paths that must fail with
	// 507 within quotaWindowDuration to trigger a quota scope block.
	quotaWindowThreshold = 3
	quotaWindowDuration  = 10 * time.Second

	// serviceWindowThreshold is the number of unique paths that must fail
	// with 5xx (or 400 outage pattern) within serviceWindowDuration to
	// trigger a service scope block.
	serviceWindowThreshold = 5
	serviceWindowDuration  = 30 * time.Second
)

// Trial timing constants per scope type.
// Initial intervals and max caps per R-2.10.6/R-2.10.7/R-2.10.8/R-2.10.14.
const (
	quotaInitialInterval   = 5 * time.Minute
	serviceInitialInterval = 60 * time.Second

	// Per-scope-type maximum trial intervals (R-2.10.6, R-2.10.8, R-2.10.14).
	quotaMaxTrialInterval     = 1 * time.Hour
	serviceMaxTrialInterval   = 10 * time.Minute
	rateLimitMaxTrialInterval = 10 * time.Minute
)

// maxTrialIntervalForIssueType returns the maximum trial interval for the
// given scope issue type. Used by handleTrialResult to cap backoff (R-2.10.14).
func maxTrialIntervalForIssueType(issueType string) time.Duration {
	switch issueType {
	case "quota_exceeded":
		return quotaMaxTrialInterval
	case "rate_limited":
		return rateLimitMaxTrialInterval
	case "service_outage":
		return serviceMaxTrialInterval
	default:
		return serviceMaxTrialInterval // safe default
	}
}

// ScopeState maintains sliding windows for scope escalation detection and
// records successes that reset windows. Thread-safety is provided by the
// engine's single-goroutine drain loop — all calls come from
// processWorkerResult which runs on one goroutine.
type ScopeState struct {
	windows map[string]*slidingWindow
	nowFunc func() time.Time
	logger  *slog.Logger
}

// NewScopeState creates a ScopeState with the given clock and logger.
func NewScopeState(nowFunc func() time.Time, logger *slog.Logger) *ScopeState {
	return &ScopeState{
		windows: make(map[string]*slidingWindow),
		nowFunc: nowFunc,
		logger:  logger,
	}
}

// ScopeUpdateResult describes the outcome of updateScope: whether a new scope
// block should be created.
type ScopeUpdateResult struct {
	Block         bool          // true if threshold crossed → create block
	ScopeKey      string        // scope key for the block
	IssueType     string        // "rate_limited", "quota_exceeded", "service_outage"
	TrialInterval time.Duration // initial trial interval for the block
}

// UpdateScope feeds a worker result into scope detection. Returns a
// ScopeUpdateResult indicating whether a new scope block should be created.
//
// Per R-2.10.3 and failure-redesign.md §7.3.1:
//   - 429 → immediate throttle:account block (server signal)
//   - 503 with Retry-After → immediate service block (server signal)
//   - 507 own-drive → sliding window quota:own (3 unique paths / 10s)
//   - 507 shortcut → sliding window quota:shortcut:$key (3 unique paths / 10s)
//   - 5xx (no Retry-After) → sliding window service (5 unique paths / 30s)
//   - 400 + outage pattern → sliding window service (same as 5xx)
func (ss *ScopeState) UpdateScope(r *WorkerResult) ScopeUpdateResult {
	switch {
	case r.HTTPStatus == http.StatusTooManyRequests:
		// Immediate block — server signal, single response triggers (R-2.10.26).
		interval := r.RetryAfter
		if interval <= 0 {
			interval = serviceInitialInterval // fallback
		}
		return ScopeUpdateResult{
			Block:         true,
			ScopeKey:      scopeKeyThrottleAccount,
			IssueType:     "rate_limited",
			TrialInterval: interval,
		}

	case r.HTTPStatus == http.StatusServiceUnavailable && r.RetryAfter > 0:
		// Immediate block — 503 with Retry-After is a server signal (R-2.10.3).
		return ScopeUpdateResult{
			Block:         true,
			ScopeKey:      scopeKeyService,
			IssueType:     "service_outage",
			TrialInterval: r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusInsufficientStorage:
		// Quota failure — scope depends on target drive (R-2.10.1, R-2.10.17).
		scopeKey := scopeKeyQuotaOwn
		if r.ShortcutKey != "" {
			scopeKey = scopeKeyQuotaShortcut + r.ShortcutKey
		}
		return ss.checkWindow(scopeKey, r.Path, quotaWindowThreshold, quotaWindowDuration, "quota_exceeded", quotaInitialInterval)

	case r.HTTPStatus >= http.StatusInternalServerError:
		// Service error — feed into service sliding window (R-2.10.28, R-2.10.29).
		return ss.checkWindow(scopeKeyService, r.Path, serviceWindowThreshold, serviceWindowDuration, "service_outage", serviceInitialInterval)

	default:
		return ScopeUpdateResult{}
	}
}

// UpdateScopeOutagePattern feeds a 400 outage-pattern result into the
// service sliding window. Called separately from UpdateScope because
// outage patterns are classified as resultRequeue (not resultScopeBlock)
// by classifyResult but still need to feed scope detection.
func (ss *ScopeState) UpdateScopeOutagePattern(path string) ScopeUpdateResult {
	return ss.checkWindow(scopeKeyService, path, serviceWindowThreshold, serviceWindowDuration, "service_outage", serviceInitialInterval)
}

// RecordSuccess resets the sliding window for scopes relevant to the
// successful action. Per §7.3.1: "A success from any path in the scope
// shall reset the unique-path failure counter."
func (ss *ScopeState) RecordSuccess(r *WorkerResult) {
	// Success resets all potentially-relevant windows for this action's scope.
	if r.ShortcutKey != "" {
		delete(ss.windows, scopeKeyQuotaShortcut+r.ShortcutKey)
	} else {
		delete(ss.windows, scopeKeyQuotaOwn)
	}
	// Also reset service window — a successful request proves the service is up.
	delete(ss.windows, scopeKeyService)
}

// checkWindow adds a failure to the named sliding window and returns a
// ScopeUpdateResult indicating whether the threshold was crossed.
func (ss *ScopeState) checkWindow(
	scopeKey, path string, threshold int, window time.Duration,
	issueType string, initialInterval time.Duration,
) ScopeUpdateResult {
	now := ss.nowFunc()

	w, ok := ss.windows[scopeKey]
	if !ok {
		w = &slidingWindow{
			window:    window,
			threshold: threshold,
		}
		ss.windows[scopeKey] = w
	}

	triggered := w.add(path, now)
	if triggered {
		ss.logger.Info("scope threshold crossed",
			slog.String("scope_key", scopeKey),
			slog.Int("unique_paths", w.uniqueCount(now)),
		)
		// Reset window after triggering to avoid re-triggering on next failure.
		delete(ss.windows, scopeKey)
		return ScopeUpdateResult{
			Block:         true,
			ScopeKey:      scopeKey,
			IssueType:     issueType,
			TrialInterval: initialInterval,
		}
	}

	return ScopeUpdateResult{}
}

// slidingWindow tracks unique failed paths within a time window for
// scope escalation detection.
type slidingWindow struct {
	entries   []windowEntry
	window    time.Duration
	threshold int
}

type windowEntry struct {
	path string
	at   time.Time
}

// add records a failure at the given path and time. Returns true if the
// unique path count within the window crossed the threshold.
func (sw *slidingWindow) add(path string, now time.Time) bool {
	// Expire old entries.
	cutoff := now.Add(-sw.window)
	fresh := 0
	for _, e := range sw.entries {
		if e.at.After(cutoff) {
			sw.entries[fresh] = e
			fresh++
		}
	}
	sw.entries = sw.entries[:fresh]

	// Add the new entry.
	sw.entries = append(sw.entries, windowEntry{path: path, at: now})

	// Count unique paths.
	return sw.uniqueCount(now) >= sw.threshold
}

// uniqueCount returns the number of unique paths in the window.
func (sw *slidingWindow) uniqueCount(now time.Time) int {
	cutoff := now.Add(-sw.window)
	seen := make(map[string]struct{})
	for _, e := range sw.entries {
		if e.at.After(cutoff) {
			seen[e.path] = struct{}{}
		}
	}
	return len(seen)
}
