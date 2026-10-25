package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
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
		return formatStateStoreIncompatibleMessage(&result.Identity, result.Err)
	case multisync.MountStartupFatal:
		if result.Err == nil {
			return ""
		}
		return result.Err.Error()
	}

	return ""
}

func formatStateStoreIncompatibleMessage(identity *multisync.MountIdentity, err error) string {
	var incompatibleErr *syncengine.StateStoreIncompatibleError
	if !errors.As(err, &incompatibleErr) {
		if err == nil {
			return ""
		}
		return err.Error()
	}

	if identity.IsStandalone() {
		return fmt.Sprintf(
			"%s. To continue, either pause or stop this mount first ('%s'), "+
				"rerun sync with --drive selecting only other configured parent drives, or fix the DB with '%s'.",
			incompatibleErr.Error(),
			syncPauseDriveCommand(identity.CanonicalID),
			syncStateResetCommand(identity.CanonicalID),
		)
	}

	label := identity.Label()
	if label == "" {
		label = "managed child mount"
	}
	statePath := ""
	if identity != nil && identity.MountID != "" {
		statePath = config.MountStatePath(identity.MountID)
	}
	if statePath != "" {
		return fmt.Sprintf(
			"%s. Managed shortcut mount %s is controlled by its parent drive and OneDrive shortcut. "+
				"Inspect its state DB at %s, or remove/recreate the shortcut if this persists.",
			incompatibleErr.Error(),
			label,
			statePath,
		)
	}

	return fmt.Sprintf(
		"%s. Managed shortcut mount %s is controlled by its parent drive and OneDrive shortcut.",
		incompatibleErr.Error(),
		label,
	)
}

func formatStateStoreIncompatibleError(identity *multisync.MountIdentity, err error) error {
	message := formatStateStoreIncompatibleMessage(identity, err)
	if message == "" {
		return err
	}

	return newPresentedError(message, err)
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
		return startupFailureError(formatStartupResultMessage(&failures[0]), failures)
	}

	parts := make([]string, 0, len(failures))
	for i := range failures {
		parts = append(parts, formatStartupResultMessage(&failures[i]))
	}

	return startupFailureError("watch startup failed: "+strings.Join(parts, "; "), failures)
}

// startupFailureError keeps the drives' causes reachable for the exit status
// (R-6.6.31) only when every failed drive failed for the same reason that a
// restart cannot fix: all need sign-in, or all have invalid config. A mixed
// set exits 1, because restarting may still bring the other drives up.
func startupFailureError(message string, failures []multisync.MountStartupResult) error {
	causes := make([]error, 0, len(failures))
	allSignIn, allConfig := len(failures) > 0, len(failures) > 0
	for i := range failures {
		cause := failures[i].Err
		causes = append(causes, cause)
		allSignIn = allSignIn && cause != nil && isSignInRequired(cause)
		allConfig = allConfig && cause != nil && errors.Is(cause, config.ErrInvalidConfig)
	}

	if allSignIn || allConfig {
		return newPresentedError(message, errors.Join(causes...))
	}

	return errors.New(message)
}

func writeWatchStartWarnings(output io.Writer, warning multisync.StartupWarning) {
	results := warning.Summary.SkippedResults()
	if len(results) == 0 {
		return
	}

	for i := range results {
		result := results[i]
		writeWarningf(output, "Warning: %s did not start: %s\n",
			startupResultLabel(&result),
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

// startupResultLabel names a drive by its display name, falling back to the
// internal mount label only when there is none.
func startupResultLabel(result *multisync.MountStartupResult) string {
	if result.DisplayName != "" {
		return result.DisplayName
	}

	return result.Identity.Label()
}
