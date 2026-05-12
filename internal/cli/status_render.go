package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// computeSummary aggregates health information across all status accounts.
func computeSummary(accounts []statusAccount) statusSummary {
	accounts = normalizeStatusAccounts(accounts)
	var s statusSummary

	for i := range accounts {
		acct := &accounts[i]
		if acct.AuthState == authStateAuthenticationNeeded {
			s.AccountsRequiringAuth++
		}

		for j := range acct.Drives {
			addDriveToSummary(&s, &acct.Drives[j], false)
		}
	}

	return s
}

func addDriveToSummary(s *statusSummary, drive *statusDrive, sharedFolder bool) {
	if s == nil || drive == nil {
		return
	}

	if sharedFolder {
		s.TotalSharedFolders++
	} else {
		s.TotalDrives++
	}
	s.TotalMounts++

	switch drive.State {
	case driveStateReady:
		s.Ready++
	case driveStateSyncing:
		s.Syncing++
	case driveStatePending:
		s.Pending++
	case driveStatePaused:
		s.Paused++
	case driveStateIssues:
		s.Issues++
	case driveStateUnavailable:
		s.Unavailable++
	}

	if drive.SyncState != nil {
		s.TotalConditions += drive.SyncState.ConditionCount
		s.TotalRemoteDrift += drive.SyncState.RemoteDrift
		s.TotalRetrying += drive.SyncState.Retrying
	}
	if drive.RuntimeOwner != "" && s.RuntimeOwner == "" {
		s.RuntimeOwner = drive.RuntimeOwner
	}
	if drive.RuntimeState == statusRuntimeStateActive {
		s.RuntimeActiveDrives++
		s.RuntimeActiveMounts++
	}

	for i := range drive.SharedFolders {
		addDriveToSummary(s, &drive.SharedFolders[i], true)
	}
}

func printStatusJSON(w io.Writer, accounts []statusAccount) error {
	accounts = normalizeStatusAccounts(accounts)
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
	accounts = normalizeStatusAccounts(accounts)
	summary := computeSummary(accounts)
	if shouldPrintStatusSummary(&summary, len(accounts)) {
		if err := printSummaryText(w, &summary); err != nil {
			return err
		}
		if err := writeln(w); err != nil {
			return err
		}
	}

	for i := range accounts {
		if err := printAccountStatus(w, &accounts[i], i > 0, history); err != nil {
			return err
		}
	}

	return nil
}

func normalizeStatusAccounts(accounts []statusAccount) []statusAccount {
	if len(accounts) == 0 {
		return accounts
	}

	normalized := make([]statusAccount, len(accounts))
	copy(normalized, accounts)
	for i := range normalized {
		if len(normalized[i].Drives) == 0 && len(normalized[i].Mounts) > 0 {
			normalized[i].Drives = append([]statusDrive(nil), normalized[i].Mounts...)
		} else {
			normalized[i].Drives = append([]statusDrive(nil), normalized[i].Drives...)
		}
		normalizeStatusDrives(normalized[i].Drives)
		if normalized[i].SignInRequired == nil {
			setAccountSignInRequired(&normalized[i])
		}
	}
	return normalized
}

func normalizeStatusDrives(drives []statusDrive) {
	for i := range drives {
		if drives[i].InternalID == "" {
			if drives[i].CanonicalID != "" {
				drives[i].InternalID = drives[i].CanonicalID
			} else {
				drives[i].InternalID = drives[i].MountID
			}
		}
		if drives[i].Name == "" {
			drives[i].Name = drives[i].DisplayName
		}
		if drives[i].Name == "" {
			drives[i].Name = statusFallbackDriveName(&drives[i])
		}
		if drives[i].Folder == "" {
			drives[i].Folder = drives[i].SyncDir
		}
		if drives[i].Kind == "" {
			if drives[i].NamespaceID != "" || drives[i].ProjectionKind == statusProjectionChild {
				drives[i].Kind = statusDriveKindSharedFolder
			} else {
				drives[i].Kind = statusDriveKindGeneric
			}
		}
		if len(drives[i].SharedFolders) == 0 && len(drives[i].ChildMounts) > 0 {
			drives[i].SharedFolders = append([]statusDrive(nil), drives[i].ChildMounts...)
		} else {
			drives[i].SharedFolders = append([]statusDrive(nil), drives[i].SharedFolders...)
		}
		normalizeStatusDrives(drives[i].SharedFolders)
		finalizeStatusDriveState(&drives[i])
	}
}

func shouldPrintStatusSummary(summary *statusSummary, accountCount int) bool {
	if summary == nil {
		return false
	}
	if accountCount > 1 || summary.TotalDrives > 1 || summary.TotalSharedFolders > 0 {
		return true
	}

	return false
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
	if acct.SignInRequired == nil {
		setAccountSignInRequired(acct)
	}

	if err := printAccountHeading(w, acct); err != nil {
		return err
	}
	if err := printAccountSignIn(w, acct); err != nil {
		return err
	}

	drives := acct.Drives
	if len(drives) == 0 && len(acct.Mounts) > 0 {
		drives = append([]statusDrive(nil), acct.Mounts...)
		normalizeStatusDrives(drives)
	}
	for i := range drives {
		if err := printDriveStatus(w, &drives[i], history, 0); err != nil {
			return err
		}
	}

	return nil
}

