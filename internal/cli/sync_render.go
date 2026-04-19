package cli

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// printDriveReports prints sync reports for all drives. When there's only
// one drive, the output is identical to the pre-Orchestrator format. For
// multiple drives, each drive's output is prefixed with a header.
func printDriveReports(reports []*multisync.DriveReport, cc *CLIContext) {
	multiDrive := len(reports) > 1

	for _, dr := range reports {
		if multiDrive {
			cc.Statusf("\n--- %s ---\n", dr.DisplayName)
		}

		if dr.Err != nil {
			cc.Statusf("Error: %s\n", formatDriveReportErrorMessage(dr))

			continue
		}

		if dr.Report != nil {
			printSyncReport(dr.Report, cc)
		}
	}
}

func printRunOnceResult(result multisync.RunOnceResult, cc *CLIContext) {
	multiDrive := result.Startup.SelectedCount() > 1
	reportByDrive := make(map[driveid.CanonicalID]*multisync.DriveReport, len(result.Reports))
	for _, report := range result.Reports {
		if report == nil {
			continue
		}
		reportByDrive[report.CanonicalID] = report
	}

	for i := range result.Startup.Results {
		startup := &result.Startup.Results[i]
		if multiDrive {
			cc.Statusf("\n--- %s ---\n", startup.DisplayName)
		}

		if startup.Status != multisync.DriveStartupRunnable {
			label := "Skipped"
			if startup.Status != multisync.DriveStartupPaused {
				label = "Error"
			}
			cc.Statusf("%s: %s\n", label, formatStartupResultMessage(startup))
			continue
		}

		report := reportByDrive[startup.CanonicalID]
		if report == nil {
			cc.Statusf("Error: missing sync report for drive startup result\n")
			continue
		}

		if report.Err != nil {
			cc.Statusf("Error: %s\n", formatDriveReportErrorMessage(report))
			continue
		}

		if report.Report != nil {
			printSyncReport(report.Report, cc)
		}
	}
}

// driveReportsError returns an error if any drive report has an error.
// Returns nil when all drives succeeded.
func driveReportsError(reports []*multisync.DriveReport) error {
	var firstErr error

	failCount := 0

	for _, dr := range reports {
		if dr.Err != nil {
			failCount++

			if firstErr == nil {
				firstErr = formatStateStoreIncompatibleError(dr.CanonicalID, dr.Err)
			}
		}
	}

	if failCount == 0 {
		return nil
	}

	if len(reports) == 1 {
		return firstErr
	}

	return fmt.Errorf("%d of %d drives failed: %w", failCount, len(reports), firstErr)
}

func runOnceResultError(result multisync.RunOnceResult) error {
	if result.Startup.AllPaused() {
		return fmt.Errorf("all selected drives are paused — run 'onedrive-go resume' to unpause")
	}

	var firstErr error
	failCount := 0

	skipped := result.Startup.SkippedResults()
	for i := range skipped {
		startup := &skipped[i]
		failCount++
		if firstErr == nil {
			firstErr = fmt.Errorf("%s", formatStartupResultMessage(startup))
		}
	}

	for _, dr := range result.Reports {
		if dr == nil || dr.Err == nil {
			continue
		}
		failCount++
		if firstErr == nil {
			firstErr = formatStateStoreIncompatibleError(dr.CanonicalID, dr.Err)
		}
	}

	if failCount == 0 {
		return nil
	}

	if result.Startup.SelectedCount() <= 1 {
		return firstErr
	}

	return fmt.Errorf("%d of %d selected drives failed or were skipped: %w",
		failCount,
		result.Startup.SelectedCount(),
		firstErr,
	)
}

func formatDriveReportErrorMessage(dr *multisync.DriveReport) string {
	if dr == nil || dr.Err == nil {
		return ""
	}

	return formatStateStoreIncompatibleMessage(dr.CanonicalID, dr.Err)
}

// printNonZero prints a labeled count line only when n > 0.
func printNonZero(cc *CLIContext, label string, n int) {
	if n > 0 {
		cc.Statusf("  %-16s%d\n", label+":", n)
	}
}

func reportActionTotal(r *syncengine.Report) int {
	if r == nil {
		return 0
	}

	return r.FolderCreates + r.Moves + r.Downloads + r.Uploads +
		r.LocalDeletes + r.RemoteDeletes + r.Conflicts +
		r.SyncedUpdates + r.Cleanups
}

// printSyncReport formats and prints the sync report to the CLI status stream.
func printSyncReport(r *syncengine.Report, cc *CLIContext) {
	if r.DryRun {
		cc.Statusf("Dry run — no changes applied\n")
	}

	cc.Statusf("Mode: %s\n", r.Mode)
	cc.Statusf("Duration: %s\n", r.Duration)

	planTotal := reportActionTotal(r)
	deferredTotal := r.DeferredByMode.Total()

	if planTotal == 0 && deferredTotal == 0 {
		cc.Statusf("No changes detected\n")
		return
	}

	if planTotal > 0 {
		cc.Statusf("\nPlan:\n")
		printNonZero(cc, "Folder creates", r.FolderCreates)
		printNonZero(cc, "Moves", r.Moves)
		printNonZero(cc, "Downloads", r.Downloads)
		printNonZero(cc, "Uploads", r.Uploads)
		printNonZero(cc, "Local deletes", r.LocalDeletes)
		printNonZero(cc, "Remote deletes", r.RemoteDeletes)
		printNonZero(cc, "Conflicts", r.Conflicts)
		printNonZero(cc, "Synced updates", r.SyncedUpdates)
		printNonZero(cc, "Cleanups", r.Cleanups)
	}

	if deferredTotal > 0 {
		cc.Statusf("\nDeferred by mode:\n")
		printNonZero(cc, "Folder creates", r.DeferredByMode.FolderCreates)
		printNonZero(cc, "Moves", r.DeferredByMode.Moves)
		printNonZero(cc, "Downloads", r.DeferredByMode.Downloads)
		printNonZero(cc, "Uploads", r.DeferredByMode.Uploads)
		printNonZero(cc, "Local deletes", r.DeferredByMode.LocalDeletes)
		printNonZero(cc, "Remote deletes", r.DeferredByMode.RemoteDeletes)
	}

	if !r.DryRun && planTotal > 0 {
		cc.Statusf("\nResults:\n")
		cc.Statusf("  Succeeded: %d\n", r.Succeeded)
		cc.Statusf("  Failed:    %d\n", r.Failed)

		for _, e := range r.Errors {
			cc.Statusf("  Error:     %v\n", e)
		}
	}
}
