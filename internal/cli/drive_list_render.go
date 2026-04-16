package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// minColumnWidth is the minimum column width for formatted text output,
// preventing narrow columns when all entries happen to be short.
const minColumnWidth = 20

// driveListJSONOutput is the structured JSON schema for drive list output.
// Separates configured and available drives into distinct top-level keys,
// replacing the flat array that required callers to filter by "source" field.
type driveListJSONOutput struct {
	Configured            []driveListEntry         `json:"configured"`
	Available             []driveListEntry         `json:"available"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
	AccountsDegraded      []accountDegradedNotice  `json:"accounts_degraded,omitempty"`
}

func printDriveListJSON(
	w io.Writer,
	configured, available []driveListEntry,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	// Initialize nil slices to empty so JSON renders [] not null.
	if configured == nil {
		configured = []driveListEntry{}
	}

	if available == nil {
		available = []driveListEntry{}
	}

	out := driveListJSONOutput{
		Configured:            configured,
		Available:             available,
		AccountsRequiringAuth: authRequired,
		AccountsDegraded:      degraded,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

// driveLabel returns a human-readable label for a drive list entry.
// Shows "DisplayName (CanonicalID)" when a display name differs from the
// canonical ID, otherwise just the canonical ID.
func driveLabel(e *driveListEntry) string {
	if e.DisplayName != "" && e.DisplayName != e.CanonicalID {
		return fmt.Sprintf("%s (%s)", e.DisplayName, e.CanonicalID)
	}

	return e.CanonicalID
}

func printDriveListText(
	w io.Writer,
	configured, available []driveListEntry,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	if len(configured) == 0 && len(available) == 0 && len(authRequired) == 0 && len(degraded) == 0 {
		return writeln(w, "No drives configured. Run 'onedrive-go login' to get started.")
	}

	return printDriveListSections(w, configured, available, authRequired, degraded)
}

func printConfiguredDrives(w io.Writer, entries []driveListEntry) error {
	if err := writeln(w, "Configured drives:"); err != nil {
		return err
	}

	maxName, maxDir, maxAuth := 0, 0, 0
	for i := range entries {
		label := driveLabel(&entries[i])
		if len(label) > maxName {
			maxName = len(label)
		}

		sd := entries[i].SyncDir
		if sd == "" {
			sd = syncDirNotSet
		}

		if len(sd) > maxDir {
			maxDir = len(sd)
		}

		authLabel := driveAuthLabel(&entries[i])
		if len(authLabel) > maxAuth {
			maxAuth = len(authLabel)
		}
	}

	maxName = max(maxName, minColumnWidth)
	maxDir = max(maxDir, minColumnWidth)
	maxAuth = max(maxAuth, len("AUTH"))

	fmtStr := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%s\n", maxName, maxDir, maxAuth)
	if err := writef(w, fmtStr, "DRIVE", "SYNC DIR", "AUTH", "STATE"); err != nil {
		return err
	}

	for i := range entries {
		syncDir := entries[i].SyncDir
		if syncDir == "" {
			syncDir = syncDirNotSet
		}

		if err := writef(w, fmtStr, driveLabel(&entries[i]), syncDir, driveAuthLabel(&entries[i]), entries[i].State); err != nil {
			return err
		}
	}

	return nil
}

func driveAuthLabel(entry *driveListEntry) string {
	if entry == nil {
		return authStateReady
	}

	if entry.AuthState == authStateAuthenticationNeeded {
		return "required"
	}

	return authStateReady
}

func printDriveListSections(
	w io.Writer,
	configured, available []driveListEntry,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	if err := printConfiguredSection(w, configured); err != nil {
		return err
	}

	if err := printAvailableSection(w, configured, available); err != nil {
		return err
	}

	hasPriorSection := len(configured) > 0 || len(available) > 0
	if err := printAuthRequiredSection(w, hasPriorSection, authRequired); err != nil {
		return err
	}
	if len(authRequired) > 0 {
		hasPriorSection = true
	}

	return printDegradedSection(w, hasPriorSection, degraded)
}

func printConfiguredSection(w io.Writer, configured []driveListEntry) error {
	if len(configured) == 0 {
		return nil
	}

	return printConfiguredDrives(w, configured)
}

func printAvailableSection(
	w io.Writer,
	configured, available []driveListEntry,
) error {
	if len(available) == 0 {
		return nil
	}

	if len(configured) > 0 {
		if err := writeln(w); err != nil {
			return err
		}
	}

	return printAvailableDrives(w, available)
}

func printAuthRequiredSection(
	w io.Writer,
	hasPriorSection bool,
	authRequired []accountAuthRequirement,
) error {
	if len(authRequired) == 0 {
		return nil
	}

	if hasPriorSection {
		if err := writeln(w); err != nil {
			return err
		}
	}

	return printAccountAuthRequirementsText(w, "Authentication required:", authRequired)
}

func printDegradedSection(
	w io.Writer,
	hasPriorSection bool,
	degraded []accountDegradedNotice,
) error {
	if len(degraded) == 0 {
		return nil
	}

	if hasPriorSection {
		if err := writeln(w); err != nil {
			return err
		}
	}

	return printAccountDegradedText(w, "Accounts with degraded live discovery:", degraded)
}

func printAvailableDrives(w io.Writer, entries []driveListEntry) error {
	if err := writeln(w, "Available drives (not configured):"); err != nil {
		return err
	}

	for i := range entries {
		var parts []string
		if entries[i].SiteName != "" {
			parts = append(parts, entries[i].SiteName)
		}

		if ownerLabel := driveListSharedOwnerLabel(&entries[i]); ownerLabel != "" {
			parts = append(parts, ownerLabel)
		}

		label := ""
		if len(parts) > 0 {
			label = fmt.Sprintf(" (%s)", strings.Join(parts, ", "))
		}

		stateDBMarker := ""
		if entries[i].HasStateDB {
			stateDBMarker = " [has sync data]"
		}

		if err := writef(w, "  %s%s%s\n", driveLabel(&entries[i]), label, stateDBMarker); err != nil {
			return err
		}
	}

	return writeln(w, "\nRun 'onedrive-go drive add <canonical-id>' to add a drive.")
}

func driveListSharedOwnerLabel(entry *driveListEntry) string {
	if entry == nil {
		return ""
	}
	if entry.OwnerEmail != "" {
		return "shared by " + entry.OwnerEmail
	}
	if entry.OwnerName != "" {
		return "shared by " + entry.OwnerName
	}
	if entry.OwnerIdentityStatus == sharedOwnerIdentityStatusUnavailableRetryable {
		return sharedOwnerUnavailableRetryLaterText
	}

	return ""
}
