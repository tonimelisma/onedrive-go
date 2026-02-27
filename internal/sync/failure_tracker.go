package sync

import (
	"log/slog"
	"sync"
	"time"
)

// Failure suppression constants for watch mode (B-123).
const (
	failureThreshold = 3                // skip after this many failures
	failureCooldown  = 30 * time.Minute // forget failures older than this
)

// failureRecord tracks failures for a single path.
type failureRecord struct {
	count   int
	lastErr string
	lastAt  time.Time
}

// failureTracker suppresses paths that fail repeatedly in watch mode.
// Thread-safe. Paths that fail >= failureThreshold times within
// failureCooldown are skipped with a Warn log. Success clears the record.
type failureTracker struct {
	mu      sync.Mutex
	records map[string]*failureRecord
	logger  *slog.Logger
	nowFunc func() time.Time // injectable for testing
}

// newFailureTracker creates a failure tracker for watch mode.
func newFailureTracker(logger *slog.Logger) *failureTracker {
	return &failureTracker{
		records: make(map[string]*failureRecord),
		logger:  logger,
		nowFunc: time.Now,
	}
}

// shouldSkip returns true if the path has failed enough times within the
// cooldown window that it should be suppressed.
func (ft *failureTracker) shouldSkip(path string) bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	rec, ok := ft.records[path]
	if !ok {
		return false
	}

	// Forget stale failures.
	if ft.nowFunc().Sub(rec.lastAt) > failureCooldown {
		delete(ft.records, path)
		return false
	}

	return rec.count >= failureThreshold
}

// recordFailure increments the failure counter for a path.
func (ft *failureTracker) recordFailure(path, errMsg string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	rec, ok := ft.records[path]
	if !ok {
		rec = &failureRecord{}
		ft.records[path] = rec
	}

	// Reset if the previous failure is older than the cooldown.
	if ft.nowFunc().Sub(rec.lastAt) > failureCooldown {
		rec.count = 0
	}

	rec.count++
	rec.lastErr = errMsg
	rec.lastAt = ft.nowFunc()

	if rec.count == failureThreshold {
		ft.logger.Warn("path suppressed after repeated failures",
			slog.String("path", path),
			slog.Int("failures", rec.count),
			slog.String("last_error", errMsg),
			slog.Duration("cooldown", failureCooldown),
		)
	}
}

// recordSuccess clears the failure record for a path.
func (ft *failureTracker) recordSuccess(path string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	delete(ft.records, path)
}
