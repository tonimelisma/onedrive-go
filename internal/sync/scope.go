// scope.go — Scope-level failure detection for the sync engine.
//
// ScopeState maintains sliding windows for scope escalation. The engine
// calls UpdateScope after each worker result; when a threshold is crossed,
// it returns a ScopeUpdateResult and the engine creates a ScopeBlock.
//
// Detection thresholds (failure-redesign.md §7.3.1):
//   - throttle:account (429) — immediate, single response
//   - service (5xx, 503+Retry-After, 400+outage) — 5 unique paths in 30s
//   - quota:own (507, own-drive) — 3 unique paths in 10s
//   - quota:shortcut:$key (507, shortcut) — 3 unique paths in 10s
//
// Trial interval computation is centralized in computeTrialInterval()
// (engine.go), not here. See R-2.10.14.
package sync

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ScopeKey — typed scope key replacing raw string keys
// ---------------------------------------------------------------------------

// ScopeKeyKind discriminates the type of scope block. Value-typed (usable
// as map key), exhaustive via switch. Zero value is invalid by construction.
type ScopeKeyKind int

const (
	ScopeThrottleAccount ScopeKeyKind = iota + 1 // no Param
	ScopeService                                 // no Param
	ScopeQuotaOwn                                // no Param
	ScopeQuotaShortcut                           // Param = "remoteDrive:remoteItem"
	ScopePermDir                                 // Param = relative directory path
	ScopeDiskLocal                               // no Param
)

// ScopeKey identifies a scope block. The Kind discriminator determines the
// semantics; Param carries per-instance data for parameterized scopes
// (ScopeQuotaShortcut, ScopePermDir). Comparable, so usable as a map key.
type ScopeKey struct {
	Kind  ScopeKeyKind
	Param string
}

// Pre-built scope keys for non-parameterized scopes. Use these instead of
// constructing ScopeKey{Kind: ...} literals for readability.
var (
	SKThrottleAccount = ScopeKey{Kind: ScopeThrottleAccount}
	SKService         = ScopeKey{Kind: ScopeService}
	SKQuotaOwn        = ScopeKey{Kind: ScopeQuotaOwn}
	SKDiskLocal       = ScopeKey{Kind: ScopeDiskLocal}
)

// SKQuotaShortcut returns the scope key for a shortcut quota block.
func SKQuotaShortcut(compositeKey string) ScopeKey {
	return ScopeKey{Kind: ScopeQuotaShortcut, Param: compositeKey}
}

// SKPermDir returns the scope key for a local directory permission block.
func SKPermDir(dirPath string) ScopeKey {
	return ScopeKey{Kind: ScopePermDir, Param: dirPath}
}

// IsZero returns true for the zero-value ScopeKey (Kind == 0).
func (sk ScopeKey) IsZero() bool {
	return sk.Kind == 0
}

// Wire-format strings for scope keys stored in SQLite scope_key columns.
// Used by String() and ParseScopeKey() — the only serialization boundary.
const (
	wireThrottleAccount = "throttle:account"
	wireService         = "service"
	wireQuotaOwn        = "quota:own"
	wireQuotaShortcut   = "quota:shortcut:" // prefix for parameterized key
	wirePermDir         = "perm:dir:"       // prefix for parameterized key
	wireDiskLocal       = "disk:local"
)

// String serializes to the wire format stored in SQLite scope_key columns.
// ParseScopeKey is the inverse.
func (sk ScopeKey) String() string {
	switch sk.Kind {
	case ScopeThrottleAccount:
		return wireThrottleAccount
	case ScopeService:
		return wireService
	case ScopeQuotaOwn:
		return wireQuotaOwn
	case ScopeQuotaShortcut:
		return wireQuotaShortcut + sk.Param
	case ScopePermDir:
		return wirePermDir + sk.Param
	case ScopeDiskLocal:
		return wireDiskLocal
	default:
		return ""
	}
}

