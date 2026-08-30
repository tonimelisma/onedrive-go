// Package sync owns the single mounted content-root runtime, including scope-state helpers
// used by execution and scope-lifecycle flows.
//
// ScopeState maintains sliding windows for scope escalation. The engine
// calls UpdateScope after each action completion; when a threshold is crossed,
// it returns a ScopeUpdateResult and the engine creates a BlockScope.
//
// Detection thresholds for shared blocker activation:
//   - throttle:target:* (429) — immediate, single response
//   - service (5xx, 503+Retry-After) — 5 unique paths in 30s
//   - quota:own (507) — 3 unique paths in 10s
//
// Trial interval computation is centralized in scope lifecycle policy helpers,
// not in detection code.
//
// Type definitions (ScopeKey, ScopeKeyKind, BlockScope, ScopeUpdateResult,
// BlockScopeStore) live in the sync package and share the same ownership
// boundary as the engine and store code that use them.
package sync

import (
	"log/slog"
	"net/http"
	"time"
)

// Scope detection thresholds.
const (
	// quotaWindowThreshold is the number of unique paths that must fail with
	// 507 within quotaWindowDuration to trigger a quota block scope.
	quotaWindowThreshold = 3
	quotaWindowDuration  = 10 * time.Second

	// serviceWindowThreshold is the number of unique paths that must fail
	// with 5xx within serviceWindowDuration to
	// trigger a service block scope.
	serviceWindowThreshold = 5
	serviceWindowDuration  = 30 * time.Second
)

// scopeState maintains sliding windows for scope escalation detection and
// records successes that reset windows. Thread-safety is provided by the
// engine-owned result loop — all calls come from applyRuntimeCompletionStage on one
// goroutine.
type scopeState struct {
	windows map[ScopeKey]*slidingWindow
	nowFunc func() time.Time
	logger  *slog.Logger
}

// newScopeState creates a ScopeState with the given clock and logger.
func newScopeState(nowFunc func() time.Time, logger *slog.Logger) *scopeState {
	return &scopeState{
		windows: make(map[ScopeKey]*slidingWindow),
		nowFunc: nowFunc,
		logger:  logger,
	}
}

// UpdateScope feeds an action completion into scope detection. Returns a
// ScopeUpdateResult indicating whether a new block scope should be created.
//
// Per the sync-engine scope taxonomy and activation rules:
//   - 429 → immediate target-drive throttle block (server signal)
//   - 503 with Retry-After → immediate service block (server signal)
//   - 507 → sliding window quota:own (3 unique paths / 10s)
//   - 5xx (no Retry-After) → sliding window service (5 unique paths / 30s)
func (ss *scopeState) UpdateScope(r *actionCompletion) scopeUpdateResult {
	switch {
	case r.HTTPStatus == http.StatusTooManyRequests:
		// Immediate block — server signal, single response triggers (R-2.10.26).
		scopeKey := scopeKeyForResult(r.HTTPStatus, r.DriveID)
		if scopeKey.IsZero() {
			return scopeUpdateResult{}
		}
		return scopeUpdateResult{
			Block:         true,
			ScopeKey:      scopeKey,
			ConditionType: issueRateLimited,
			RetryAfter:    r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusServiceUnavailable && r.RetryAfter > 0:
		// Immediate block — 503 with Retry-After is a server signal (R-2.10.3).
		return scopeUpdateResult{
			Block:         true,
			ScopeKey:      SKService(),
			ConditionType: issueServiceOutage,
			RetryAfter:    r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusInsufficientStorage:
		// Quota failure — a drive-scoped quota block suppresses uploads until the
		// backing drive has space again.
		sk := scopeKeyForResult(r.HTTPStatus, r.DriveID)
		return ss.checkWindow(sk, r.Path, quotaWindowThreshold, quotaWindowDuration, IssueQuotaExceeded)

	case r.HTTPStatus >= http.StatusInternalServerError:
		// Service error — feed into service sliding window (R-2.10.28, R-2.10.29).
		sk := scopeKeyForResult(r.HTTPStatus, r.DriveID)
		return ss.checkWindow(sk, r.Path,
			serviceWindowThreshold, serviceWindowDuration,
			issueServiceOutage)

	default:
		return scopeUpdateResult{}
	}
}

// RecordSuccess resets the two global failure windows that any success
// disproves: own-quota and service availability. Per §7.3.1, "A success from
// any path in the scope shall reset the unique-path failure counter."
//
// It deliberately takes no action argument. Per-drive and per-path scopes
// (throttle, local/remote permission) are not cleared here — clearing those is
// owned by the trial admission path via ScopeAdmissionDecision.ClearScopeKey,
// which knows which specific blocked scope a trial was admitted against. Doing
// it in both places would give two owners for one transition.
func (ss *scopeState) RecordSuccess() {
	delete(ss.windows, SKQuotaOwn())
	delete(ss.windows, SKService())
}

// checkWindow adds a failure to the named sliding window and returns a
// ScopeUpdateResult indicating whether the threshold was crossed.
func (ss *scopeState) checkWindow(
	sk ScopeKey, path string, threshold int, window time.Duration,
	conditionType string,
) scopeUpdateResult {
	now := ss.nowFunc()

	w, ok := ss.windows[sk]
	if !ok {
		w = &slidingWindow{
			window:    window,
			threshold: threshold,
		}
		ss.windows[sk] = w
	}

	triggered := w.add(path, now)
	if triggered {
		ss.logger.Info("scope threshold crossed",
			slog.String("scope_key", sk.String()),
			slog.Int("unique_paths", w.uniqueCount(now)),
		)
		// Reset window after triggering to avoid re-triggering on next failure.
		delete(ss.windows, sk)
		return scopeUpdateResult{
			Block:         true,
			ScopeKey:      sk,
			ConditionType: conditionType,
		}
	}

	return scopeUpdateResult{}
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
