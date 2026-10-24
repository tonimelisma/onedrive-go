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

		name := dr.displayName
		if name == "" {
			name = dr.identity.Label()
		}

		// The report error is the user-facing account of this panic; the CLI
		// prints it once. The stack is diagnostic detail for a bug report,
		// so it goes to the log at INFO (kept by the log file, shown with
		// --verbose) instead of dumping kilobytes onto every console,
		// including --quiet ones.
		if dr.logger != nil {
			dr.logger.Info("recovered panic while syncing a drive",
				slog.String("drive", name),
				slog.String("mount_id", dr.identity.Label()),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
		}

		result.Report = nil
		result.Err = fmt.Errorf(
			"internal error while syncing %s: %v. This is a bug; please report it, with the log file if you have one", name, r)
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}