// ParseScopeKey deserializes a wire-format string into a ScopeKey.
// Returns the zero-value ScopeKey for unknown formats.
func ParseScopeKey(s string) ScopeKey {
	switch {
	case s == wireThrottleAccount:
		return SKThrottleAccount
	case s == wireService:
		return SKService
	case s == wireQuotaOwn:
		return SKQuotaOwn
	case s == wireDiskLocal:
		return SKDiskLocal
	case len(s) > len(wireQuotaShortcut) && s[:len(wireQuotaShortcut)] == wireQuotaShortcut:
		return SKQuotaShortcut(s[len(wireQuotaShortcut):])
	case len(s) > len(wirePermDir) && s[:len(wirePermDir)] == wirePermDir:
		return SKPermDir(s[len(wirePermDir):])
	default:
		return ScopeKey{}
	}
}

// IsGlobal returns true for scope blocks that affect ALL actions (throttle,
// service). Used by isObservationSuppressed to skip API calls during outages.
func (sk ScopeKey) IsGlobal() bool {
	return sk.Kind == ScopeThrottleAccount || sk.Kind == ScopeService
}

// IsPermDir returns true for local directory permission scope blocks.
func (sk ScopeKey) IsPermDir() bool {
	return sk.Kind == ScopePermDir
}

// DirPath returns the directory path for a ScopePermDir key.
// Panics if called on a non-PermDir key (defensive — caller bug).
func (sk ScopeKey) DirPath() string {
	if sk.Kind != ScopePermDir {
		panic("ScopeKey.DirPath() called on non-PermDir key")
	}
	return sk.Param
}

// IssueType returns the issue_type constant for this scope key's kind.
// Used to populate sync_failures.issue_type consistently.
func (sk ScopeKey) IssueType() string {
	switch sk.Kind {
	case ScopeThrottleAccount:
		return IssueRateLimited
	case ScopeService:
		return IssueServiceOutage
	case ScopeQuotaOwn, ScopeQuotaShortcut:
		return IssueQuotaExceeded
	case ScopePermDir:
		return IssueLocalPermissionDenied
	case ScopeDiskLocal:
		return IssueDiskFull
	default:
		return ""
	}
}

// Humanize translates a scope key to a user-friendly description (R-2.10.22).
// For shortcut scopes, looks up the shortcut's local path from the provided
// list. For perm:dir, returns the directory path. For global scopes, returns
// a plain English description.
func (sk ScopeKey) Humanize(shortcuts []Shortcut) string {
	switch sk.Kind {
	case ScopeThrottleAccount:
		return "your OneDrive account (rate limited)"
	case ScopeService:
		return "OneDrive service"
	case ScopeQuotaOwn:
		return "your OneDrive storage"
	case ScopeQuotaShortcut:
		for i := range shortcuts {
			if shortcuts[i].RemoteDrive+":"+shortcuts[i].RemoteItem == sk.Param {
				return shortcuts[i].LocalPath
			}
		}
		return sk.Param // fallback to composite key
	case ScopePermDir:
		return sk.Param
	case ScopeDiskLocal:
		return "local disk"
	default:
		return sk.String()
	}
}

// BlocksAction returns true if this scope key blocks the given action.
// Replaces the scattered string-matching logic from blockedScope().
func (sk ScopeKey) BlocksAction(path, shortcutKey string, actionType ActionType, targetsOwnDrive bool) bool {
	switch sk.Kind {
	case ScopeThrottleAccount, ScopeService:
		return true // global blocks
	case ScopeDiskLocal:
		return actionType == ActionDownload
	case ScopeQuotaOwn:
		return targetsOwnDrive && actionType == ActionUpload
	case ScopeQuotaShortcut:
		return shortcutKey == sk.Param && actionType == ActionUpload
	case ScopePermDir:
		return path == sk.Param || strings.HasPrefix(path, sk.Param+"/")
	default:
		return false
	}
}

// ScopeKeyForStatus maps an HTTP status code and shortcut context to a
// ScopeKey. Returns the zero-value for non-scope statuses. Single source
// of truth for HTTP status → scope key classification.
func ScopeKeyForStatus(httpStatus int, shortcutKey string) ScopeKey {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		return SKThrottleAccount
	case httpStatus == http.StatusServiceUnavailable:
		return SKService
	case httpStatus == http.StatusInsufficientStorage:
		if shortcutKey != "" {
			return SKQuotaShortcut(shortcutKey)
		}
		return SKQuotaOwn
	case httpStatus >= http.StatusInternalServerError:
		return SKService
	default:
		return ScopeKey{}
	}
}

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

