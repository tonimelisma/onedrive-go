package multisync

import (
	"context"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
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
	result = dr.report(nil, nil)

	defer func() {
		r := recover()
		if r == nil {
			return
		}

		result.Report = nil
		result.Err = recoveredSyncPanic(dr.logger, dr.name(), dr.identity.Label(), r)
	}()

	report, err := fn(ctx)
	result.Report = report
	result.Err = err

	return result
}

// name is how the mount is called in messages: its display name, or its
// identity when it has none.
func (dr *MountRunner) name() string {
	if dr.displayName != "" {
		return dr.displayName
	}

	return dr.identity.Label()
}

// report is this mount's report of one outcome.
func (dr *MountRunner) report(report *syncengine.Report, err error) *MountReport {
	return &MountReport{
		SelectionIndex: dr.selectionIndex,
		Identity:       dr.identity,
		DisplayName:    dr.displayName,
		Report:         report,
		Err:            err,
	}
}

// recoveredSyncPanic is the one place a panic recovered at a mount's
// goroutine boundary is logged and turned into the mount's error: one-shot
// MountRunner.run and the watch runner goroutine both use it, so a panic reads
// the same whichever mode hit it.
//
// panicguard.Recovered writes the diagnostic account: one Error record with
// the drive, the panic value and the stack of the panicking goroutine. The
// stack attribute stays in the log file and the --verbose console; the
// default console drops it (cli's isInternalLogField) so a panic does not put
// kilobytes in front of a person. The returned error, which wraps
// panicguard.ErrPanic, is the user-facing account: the CLI prints it for a
// one-shot sync, and the watch loop reports it as the reason the drive
// stopped before restarting it.
func recoveredSyncPanic(logger *slog.Logger, name, mountID string, r any) error {
	return panicguard.Recovered(logger, "while syncing "+name, r,
		slog.String("drive", name),
		slog.String("mount_id", mountID),
	)
}