func printAccountHeading(w io.Writer, acct *statusAccount) error {
	if err := writef(w, "Account: %s\n", statusAccountLabel(acct)); err != nil {
		return err
	}
	if acct.OrgName != "" {
		if err := writef(w, "  Organization: %s\n", acct.OrgName); err != nil {
			return err
		}
	}
	return nil
}

func printAccountSignIn(w io.Writer, acct *statusAccount) error {
	if acct == nil || acct.SignInRequired == nil {
		return nil
	}
	if acct.SignInRequired.Reason != "" {
		if err := writef(w, "  Sign-in required: %s\n", acct.SignInRequired.Reason); err != nil {
			return err
		}
	}
	if acct.SignInRequired.Action != "" {
		if err := writef(w, "  Action: %s\n", acct.SignInRequired.Action); err != nil {
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

	return fmt.Sprintf("%s <%s>", acct.DisplayName, acct.Email)
}

func printDriveStatus(w io.Writer, drive *statusDrive, history bool, depth int) error {
	if drive == nil {
		return nil
	}

	layout := statusDriveTextLayoutFor(depth)
	if err := writef(w, "%s%s\n", layout.headingIndent, statusDriveLabel(drive)); err != nil {
		return err
	}
	if err := printDriveFolder(w, layout, drive); err != nil {
		return err
	}
	if err := printDriveStorage(w, layout, drive); err != nil {
		return err
	}
	if err := printDriveState(w, layout, drive); err != nil {
		return err
	}
	if err := printDriveLifecycleStatus(w, layout, drive); err != nil {
		return err
	}
	if drive.SyncState != nil {
		if err := printSyncStateText(w, layout.detailIndent, drive.SyncState, history); err != nil {
			return err
		}
	}
	return printSharedFolderStatuses(w, layout, drive.SharedFolders, history, depth)
}

func printDriveFolder(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	folder := drive.Folder
	if folder == "" {
		folder = syncDirNotSet
	}
	return writef(w, "%sFolder: %s\n", layout.detailIndent, folder)
}

func printDriveStorage(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	if drive.Storage == nil {
		return nil
	}
	return writef(w, "%sStorage: %s\n", layout.detailIndent, formatStatusStorage(drive.Storage))
}

func printDriveState(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	if drive.State == driveStateReady {
		return nil
	}
	return writef(w, "%sStatus: %s\n", layout.detailIndent, statusDriveStateLabel(drive.State))
}

func printSharedFolderStatuses(
	w io.Writer,
	layout statusDriveTextLayout,
	sharedFolders []statusDrive,
	history bool,
	depth int,
) error {
	if len(sharedFolders) == 0 {
		return nil
	}
	if err := writef(w, "%sShared folders:\n", layout.detailIndent); err != nil {
		return err
	}
	for i := range sharedFolders {
		if err := printDriveStatus(w, &sharedFolders[i], history, depth+1); err != nil {
			return err
		}
	}

	return nil
}

func printDriveLifecycleStatus(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	if drive.NamespaceID != "" {
		if err := writef(w, "%sShared folder: Managed through the parent drive\n", layout.detailIndent); err != nil {
			return err
		}
	}
	if drive.StateReason != "" {
		if err := writef(w, "%sReason: %s\n", layout.detailIndent, drive.StateReason); err != nil {
			return err
		}
	}
	if drive.StateDetail != "" {
		if err := writef(w, "%sNext: %s\n", layout.detailIndent, drive.StateDetail); err != nil {
			return err
		}
	}
	if err := printDriveProtectionStatus(w, layout, drive); err != nil {
		return err
	}
	if drive.WaitingReplacement != "" {
		if err := writef(w, "%sWaiting replacement: %s\n", layout.detailIndent, drive.WaitingReplacement); err != nil {
			return err
		}
	}
	if drive.RecoveryAction != "" && drive.RecoveryAction != drive.StateDetail {
		if err := writef(w, "%sAction: %s\n", layout.detailIndent, drive.RecoveryAction); err != nil {
			return err
		}
	}
	if drive.AutoRetry == nil {
		return nil
	}

	retryText := "no"
	if *drive.AutoRetry {
		retryText = "yes"
	}
	return writef(w, "%sAuto retry: %s\n", layout.detailIndent, retryText)
}

func printDriveProtectionStatus(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	if drive.ProtectedCurrentPath != "" {
		if err := writef(w, "%sProtected current path: %s\n", layout.detailIndent, drive.ProtectedCurrentPath); err != nil {
			return err
		}
	}
	for _, protectedPath := range drive.ProtectedReservedPaths {
		if err := writef(w, "%sReserved path: %s\n", layout.detailIndent, protectedPath); err != nil {
			return err
		}
	}
	return nil
}

type statusDriveTextLayout struct {
	headingIndent string
	detailIndent  string
}

func statusDriveTextLayoutFor(depth int) statusDriveTextLayout {
	headingSpaces := strings.Repeat("  ", depth+1)
	detailSpaces := strings.Repeat("  ", depth+2)
	return statusDriveTextLayout{
		headingIndent: headingSpaces,
		detailIndent:  detailSpaces,
	}
}

func statusDriveLabel(drive *statusDrive) string {
	if drive == nil {
		return ""
	}
	if drive.Name != "" {
		return drive.Name
	}
	if drive.DisplayName != "" {
		return drive.DisplayName
	}
	return statusFallbackDriveName(drive)
}

func statusFallbackDriveName(drive *statusDrive) string {
	if drive == nil {
		return statusDriveNameOneDrive
	}
	identity := drive.CanonicalID
	if identity == "" {
		identity = drive.InternalID
	}
	switch {
	case drive.Kind == statusDriveKindPersonalOneDrive || strings.HasPrefix(identity, "personal:"):
		return "Personal OneDrive"
	case drive.Kind == statusDriveKindBusinessOneDrive || strings.HasPrefix(identity, "business:"):
		return "Work OneDrive"
	case drive.Kind == statusDriveKindSharePoint || strings.HasPrefix(identity, "sharepoint:"):
		return "SharePoint library"
	case drive.Kind == statusDriveKindSharedFolder ||
		drive.ProjectionKind == statusProjectionChild ||
		strings.HasPrefix(identity, "shared:"):
		return statusDriveNameSharedFolder
	}
	return statusDriveNameOneDrive
}

func printMountStatus(w io.Writer, mount *statusMount) error {
	if mount == nil {
		return nil
	}
	drives := []statusDrive{*mount}
	normalizeStatusDrives(drives)
	return printDriveStatus(w, &drives[0], false, 0)
}

func statusMountLabel(mount *statusMount) string {
	if mount == nil {
		return ""
	}
	drives := []statusDrive{*mount}
	normalizeStatusDrives(drives)
	return statusDriveLabel(&drives[0])
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
		if err := printDriveSyncSections(w, indent, ss, history); err != nil {
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
		{count: ss.FileCount, format: indent + "Files: %d\n"},
		{count: ss.RemoteDrift, format: indent + "Remote changes: %d %s\n"},
		{count: ss.Retrying, format: indent + "Retrying: %d %s\n"},
	}
	for i := range countLines {
		if countLines[i].count <= 0 {
			continue
		}
		if strings.Contains(countLines[i].format, "%s") {
			if err := writef(w, countLines[i].format, countLines[i].count, itemNoun(countLines[i].count)); err != nil {
				return err
			}
			continue
		}
		if err := writef(w, countLines[i].format, countLines[i].count); err != nil {
			return err
		}
	}

	return nil
}

func printSyncStateStoreLines(w io.Writer, _ string, ss *syncStateInfo) error {
	_ = ss
	return nil
}

func printSummaryText(w io.Writer, s *statusSummary) error {
	if s == nil {
		return nil
	}

	var parts []string

	if s.Ready > 0 {
		parts = append(parts, formatStatusCount(s.Ready, "up to date", "up to date"))
	}
	if s.Syncing > 0 {
		parts = append(parts, formatStatusCount(s.Syncing, "syncing", "syncing"))
	}
	if s.Pending > 0 {
		parts = append(parts, formatStatusCount(s.Pending, "pending", "pending"))
	}
	if s.Paused > 0 {
		parts = append(parts, formatStatusCount(s.Paused, "paused", "paused"))
	}
	if s.Issues > 0 {
		parts = append(parts, formatStatusCount(s.Issues, "with issues", "with issues"))
	}
	if s.Unavailable > 0 {
		parts = append(parts, formatStatusCount(s.Unavailable, "unavailable", "unavailable"))
	}
	if s.AccountsRequiringAuth > 0 {
		parts = append(parts, formatStatusCount(s.AccountsRequiringAuth, "account needs sign-in", "accounts need sign-in"))
	}

	prefix := formatStatusCount(s.TotalDrives, "drive", "drives")
	if s.TotalSharedFolders > 0 {
		prefix += ", " + formatStatusCount(s.TotalSharedFolders, "shared folder", "shared folders")
	}
	if len(parts) == 0 {
		return writef(w, "Status: %s\n", prefix)
	}
	return writef(w, "Status: %s: %s\n", prefix, strings.Join(parts, ", "))
}

func formatStatusCount(count int, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return fmt.Sprintf("%d %s", count, label)
}

func statusDriveStateLabel(state string) string {
	switch state {
	case driveStateReady:
		return "Up to date"
	case driveStateSyncing:
		return "Syncing"
	case driveStatePending:
		return "Pending changes"
	case driveStatePaused:
		return "Paused"
	case driveStateIssues:
		return "Issues"
	case driveStateUnavailable:
		return "Unavailable"
	default:
		return state
	}
}

func formatStatusStorage(storage *statusStorage) string {
	if storage == nil {
		return ""
	}
	if storage.Total != "" {
		return fmt.Sprintf("%s of %s used", storage.Used, storage.Total)
	}
	return storage.Used + " used"
}
