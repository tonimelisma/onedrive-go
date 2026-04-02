package multisync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// DriveRunner manages a single drive's sync lifecycle with panic recovery
// and error isolation. Each DriveRunner runs independently, so one drive can
// fail without destabilizing the rest of the multi-drive control plane.
type DriveRunner struct {
	canonID     driveid.CanonicalID
	displayName string
}

// run executes the provided sync function with panic recovery. The control
// plane injects the per-drive RunOnce closure instead of holding a direct
// Engine reference so tests can exercise panic isolation without a real
// engine stack.
func (dr *DriveRunner) run(ctx context.Context, fn func(context.Context) (*synctypes.SyncReport, error)) (result *synctypes.DriveReport) {
	result = &synctypes.DriveReport{
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
