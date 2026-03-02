package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

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
