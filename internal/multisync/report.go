package multisync

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// DriveReport summarizes the result of a single drive's sync run.
// Err and Report are mutually exclusive: when Err is set, Report is nil.
type DriveReport struct {
	CanonicalID driveid.CanonicalID
	DisplayName string
	Report      *syncengine.Report
	Err         error
}

type WatchStartupError struct {
	Failures []DriveReport
}

func (e *WatchStartupError) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "watch startup failed"
	}
	if len(e.Failures) == 1 {
		failure := e.Failures[0]
		return fmt.Sprintf("watch startup failed for %s: %v", failure.CanonicalID, failure.Err)
	}

	return fmt.Sprintf("%d drives failed to start in watch mode", len(e.Failures))
}
