package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type DriveStartupStatus string

const (
	DriveStartupRunnable      DriveStartupStatus = "runnable"
	DriveStartupPaused        DriveStartupStatus = "paused"
	DriveStartupResetRequired DriveStartupStatus = "reset_required"
	DriveStartupFatal         DriveStartupStatus = "fatal"
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
	if isResetRequiredStartupError(err) {
		return DriveStartupResetRequired
	}

	return DriveStartupFatal
}

func isResetRequiredStartupError(err error) bool {
	return err != nil && syncengine.IsStateDBResetRequired(err)
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
	Results []DriveStartupResult
}

func (e *WatchStartupError) Error() string {
	if e == nil || len(e.Results) == 0 {
		return "watch startup failed"
	}
	if len(e.Results) == 1 {
		failure := e.Results[0]
		return fmt.Sprintf("watch startup failed for %s: %v", failure.CanonicalID, failure.Err)
	}

	return fmt.Sprintf("%d drives failed to start in watch mode", len(e.Results))
}
