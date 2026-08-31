package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// MountRunner manages a single mount's sync lifecycle with panic recovery
// and error isolation. Each MountRunner runs independently, so one mount can
// fail without destabilizing the rest of the multi-mount control plane.
type MountRunner struct {
	selectionIndex int
	identity       MountIdentity
	displayName    string

	// logger records the panic stack. The recovered value alone names what
	// failed but not where, and the goroutine that could answer that is gone
	// by the time the report is read.
	logger *slog.Logger
}

// run executes the provided sync function with panic recovery. The control
// plane injects the per-mount RunOnce closure instead of holding a direct
// Engine reference so tests can exercise panic isolation without a real
// engine stack.
func (dr *MountRunner) run(ctx context.Context, fn func(context.Context) (*syncengine.Report, error)) (result *MountReport) {
	result = &MountReport{
		SelectionIndex: dr.selectionIndex,
		Identity:       dr.identity,
		DisplayName:    dr.displayName,
	}

	defer func() {
		r := recover()
		if r == nil {
			return
		}

		if dr.logger != nil {
			dr.logger.Error("panic in mount sync",
				slog.String("mount", dr.identity.Label()),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
		}

		result.Report = nil
		result.Err = fmt.Errorf("panic in mount %s: %v", dr.identity.Label(), r)
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}
