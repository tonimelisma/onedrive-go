package cli

import (
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// errSyncLeftWork is the cause of a one-shot sync that finished but left
// files unsynced (R-6.6.28). It carries no exit-status sentinel, so the run
// exits 1: running again may finish the work.
var errSyncLeftWork = errors.New("sync left work unfinished")

// runOnceResultError is the one-shot command's final error: the drives that
// failed or were skipped; otherwise the drives that left work (left, keyed
// by mount ID, each the report's own Report.LeftWork, from
// syncConsole.Summary, so the exit status and the summaries cannot
// disagree); and a drive whose sync state did
// not close after the pass (RunOnceResult.Stuck), which fails the command
// even when every drive synced, so the process exits and the stuck engine's
// locks go. A drive error comes first, so its exit status (77, 78) wins.
func runOnceResultError(result multisync.RunOnceResult, left map[string]syncengine.LeftWork) error {
	drivesErr := runOnceDrivesError(result)
	if drivesErr == nil {
		drivesErr = runOnceLeftWorkError(result, left)
	}
	if result.Stuck == nil {
		return drivesErr
	}

	return errors.Join(drivesErr, result.Stuck)
}

// runOnceLeftWorkError fails a run that left work on any drive it ran. Each
// drive's summary line already said what; the error says where to look.
func runOnceLeftWorkError(result multisync.RunOnceResult, left map[string]syncengine.LeftWork) error {
	drives, files := 0, 0
	for _, counts := range left {
		if counts.Any() {
			drives++
			files += counts.Total()
		}
	}
	if drives == 0 {
		return nil
	}

	selected := result.Startup.SelectedCount()
	if selected <= 1 {
		noun := "files"
		if files == 1 {
			noun = "file"
		}

		return newPresentedError(fmt.Sprintf("sync left %d %s unsynced; run 'onedrive-go status' for details",
			files, noun), errSyncLeftWork)
	}

	return newPresentedError(fmt.Sprintf("%d of %d selected drives left files unsynced (see above)",
		drives, selected), errSyncLeftWork)
}

func runOnceDrivesError(result multisync.RunOnceResult) error {
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
