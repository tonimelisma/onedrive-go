package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Backoff durations for consecutive failures in watch mode (6.0c).
// Threshold: 3 consecutive failures before any backoff is applied.
const (
	backoffThreshold = 3
	backoffMaxCap    = 1 * time.Hour
)

// backoffSteps maps consecutive failure counts (starting at the threshold)
// to their backoff durations: 3→1m, 4→5m, 5→15m, 6+→1h.
var backoffSteps = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	backoffMaxCap,
}

// DriveReport summarizes the result of a single drive's sync cycle.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type DriveReport struct {
	CanonicalID driveid.CanonicalID
	DisplayName string
	Report      *SyncReport // nil when Err is set
	Err         error
}

// DriveRunner manages a single drive's sync lifecycle with panic recovery
// and error isolation. Each DriveRunner runs independently — a panic or
// error in one drive does not affect others.
type DriveRunner struct {
	canonID     driveid.CanonicalID
	displayName string
}

// run executes the provided sync function with panic recovery. The fn
// parameter is a closure binding engine.RunOnce(ctx, mode, opts) — using
// function injection rather than a direct Engine reference enables testing
// panic recovery without real Engines.
func (dr *DriveRunner) run(ctx context.Context, fn func(context.Context) (*SyncReport, error)) (result *DriveReport) {
	result = &DriveReport{
		CanonicalID: dr.canonID,
		DisplayName: dr.displayName,
	}

	defer func() {
		if r := recover(); r != nil {
			result.Report = nil
			result.Err = fmt.Errorf("panic in drive %s: %v", dr.canonID, r)
		}
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}

// backoffDuration returns the backoff duration for the given number of
// consecutive failures. Returns 0 for fewer than backoffThreshold failures.
// Used by 6.0c watch mode for error backoff.
func backoffDuration(failures int) time.Duration {
	if failures < backoffThreshold {
		return 0
	}

	idx := failures - backoffThreshold
	if idx >= len(backoffSteps) {
		return backoffMaxCap
	}

	return backoffSteps[idx]
}