// Unified trial timing constants (R-2.10.6, R-2.10.7, R-2.10.8, R-2.10.14).
// All scope types share the same initial interval and cap. Server-provided
// Retry-After values bypass the cap entirely (server is ground truth).
const (
	defaultInitialTrialInterval = 5 * time.Second
	defaultMaxTrialInterval     = 5 * time.Minute
)

// ScopeState maintains sliding windows for scope escalation detection and
// records successes that reset windows. Thread-safety is provided by the
// engine's single-goroutine drain loop — all calls come from
// processWorkerResult which runs on one goroutine.
type ScopeState struct {
	windows map[ScopeKey]*slidingWindow
	nowFunc func() time.Time
	logger  *slog.Logger
}

// NewScopeState creates a ScopeState with the given clock and logger.
func NewScopeState(nowFunc func() time.Time, logger *slog.Logger) *ScopeState {
	return &ScopeState{
		windows: make(map[ScopeKey]*slidingWindow),
		nowFunc: nowFunc,
		logger:  logger,
	}
}

// ScopeUpdateResult describes the outcome of updateScope: whether a new scope
// block should be created. Does NOT contain the computed trial interval —
// interval computation is centralized in computeTrialInterval() to prevent
// divergence between initial block creation and subsequent trial extensions.
type ScopeUpdateResult struct {
	Block      bool          // true if threshold crossed → create block
	ScopeKey   ScopeKey      // scope key for the block
	IssueType  string        // "rate_limited", IssueQuotaExceeded, IssueServiceOutage
	RetryAfter time.Duration // server-provided Retry-After (0 if absent)
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
		return ScopeUpdateResult{
			Block:      true,
			ScopeKey:   SKThrottleAccount,
			IssueType:  IssueRateLimited,
			RetryAfter: r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusServiceUnavailable && r.RetryAfter > 0:
		// Immediate block — 503 with Retry-After is a server signal (R-2.10.3).
		return ScopeUpdateResult{
			Block:      true,
			ScopeKey:   SKService,
			IssueType:  IssueServiceOutage,
			RetryAfter: r.RetryAfter,
		}

	case r.HTTPStatus == http.StatusInsufficientStorage:
		// Quota failure — scope depends on target drive (R-2.10.1, R-2.10.17).
		sk := ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
		return ss.checkWindow(sk, r.Path, quotaWindowThreshold, quotaWindowDuration, IssueQuotaExceeded)

	case r.HTTPStatus >= http.StatusInternalServerError:
		// Service error — feed into service sliding window (R-2.10.28, R-2.10.29).
		sk := ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
		return ss.checkWindow(sk, r.Path,
			serviceWindowThreshold, serviceWindowDuration,
			IssueServiceOutage)

	default:
		return ScopeUpdateResult{}
	}
}

// UpdateScopeOutagePattern feeds a 400 outage-pattern result into the
// service sliding window. Called separately from UpdateScope because
// outage patterns are classified as resultRequeue (not resultScopeBlock)
// by classifyResult but still need to feed scope detection.
func (ss *ScopeState) UpdateScopeOutagePattern(path string) ScopeUpdateResult {
	return ss.checkWindow(SKService, path, serviceWindowThreshold, serviceWindowDuration, IssueServiceOutage)
}

// RecordSuccess resets the sliding window for scopes relevant to the
// successful action. Per §7.3.1: "A success from any path in the scope
// shall reset the unique-path failure counter."
func (ss *ScopeState) RecordSuccess(r *WorkerResult) {
	// Success resets all potentially-relevant windows for this action's scope.
	if r.ShortcutKey != "" {
		delete(ss.windows, SKQuotaShortcut(r.ShortcutKey))
	} else {
		delete(ss.windows, SKQuotaOwn)
	}
	// Also reset service window — a successful request proves the service is up.
	delete(ss.windows, SKService)
}

// checkWindow adds a failure to the named sliding window and returns a
// ScopeUpdateResult indicating whether the threshold was crossed.
func (ss *ScopeState) checkWindow(
	sk ScopeKey, path string, threshold int, window time.Duration,
	issueType string,
) ScopeUpdateResult {
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
		return ScopeUpdateResult{
			Block:     true,
			ScopeKey:  sk,
			IssueType: issueType,
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
