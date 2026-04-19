package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type DriveStartupStatus string

const (
	DriveStartupRunnable          DriveStartupStatus = "runnable"
	DriveStartupPaused            DriveStartupStatus = "paused"
	DriveStartupIncompatibleStore DriveStartupStatus = "incompatible_store"
	DriveStartupFatal             DriveStartupStatus = "fatal"
)

// DriveStartupResult captures per-drive startup eligibility before any one-shot
// pass or watch runner actually runs. It keeps expected startup policy separate
// from completed sync reports.
type DriveStartupResult struct {
	CanonicalID driveid.CanonicalID
	DisplayName string
	Status      DriveStartupStatus
	Err         error
}

func classifyDriveStartupError(err error) DriveStartupStatus {
	if err == nil {
		return DriveStartupRunnable
	}
	if isIncompatibleStoreStartupError(err) {
		return DriveStartupIncompatibleStore
	}

	return DriveStartupFatal
}

func isIncompatibleStoreStartupError(err error) bool {
	return err != nil && syncengine.IsStateStoreIncompatible(err)
}

type StartupSelectionSummary struct {
	Results []DriveStartupResult
}

func summarizeStartupResults(results []DriveStartupResult) StartupSelectionSummary {
	return StartupSelectionSummary{
		Results: append([]DriveStartupResult(nil), results...),
	}
}

func (s StartupSelectionSummary) SelectedCount() int {
	return len(s.Results)
}

func (s StartupSelectionSummary) RunnableCount() int {
	count := 0
	for i := range s.Results {
		if s.Results[i].Status == DriveStartupRunnable {
			count++
		}
	}

	return count
}

func (s StartupSelectionSummary) PausedCount() int {
	count := 0
	for i := range s.Results {
		if s.Results[i].Status == DriveStartupPaused {
			count++
		}
	}

	return count
}

func (s StartupSelectionSummary) AllPaused() bool {
	return len(s.Results) > 0 && s.PausedCount() == len(s.Results)
}

func (s StartupSelectionSummary) SkippedResults() []DriveStartupResult {
	skipped := make([]DriveStartupResult, 0, len(s.Results))
	for i := range s.Results {
		if s.Results[i].Status == DriveStartupRunnable || s.Results[i].Status == DriveStartupPaused {
			continue
		}
		skipped = append(skipped, s.Results[i])
	}

	return skipped
}

type StartupWarning struct {
	Summary StartupSelectionSummary
}

type RunOnceResult struct {
	Startup StartupSelectionSummary
	Reports []*DriveReport
}

// DriveReport summarizes the result of a single drive's sync run.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type DriveReport struct {
	CanonicalID driveid.CanonicalID
	DisplayName string
	Report      *syncengine.Report
	Err         error
}

type WatchStartupError struct {
	Summary StartupSelectionSummary
}

func (e *WatchStartupError) Error() string {
	if e == nil || e.Summary.SelectedCount() == 0 {
		return "watch startup failed"
	}
	if e.Summary.AllPaused() {
		return "watch startup failed: all selected drives are paused"
	}
	failures := e.Summary.SkippedResults()
	if len(failures) == 1 {
		failure := failures[0]
		return fmt.Sprintf("watch startup failed for %s: %v", failure.CanonicalID, failure.Err)
	}

	return fmt.Sprintf("%d drives failed to start in watch mode", len(failures))
}
