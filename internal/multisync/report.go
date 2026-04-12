package multisync

import (
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
