package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
)

// computeSummary aggregates health information across all status accounts.
func computeSummary(accounts []statusAccount) statusSummary {
	var s statusSummary

	for i := range accounts {
		acct := &accounts[i]
		if acct.AuthState == authStateAuthenticationNeeded {
			s.AccountsRequiringAuth++
		}

		for j := range acct.Mounts {
			addMountToSummary(&s, &acct.Mounts[j])
		}
	}

	return s
}

func addMountToSummary(s *statusSummary, mount *statusMount) {
	if s == nil || mount == nil {
		return
	}

	s.TotalMounts++
	switch mount.State {
	case driveStateReady:
		s.Ready++
	case driveStatePaused:
		s.Paused++
	}

	if mount.SyncState != nil {
		s.TotalConditions += mount.SyncState.ConditionCount
		s.TotalRemoteDrift += mount.SyncState.RemoteDrift
		s.TotalRetrying += mount.SyncState.Retrying
	}

	for i := range mount.ChildMounts {
		addMountToSummary(s, &mount.ChildMounts[i])
	}
}

func printStatusJSON(w io.Writer, accounts []statusAccount) error {
	output := statusOutput{
		Accounts: accounts,
		Summary:  computeSummary(accounts),
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(output); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printStatusText(w io.Writer, accounts []statusAccount, history bool) error {
	summary := computeSummary(accounts)
	if err := printSummaryText(w, summary); err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}

	if err := writeln(w); err != nil {
		return err
	}

	for i := range accounts {
		if err := printAccountStatus(w, &accounts[i], i > 0, history); err != nil {
			return err
		}
	}

	return nil
}

func printAccountStatus(w io.Writer, acct *statusAccount, leadingBlank bool, history bool) error {
	if acct == nil {
		return nil
	}

	if leadingBlank {
		if err := writeln(w); err != nil {
			return err
		}
	}

	if err := printAccountHeading(w, acct); err != nil {
		return err
	}
	if err := printAccountAuthDetails(w, acct); err != nil {
		return err
	}
	if err := printAccountDiscoveryDetails(w, acct); err != nil {
		return err
	}
	if len(acct.LiveDrives) > 0 {
		if err := printStatusLiveDrives(w, acct.LiveDrives); err != nil {
			return err
		}
	}

	for i := range acct.Mounts {
		if err := printMountStatus(w, &acct.Mounts[i], history); err != nil {
			return err
		}
	}

	return nil
}

func printAccountHeading(w io.Writer, acct *statusAccount) error {
	if err := writef(w, "Account: %s [%s]\n", statusAccountLabel(acct), acct.DriveType); err != nil {
		return err
	}
	if acct.UserID != "" {
		if err := writef(w, "  User ID: %s\n", acct.UserID); err != nil {
			return err
		}
	}
	if acct.OrgName != "" {
		if err := writef(w, "  Org:   %s\n", acct.OrgName); err != nil {
			return err
		}
	}
	return nil
}

func printAccountAuthDetails(w io.Writer, acct *statusAccount) error {
	if err := writef(w, "  Auth:  %s\n", acct.AuthState); err != nil {
		return err
	}
	if acct.AuthReason != "" {
		if err := writef(w, "  Reason: %s\n", authReasonText(authstate.Reason(acct.AuthReason))); err != nil {
			return err
		}
	}
	if acct.AuthAction != "" {
		if err := writef(w, "  Action: %s\n", acct.AuthAction); err != nil {
			return err
		}
	}
	return nil
}

func printAccountDiscoveryDetails(w io.Writer, acct *statusAccount) error {
	if acct.DegradedReason != "" {
		if err := writef(w, "  Live discovery: %s\n", degradedReasonText(acct.DegradedReason)); err != nil {
			return err
		}
	}
	if acct.DegradedAction != "" {
		if err := writef(w, "  Live action: %s\n", acct.DegradedAction); err != nil {
			return err
		}
	}
	return nil
}

func statusAccountLabel(acct *statusAccount) string {
	if acct == nil {
		return ""
	}

	if acct.DisplayName == "" {
		return acct.Email
	}

	return fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
}

func printMountStatus(w io.Writer, mount *statusMount, history bool) error {
	if mount == nil {
		return nil
	}

	syncDir := mount.SyncDir
	if syncDir == "" {
		syncDir = syncDirNotSet
	}

	layout := statusMountTextLayoutFor(mount)

	if err := writef(w, "%s%s\n", layout.headingIndent, statusMountLabel(mount)); err != nil {
		return err
	}
	if err := writef(w, "%sSync dir:  %s\n", layout.detailIndent, syncDir); err != nil {
		return err
	}
	if err := writef(w, "%sState:     %s\n", layout.detailIndent, mount.State); err != nil {
		return err
	}
	if err := printMountLifecycleStatus(w, layout, mount); err != nil {
		return err
	}
	if mount.SyncState == nil {
		return printChildMountStatuses(w, mount.ChildMounts, history)
	}

	if err := printSyncStateText(w, layout.detailIndent, mount.SyncState, history); err != nil {
		return err
	}

	return printChildMountStatuses(w, mount.ChildMounts, history)
}

func printMountLifecycleStatus(w io.Writer, layout statusMountTextLayout, mount *statusMount) error {
	if mount.NamespaceID != "" {
		if err := writef(w, "%sControl:   Parent drive pause/resume and the OneDrive shortcut\n", layout.detailIndent); err != nil {
			return err
		}
	}
	if mount.StateReason != "" {
		if err := writef(w, "%sReason:    %s\n", layout.detailIndent, mount.StateReason); err != nil {
			return err
		}
	}
	if mount.StateDetail != "" {
		if err := writef(w, "%sNext:      %s\n", layout.detailIndent, mount.StateDetail); err != nil {
			return err
		}
	}
	if err := printMountProtectionStatus(w, layout, mount); err != nil {
		return err
	}
	if mount.RecoveryAction != "" && mount.RecoveryAction != mount.StateDetail {
		if err := writef(w, "%sAction:    %s\n", layout.detailIndent, mount.RecoveryAction); err != nil {
			return err
		}
	}
	if mount.AutoRetry == nil {
		return nil
	}

	retryText := "no"
	if *mount.AutoRetry {
		retryText = "yes"
	}
	return writef(w, "%sAuto retry: %s\n", layout.detailIndent, retryText)
}

func printMountProtectionStatus(w io.Writer, layout statusMountTextLayout, mount *statusMount) error {
	if mount.ProtectedCurrentPath != "" {
		if err := writef(w, "%sProtected current path: %s\n", layout.detailIndent, mount.ProtectedCurrentPath); err != nil {
			return err
		}
	}
	for _, protectedPath := range mount.ProtectedReservedPaths {
		if err := writef(w, "%sReserved path: %s\n", layout.detailIndent, protectedPath); err != nil {
			return err
		}
	}
	for _, replacement := range mount.DeferredReplacements {
		if err := writef(
			w,
			"%sDeferred replacement: %s (%s) - %s\n",
			layout.detailIndent,
			replacement.DisplayName,
			replacement.BindingItemID,
			replacement.Detail,
		); err != nil {
			return err
		}
	}
	return nil
}

type statusMountTextLayout struct {
	headingIndent string
	detailIndent  string
}

func statusMountTextLayoutFor(mount *statusMount) statusMountTextLayout {
	if mount != nil && mount.NamespaceID != "" {
		return statusMountTextLayout{
			headingIndent: "    ",
			detailIndent:  "      ",
		}
	}

	return statusMountTextLayout{
		headingIndent: "  ",
		detailIndent:  "    ",
	}
}

func printChildMountStatuses(w io.Writer, mounts []statusMount, history bool) error {
	for i := range mounts {
		if err := printMountStatus(w, &mounts[i], history); err != nil {
			return err
		}
	}

	return nil
}

func statusMountLabel(mount *statusMount) string {
	if mount == nil {
		return ""
	}

	identity := mount.CanonicalID
	if identity == "" {
		identity = mount.MountID
	}
	if mount.DisplayName == "" || mount.DisplayName == identity {
		return identity
	}

	return fmt.Sprintf("%s (%s)", mount.DisplayName, identity)
}

func printStatusLiveDrives(w io.Writer, drives []statusLiveDrive) error {
	if err := writeln(w, "  Live drives:"); err != nil {
		return err
	}
	for _, drive := range drives {
		if err := writef(w, "    %s (%s)\n", drive.Name, drive.DriveType); err != nil {
			return err
		}
		if err := writef(w, "      ID: %s\n", drive.ID); err != nil {
			return err
		}
		if err := writef(w, "      Quota: %s / %s\n", formatSize(drive.QuotaUsed), formatSize(drive.QuotaTotal)); err != nil {
			return err
		}
	}
	return nil
}

func printSyncStateText(w io.Writer, indent string, ss *syncStateInfo, history bool) error {
	if ss == nil {
		return nil
	}

	if ss.hasPersistentStatusData() || (ss.Perf == nil && ss.PerfUnavailableReason == "") {
		if err := printSyncStateSummaryLines(w, indent, ss); err != nil {
			return err
		}
		if err := printSyncStateStoreLines(w, indent, ss); err != nil {
			return err
		}
		if err := printMountSyncSections(w, indent, ss, history); err != nil {
			return err
		}
	}

	return printStatusPerfText(w, indent, ss)
}

func printSyncStateSummaryLines(w io.Writer, indent string, ss *syncStateInfo) error {
	countLines := []struct {
		count  int
		format string
	}{
		{count: ss.FileCount, format: indent + "Files:     %d\n"},
		{count: ss.RemoteDrift, format: indent + "Remote drift: %d items\n"},
		{count: ss.ConditionCount, format: indent + "Conditions: %d\n"},
		{count: ss.Retrying, format: indent + "Retrying:  %d items\n"},
	}
	for i := range countLines {
		if err := writeOptionalStatusCountLine(w, countLines[i].count, countLines[i].format); err != nil {
			return err
		}
	}

	return nil
}

func printSyncStateStoreLines(w io.Writer, _ string, ss *syncStateInfo) error {
	_ = ss
	return nil
}

func writeOptionalStatusCountLine(w io.Writer, count int, format string) error {
	if count <= 0 {
		return nil
	}

	return writef(w, format, count)
}

func printSummaryText(w io.Writer, s statusSummary) error {
	var parts []string

	if s.Ready > 0 {
		parts = append(parts, fmt.Sprintf("%d ready", s.Ready))
	}

	if s.Paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", s.Paused))
	}

	if s.AccountsRequiringAuth > 0 {
		parts = append(parts, fmt.Sprintf("%d accounts requiring auth", s.AccountsRequiringAuth))
	}

	stateStr := strings.Join(parts, ", ")

	extra := fmt.Sprintf("%d conditions", s.TotalConditions)

	if s.TotalRemoteDrift > 0 {
		extra += fmt.Sprintf(", %d remote drift", s.TotalRemoteDrift)
	}

	if s.TotalRetrying > 0 {
		extra += fmt.Sprintf(", %d retrying", s.TotalRetrying)
	}

	if stateStr == "" {
		return writef(w, "Summary: %d mounts, %s\n", s.TotalMounts, extra)
	}

	return writef(w, "Summary: %d mounts (%s), %s\n", s.TotalMounts, stateStr, extra)
}
