package multisync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// MountRunner manages a single mount's sync lifecycle with panic recovery
// and error isolation. Each MountRunner runs independently, so one mount can
// fail without destabilizing the rest of the multi-mount control plane.
type MountRunner struct {
	selectionIndex int
	canonID        driveid.CanonicalID
	displayName    string
}

// run executes the provided sync function with panic recovery. The control
// plane injects the per-mount RunOnce closure instead of holding a direct
// Engine reference so tests can exercise panic isolation without a real
// engine stack.
func (dr *MountRunner) run(ctx context.Context, fn func(context.Context) (*syncengine.Report, error)) (result *MountReport) {
	result = &MountReport{
		SelectionIndex: dr.selectionIndex,
		CanonicalID:    dr.canonID,
		DisplayName:    dr.displayName,
	}

	defer func() {
		if r := recover(); r != nil {
			result.Report = nil
			result.Err = fmt.Errorf("panic in mount %s: %v", dr.canonID, r)
		}
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}
