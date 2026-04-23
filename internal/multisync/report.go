package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type MountStartupStatus string

const (
	MountStartupRunnable          MountStartupStatus = "runnable"
	MountStartupPaused            MountStartupStatus = "paused"
	MountStartupIncompatibleStore MountStartupStatus = "incompatible_store"
	MountStartupFatal             MountStartupStatus = "fatal"
)

// MountStartupResult captures per-mount startup eligibility before any one-shot
// pass or watch runner actually runs. It keeps expected startup policy separate
// from completed sync reports.
type MountStartupResult struct {
	SelectionIndex int
	CanonicalID    driveid.CanonicalID
	DisplayName    string
	Status         MountStartupStatus
	Err            error
}

func classifyMountStartupError(err error) MountStartupStatus {
	if err == nil {
		return MountStartupRunnable
	}
	if isIncompatibleStoreStartupError(err) {
		return MountStartupIncompatibleStore
	}

	return MountStartupFatal
}

func isIncompatibleStoreStartupError(err error) bool {
	return err != nil && syncengine.IsStateStoreIncompatible(err)
}

type StartupSelectionSummary struct {
	Results []MountStartupResult
}

func summarizeStartupResults(results []MountStartupResult) StartupSelectionSummary {
	return StartupSelectionSummary{
		Results: append([]MountStartupResult(nil), results...),
	}
}

func (s StartupSelectionSummary) SelectedCount() int {
	return len(s.Results)
}

func (s StartupSelectionSummary) RunnableCount() int {
	count := 0
	for i := range s.Results {
		if s.Results[i].Status == MountStartupRunnable {
			count++
		}
	}

	return count
}

func (s StartupSelectionSummary) PausedCount() int {
	count := 0
	for i := range s.Results {
		if s.Results[i].Status == MountStartupPaused {
			count++
		}
	}

	return count
}

func (s StartupSelectionSummary) AllPaused() bool {
	return len(s.Results) > 0 && s.PausedCount() == len(s.Results)
}

func (s StartupSelectionSummary) SkippedResults() []MountStartupResult {
	skipped := make([]MountStartupResult, 0, len(s.Results))
	for i := range s.Results {
		if s.Results[i].Status == MountStartupRunnable || s.Results[i].Status == MountStartupPaused {
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
	Reports []*MountReport
}

// MountReport summarizes the result of a single mount's sync run.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type MountReport struct {
	SelectionIndex int
	CanonicalID    driveid.CanonicalID
	DisplayName    string
	Report         *syncengine.Report
	Err            error
}

type WatchStartupError struct {
	Summary StartupSelectionSummary
}

func (e *WatchStartupError) Error() string {
	if e == nil || e.Summary.SelectedCount() == 0 {
		return "watch startup failed"
	}
	if e.Summary.AllPaused() {
		return "watch startup failed: all selected mounts are paused"
	}
	failures := e.Summary.SkippedResults()
	if len(failures) == 1 {
		failure := failures[0]
		return fmt.Sprintf("watch startup failed for %s: %v", failure.CanonicalID, failure.Err)
	}

	return fmt.Sprintf("%d mounts failed to start in watch mode", len(failures))
}
