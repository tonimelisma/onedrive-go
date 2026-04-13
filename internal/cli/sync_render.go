package cli

import (
	"fmt"

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
			cc.Statusf("Error: %v\n", dr.Err)

			continue
		}

		if dr.Report != nil {
			printSyncReport(dr.Report, cc)
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
				firstErr = dr.Err
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
