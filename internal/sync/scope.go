// Package sync owns the single-drive runtime, including scope-state helpers
// used by execution and recovery flows.
//
// ScopeState maintains sliding windows for scope escalation. The engine
// calls UpdateScope after each worker result; when a threshold is crossed,
// it returns a ScopeUpdateResult and the engine creates a syncstore.ScopeBlock.
//
// Detection thresholds (failure-redesign.md §7.3.1):
//   - throttle:target:* (429) — immediate, single response
//   - service (5xx, 503+Retry-After) — 5 unique paths in 30s
//   - quota:own (507, own-drive) — 3 unique paths in 10s
//   - quota:shortcut:$key (507, shortcut) — 3 unique paths in 10s
//
// Trial interval computation is centralized in computeTrialInterval()
// (engine.go), not here. See R-2.10.14.
//
// Type definitions (ScopeKey, ScopeKeyKind, syncstore.ScopeBlock, ScopeUpdateResult,
// ScopeBlockStore) are in synctypes and re-exported via types.go.
package sync

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Scope detection thresholds.
const (
	// quotaWindowThreshold is the number of unique paths that must fail with
	// 507 within quotaWindowDuration to trigger a quota scope block.
	quotaWindowThreshold = 3
	quotaWindowDuration  = 10 * time.Second

	// serviceWindowThreshold is the number of unique paths that must fail
	// with 5xx within serviceWindowDuration to
	// trigger a service scope block.
	serviceWindowThreshold = 5
	serviceWindowDuration  = 30 * time.Second
)

// Unified trial timing constants (R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.14).
// All scope types share the same initial interval and cap. Server-provided
// Retry-After values bypass the cap entirely (server is ground truth).
const (
	DefaultInitialTrialInterval = 5 * time.Second
	DefaultMaxTrialInterval     = 5 * time.Minute
)

// ScopeState maintains sliding windows for scope escalation detection and
// records successes that reset windows. Thread-safety is provided by the
// engine-owned result loop — all calls come from processWorkerResult on one
// goroutine.
type ScopeState struct {
	windows map[synctypes.ScopeKey]*slidingWindow
	nowFunc func() time.Time
	logger  *slog.Logger
}

// NewScopeState creates a ScopeState with the given clock and logger.
func NewScopeState(nowFunc func() time.Time, logger *slog.Logger) *ScopeState {
	return &ScopeState{
		windows: make(map[synctypes.ScopeKey]*slidingWindow),
		nowFunc: nowFunc,
		logger:  logger,
	}
}

// UpdateScope feeds a worker result into scope detection. Returns a
// ScopeUpdateResult indicating whether a new scope block should be created.
//
// Per R-2.10.3 and failure-redesign.md §7.3.1:
//   - 429 → immediate target-scoped throttle block (server signal)
//   - 503 with Retry-After → immediate service block (server signal)
//   - 507 own-drive → sliding window quota:own (3 unique paths / 10s)
//   - 507 shortcut → sliding window quota:shortcut:$key (3 unique paths / 10s)
//   - 5xx (no Retry-After) → sliding window service (5 unique paths / 30s)
func (ss *ScopeState) UpdateScope(r *WorkerResult) synctypes.ScopeUpdateResult {
	targetDriveID := r.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = r.DriveID
	}

	switch {
	case r.HTTPStatus == http.StatusTooManyRequests:
		// Immediate block — server signal, single response triggers (R-2.10.26).
		scopeKey := synctypes.ScopeKeyForResult(r.HTTPStatus, targetDriveID, r.ShortcutKey)
		if scopeKey.IsZero() {
			return synctypes.ScopeUpdateResult{}
		}
		return synctypes.ScopeUpdateResult{
			Block:      true,
			ScopeKey:   scopeKey,
			IssueType:  synctypes.IssueRateLimited,
			RetryAfter: r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusServiceUnavailable && r.RetryAfter > 0:
		// Immediate block — 503 with Retry-After is a server signal (R-2.10.3).
		return synctypes.ScopeUpdateResult{
			Block:      true,
			ScopeKey:   synctypes.SKService(),
			IssueType:  synctypes.IssueServiceOutage,
			RetryAfter: r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusInsufficientStorage:
		// Quota failure — scope depends on target drive (R-2.10.1, R-2.10.17).
		sk := synctypes.ScopeKeyForResult(r.HTTPStatus, targetDriveID, r.ShortcutKey)
		return ss.checkWindow(sk, r.Path, quotaWindowThreshold, quotaWindowDuration, synctypes.IssueQuotaExceeded)

	case r.HTTPStatus >= http.StatusInternalServerError:
		// Service error — feed into service sliding window (R-2.10.28, R-2.10.29).
		sk := synctypes.ScopeKeyForResult(r.HTTPStatus, targetDriveID, r.ShortcutKey)
		return ss.checkWindow(sk, r.Path,
			serviceWindowThreshold, serviceWindowDuration,
			synctypes.IssueServiceOutage)

	default:
		return synctypes.ScopeUpdateResult{}
	}
}

// RecordSuccess resets the sliding window for scopes relevant to the
// successful action. Per §7.3.1: "A success from any path in the scope
// shall reset the unique-path failure counter."
func (ss *ScopeState) RecordSuccess(r *WorkerResult) {
	// Success resets all potentially-relevant windows for this action's scope.
	if r.ShortcutKey != "" {
		delete(ss.windows, synctypes.SKQuotaShortcut(r.ShortcutKey))
	} else {
		delete(ss.windows, synctypes.SKQuotaOwn())
	}
	// Also reset service window — a successful request proves the service is up.
	delete(ss.windows, synctypes.SKService())
}

// checkWindow adds a failure to the named sliding window and returns a
// ScopeUpdateResult indicating whether the threshold was crossed.
func (ss *ScopeState) checkWindow(
	sk synctypes.ScopeKey, path string, threshold int, window time.Duration,
	issueType string,
) synctypes.ScopeUpdateResult {
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
		return synctypes.ScopeUpdateResult{
			Block:     true,
			ScopeKey:  sk,
			IssueType: issueType,
		}
	}

	return synctypes.ScopeUpdateResult{}
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
