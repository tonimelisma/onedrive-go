package cli

import (
	"fmt"
	"io"
	"strings"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"
)

// computeSummary aggregates health information across the accounts the
// status builder made.
func computeSummary(accounts []statusAccount) statusSummary {
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
		s.TotalLocalDrift += drive.SyncState.LocalDrift
		s.TotalRetrying += drive.SyncState.Retrying
	}
	if drive.RuntimeOwner != "" && s.RuntimeOwner == "" {
		s.RuntimeOwner = drive.RuntimeOwner
	}
	if drive.RuntimeState == statusRuntimeStateActive {
		s.RuntimeActiveDrives++
	}

	for i := range drive.SharedFolders {
		addDriveToSummary(s, &drive.SharedFolders[i], true)
	}
}

// newStatusOutput is the status document for the accounts the builder made,
// with their summary. Both printers start from it. The two lists status
// prints without omitempty are never nil, so an empty one prints [] rather
// than null: the accounts, and each account's drives.
func newStatusOutput(accounts []statusAccount) statusOutput {
	if accounts == nil {
		accounts = []statusAccount{}
	}
	for i := range accounts {
		if accounts[i].Drives == nil {
			accounts[i].Drives = []statusDrive{}
		}
	}

	return statusOutput{Accounts: accounts, Summary: computeSummary(accounts)}
}

func printStatusJSON(w io.Writer, accounts []statusAccount) error {
	return writeJSONDocument(w, newStatusOutput(accounts))
}

func printStatusText(w io.Writer, accounts []statusAccount) error {
	status := newStatusOutput(accounts)
	if shouldPrintStatusSummary(&status.Summary, len(status.Accounts)) {
		if err := printSummaryText(w, &status.Summary); err != nil {
			return err
		}
		if err := writeln(w); err != nil {
			return err
		}
	}

	for i := range status.Accounts {
		if err := printAccountStatus(w, &status.Accounts[i], i > 0); err != nil {
			return err
		}
	}

	return nil
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

func printAccountStatus(w io.Writer, acct *statusAccount, leadingBlank bool) error {
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
	if err := printAccountSignIn(w, acct); err != nil {
		return err
	}

	for i := range acct.Drives {
		if err := printDriveStatus(w, &acct.Drives[i], 0); err != nil {
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

func printDriveStatus(w io.Writer, drive *statusDrive, depth int) error {
	if drive == nil {
		return nil
	}

	layout := statusDriveTextLayoutFor(depth)
	if err := writef(w, "%s%s\n", layout.headingIndent, drive.Name); err != nil {
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
		if err := printSyncStateText(w, layout.detailIndent, drive.SyncState); err != nil {
			return err
		}
	}
	return printSharedFolderStatuses(w, layout, drive.SharedFolders, depth)
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

// printDriveState always names the drive's state, "Up to date" included: with
// a single drive there is no summary line, so omitting it left status silent
// on whether the drive was synced. A running watch is named too, because "up
// to date" alone does not say whether changes will keep syncing.
func printDriveState(w io.Writer, layout statusDriveTextLayout, drive *statusDrive) error {
	label := statusDriveStateLabel(drive.State)
	if label == drive.State && drive.StateLabel != "" {
		label = drive.StateLabel
	}
	if drive.RuntimeState == statusRuntimeStateActive && drive.RuntimeOwner == string(synccontrol.OwnerModeWatch) {
		label += ", watching for changes"
	}
	return writef(w, "%sStatus: %s\n", layout.detailIndent, label)
}

func printSharedFolderStatuses(
	w io.Writer,
	layout statusDriveTextLayout,
	sharedFolders []statusDrive,
	depth int,
) error {
	if len(sharedFolders) == 0 {
		return nil
	}
	if err := writef(w, "%sShared folders:\n", layout.detailIndent); err != nil {
		return err
	}
	for i := range sharedFolders {
		if err := printDriveStatus(w, &sharedFolders[i], depth+1); err != nil {
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
	// The machine token (StateReason) is for --json; people read the plain
	// sentence, and the raw diagnostic only when they asked for it.
	if drive.StateDetail != "" {
		if err := writef(w, "%sReason: %s\n", layout.detailIndent, drive.StateDetail); err != nil {
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
	if drive.StateDebugDetail != "" {
		if err := writef(w, "%sDetail: %s\n", layout.detailIndent, drive.StateDebugDetail); err != nil {
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

func printSyncStateText(w io.Writer, indent string, ss *syncStateInfo) error {
	if ss == nil {
		return nil
	}

	if ss.hasPersistentStatusData() || (ss.Perf == nil && ss.PerfUnavailableReason == "") {
		if err := printSyncStateSummaryLines(w, indent, ss); err != nil {
			return err
		}
		if err := printDriveSyncSections(w, indent, ss); err != nil {
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
		{count: ss.LocalDrift, format: indent + "Local changes: %s\n"},
		{count: ss.RemoteDrift, format: indent + "Remote changes: %s\n"},
		{count: ss.Retrying, format: indent + "Retrying: %s\n"},
	}
	for i := range countLines {
		if countLines[i].count <= 0 {
			continue
		}
		if strings.Contains(countLines[i].format, "%s") {
			if err := writef(w, countLines[i].format, countItems(countLines[i].count)); err != nil {
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

func printSummaryText(w io.Writer, s *statusSummary) error {
	if s == nil {
		return nil
	}

	// The drives in each state: a state reads the same for one drive and many.
	var parts []string
	for _, state := range []struct {
		count int
		label string
	}{
		{count: s.Ready, label: "up to date"},
		{count: s.Syncing, label: "syncing"},
		{count: s.Pending, label: "pending"},
		{count: s.Paused, label: "paused"},
		{count: s.Issues, label: "with issues"},
		{count: s.Unavailable, label: "unavailable"},
	} {
		if state.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", state.count, state.label))
		}
	}
	if s.AccountsRequiringAuth > 0 {
		needSignIn := " need sign-in"
		if s.AccountsRequiringAuth == 1 {
			needSignIn = " needs sign-in"
		}
		parts = append(parts, countOf(s.AccountsRequiringAuth, "account")+needSignIn)
	}

	prefix := countOf(s.TotalDrives, "drive")
	if s.TotalSharedFolders > 0 {
		prefix += ", " + countOf(s.TotalSharedFolders, "shared folder")
	}
	if len(parts) == 0 {
		return writef(w, "Status: %s\n", prefix)
	}
	return writef(w, "Status: %s: %s\n", prefix, strings.Join(parts, ", "))
}

func statusDriveStateLabel(state string) string {
	switch state {
	case driveStateReady:
		return "Up to date"
	case driveStateSyncing:
		return driveStateLabelSyncing
	case driveStatePending:
		return "Pending changes"
	case driveStatePaused:
		return "Paused"
	case driveStateNotSynced:
		return "Not synced yet. Run 'onedrive-go sync' to start"
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
