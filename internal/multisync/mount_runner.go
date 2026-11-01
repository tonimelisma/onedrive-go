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

		result.Report = nil
		result.Err = recoveredSyncPanic(dr.logger, name, dr.identity.Label(), r)
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}

// recoveredSyncPanic is the one place a panic recovered at a mount's
// goroutine boundary is logged and turned into the mount's error: one-shot
// MountRunner.run and the watch runner goroutine both use it, so a panic reads
// the same whichever mode hit it.
//
// The log record is the diagnostic account, at Error as every recovered panic
// is. It is captured here, on the panicking goroutine, because the recovered
// value names what failed but not where, and the goroutine that could answer
// that is gone by the time anyone reads the report. The stack attribute stays
// in the log file and the --verbose console; the default console drops it
// (cli's isInternalLogField) so a panic does not put kilobytes in front of a
// person. The returned error is the user-facing account: the CLI prints it
// for a one-shot sync, and the watch loop reports it as the reason the drive
// stopped before restarting it.
func recoveredSyncPanic(logger *slog.Logger, name, mountID string, r any) error {
	if logger != nil {
		logger.Error("Internal error while syncing "+name+"; this is a bug, please report it",
			slog.String("drive", name),
			slog.String("mount_id", mountID),
			slog.Any("panic", r),
			slog.String("stack", string(debug.Stack())),
		)
	}

	return fmt.Errorf(
		"internal error while syncing %s: %v. This is a bug; please report it, with the log file if you have one", name, r)
}
