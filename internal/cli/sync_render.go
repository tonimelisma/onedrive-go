package cli

import (
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/multisync"
)

// mountReportsError returns an error if any mount report has an error.
func mountReportsError(reports []*multisync.MountReport) error {
	var firstErr error

	failCount := 0

	for _, dr := range reports {
		if dr.Err != nil {
			failCount++

			if firstErr == nil {
				firstErr = formatStateStoreIncompatibleError(&dr.Identity, dr.Err)
			}
		}
	}

	if failCount == 0 {
		return nil
	}

	if len(reports) == 1 {
		return firstErr
	}

	return fmt.Errorf("%d of %d mounts failed: %w", failCount, len(reports), firstErr)
}

func runOnceResultError(result multisync.RunOnceResult) error {
	if result.Startup.AllPaused() {
		return fmt.Errorf("all selected mounts are paused — run 'onedrive-go resume' to unpause")
	}

	var firstErr error
	failCount := 0

	skipped := result.Startup.SkippedResults()
	for i := range skipped {
		startup := &skipped[i]
		failCount++
		if firstErr == nil {
			firstErr = newPresentedError(formatStartupResultMessage(startup), startup.Err)
		}
	}

	for _, dr := range result.Reports {
		if dr == nil || dr.Err == nil {
			continue
		}
		failCount++
		if firstErr == nil {
			firstErr = formatStateStoreIncompatibleError(&dr.Identity, dr.Err)
		}
	}

	if failCount == 0 {
		return nil
	}

	if result.Startup.SelectedCount() <= 1 {
		return firstErr
	}

	// The per-drive errors were already printed with their drive names, so
	// the returned message only counts them; the first error stays reachable
	// for classification and the exit code.
	return newPresentedError(fmt.Sprintf("%d of %d selected drives failed or were skipped (see above)",
		failCount,
		result.Startup.SelectedCount(),
	), firstErr)
}

func formatMountReportErrorMessage(dr *multisync.MountReport) string {
	if dr == nil || dr.Err == nil {
		return ""
	}

	return formatStateStoreIncompatibleMessage(&dr.Identity, dr.Err)
}

// errSyncIncomplete marks a pass that ran but left items unsynced, so cron
// and systemd see a failure instead of exit 0 after "...; 2 failed."
var errSyncIncomplete = errors.New("sync incomplete")

// incompleteSyncError fails a pass that left items unsynced for good. Items
// the engine recorded for retry (throttling, transient 404s, server errors)
// are not counted: the summary already says the next sync retries them, and
// failing every run on a passing condition would train cron users to ignore
// the exit status. permanentFailures is the console's per-drive count, so the
// exit status and the "N failed" in the summary never disagree.
func incompleteSyncError(reports []*multisync.MountReport, permanentFailures func(*multisync.MountReport) int) error {
	failed := 0
	for _, dr := range reports {
		if dr != nil && dr.Report != nil {
			failed += permanentFailures(dr)
		}
	}

	if failed == 0 {
		return nil
	}

	return newPresentedError(
		countItems(failed)+" could not be synced (see above).",
		errSyncIncomplete,
	)
}
