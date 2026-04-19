package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func syncStateResetCommand(canonicalID driveid.CanonicalID) string {
	return fmt.Sprintf("onedrive-go drive reset-sync-state --drive %s", canonicalID.String())
}

func syncPauseDriveCommand(canonicalID driveid.CanonicalID) string {
	return fmt.Sprintf("onedrive-go pause --drive %s", canonicalID.String())
}

func formatStartupResultMessage(result *multisync.DriveStartupResult) string {
	return formatSyncStateResetRequiredMessage(result.CanonicalID, result.Err)
}

func formatSyncStateResetRequiredMessage(canonicalID driveid.CanonicalID, err error) string {
	var resetErr *syncengine.StateDBResetRequiredError
	if !errors.As(err, &resetErr) {
		if err == nil {
			return ""
		}
		return err.Error()
	}

	return fmt.Sprintf(
		"%s. To continue, either pause or stop this drive first ('%s'), "+
			"rerun sync with --drive selecting only other drives, or fix the DB with '%s'.",
		resetErr.Error(),
		syncPauseDriveCommand(canonicalID),
		syncStateResetCommand(canonicalID),
	)
}

func formatSyncStateResetRequiredError(canonicalID driveid.CanonicalID, err error) error {
	message := formatSyncStateResetRequiredMessage(canonicalID, err)
	if message == "" {
		return err
	}

	return fmt.Errorf("%s", message)
}

func formatWatchStartupError(err error) error {
	var startupErr *multisync.WatchStartupError
	if !errors.As(err, &startupErr) {
		return err
	}
	if len(startupErr.Results) == 0 {
		return err
	}
	if len(startupErr.Results) == 1 {
		return fmt.Errorf("%s", formatStartupResultMessage(&startupErr.Results[0]))
	}

	parts := make([]string, 0, len(startupErr.Results))
	for i := range startupErr.Results {
		parts = append(parts, formatStartupResultMessage(&startupErr.Results[i]))
	}

	return fmt.Errorf("watch startup failed: %s", strings.Join(parts, "; "))
}

func writeWatchStartWarnings(output io.Writer, results []multisync.DriveStartupResult) {
	if len(results) == 0 {
		return
	}

	for i := range results {
		result := results[i]
		writeWarningf(output, "warning: drive %s did not start: %s\n",
			result.CanonicalID.String(),
			formatStartupResultMessage(&result),
		)
	}
}
