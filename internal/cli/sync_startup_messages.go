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
	return fmt.Sprintf("onedrive-go drive reset-sync-state --drive %s", shellQuoteArg(canonicalID.String()))
}

func syncPauseDriveCommand(canonicalID driveid.CanonicalID) string {
	return fmt.Sprintf("onedrive-go pause --drive %s", shellQuoteArg(canonicalID.String()))
}

func formatStartupResultMessage(result *multisync.MountStartupResult) string {
	if result == nil {
		return ""
	}

	switch result.Status {
	case multisync.MountStartupRunnable:
		return ""
	case multisync.MountStartupPaused:
		return "mount is paused"
	case multisync.MountStartupIncompatibleStore:
		return formatStateStoreIncompatibleMessage(result.CanonicalID, result.Err)
	case multisync.MountStartupFatal:
		if result.Err == nil {
			return ""
		}
		return result.Err.Error()
	}

	return ""
}

func formatStateStoreIncompatibleMessage(canonicalID driveid.CanonicalID, err error) string {
	var incompatibleErr *syncengine.StateStoreIncompatibleError
	if !errors.As(err, &incompatibleErr) {
		if err == nil {
			return ""
		}
		return err.Error()
	}

	return fmt.Sprintf(
		"%s. To continue, either pause or stop this mount first ('%s'), "+
			"rerun sync with --drive selecting only other configured parent drives, or fix the DB with '%s'.",
		incompatibleErr.Error(),
		syncPauseDriveCommand(canonicalID),
		syncStateResetCommand(canonicalID),
	)
}

func formatStateStoreIncompatibleError(canonicalID driveid.CanonicalID, err error) error {
	message := formatStateStoreIncompatibleMessage(canonicalID, err)
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
	if startupErr.Summary.SelectedCount() == 0 {
		return err
	}
	if startupErr.Summary.AllPaused() {
		return fmt.Errorf("all selected mounts are paused — run 'onedrive-go resume' to unpause")
	}

	failures := startupErr.Summary.SkippedResults()
	if len(failures) == 1 {
		return fmt.Errorf("%s", formatStartupResultMessage(&failures[0]))
	}

	parts := make([]string, 0, len(failures))
	for i := range failures {
		parts = append(parts, formatStartupResultMessage(&failures[i]))
	}

	return fmt.Errorf("watch startup failed: %s", strings.Join(parts, "; "))
}

func writeWatchStartWarnings(output io.Writer, warning multisync.StartupWarning) {
	results := warning.Summary.SkippedResults()
	if len(results) == 0 {
		return
	}

	for i := range results {
		result := results[i]
		writeWarningf(output, "warning: mount %s did not start: %s\n",
			result.CanonicalID.String(),
			formatStartupResultMessage(&result),
		)
	}
}

func shellQuoteArg(value string) string {
	if value == "" {
		return "''"
	}

	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
