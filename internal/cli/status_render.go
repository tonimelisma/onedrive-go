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

		for j := range acct.Drives {
			d := &acct.Drives[j]
			s.TotalDrives++

			switch d.State {
			case driveStateReady:
				s.Ready++
			case driveStatePaused:
				s.Paused++
			}

			if d.SyncState != nil {
				s.TotalIssues += d.SyncState.IssueCount
				s.TotalRemoteDrift += d.SyncState.RemoteDrift
				s.TotalRetrying += d.SyncState.Retrying
			}
		}
	}

	return s
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

	for _, drive := range acct.Drives {
		if err := printDriveStatus(w, drive, history); err != nil {
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

func printDriveStatus(w io.Writer, drive statusDrive, history bool) error {
	syncDir := drive.SyncDir
	if syncDir == "" {
		syncDir = syncDirNotSet
	}

	if err := writef(w, "  %s\n", statusDriveLabel(drive)); err != nil {
		return err
	}
	if err := writef(w, "    Sync dir:  %s\n", syncDir); err != nil {
		return err
	}
	if err := writef(w, "    State:     %s\n", drive.State); err != nil {
		return err
	}
	if drive.SyncState == nil {
		return nil
	}

	return printSyncStateText(w, drive.SyncState, history)
}

func statusDriveLabel(drive statusDrive) string {
	if drive.DisplayName == "" || drive.DisplayName == drive.CanonicalID {
		return drive.CanonicalID
	}

	return fmt.Sprintf("%s (%s)", drive.DisplayName, drive.CanonicalID)
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

func printSyncStateText(w io.Writer, ss *syncStateInfo, history bool) error {
	if ss == nil {
		return nil
	}

	if ss.hasPersistentStatusData() || (ss.Perf == nil && ss.PerfUnavailableReason == "") {
		if err := printSyncStateSummaryLines(w, ss); err != nil {
			return err
		}
		if err := printSyncStateStoreLines(w, ss); err != nil {
			return err
		}
		if err := printDriveSyncSections(w, ss, history); err != nil {
			return err
		}
	}

	return printStatusPerfText(w, ss)
}

func printSyncStateSummaryLines(w io.Writer, ss *syncStateInfo) error {
	if err := printStatusLastSyncLine(w, ss); err != nil {
		return err
	}
	if ss.LastSyncDuration != "" {
		if err := writef(w, "    Duration:  %sms\n", ss.LastSyncDuration); err != nil {
			return err
		}
	}

	countLines := []struct {
		count  int
		format string
	}{
		{count: ss.FileCount, format: "    Files:     %d\n"},
		{count: ss.RemoteDrift, format: "    Remote drift: %d items\n"},
		{count: ss.IssueCount, format: "    Issues:    %d\n"},
		{count: ss.Retrying, format: "    Retrying:  %d items\n"},
	}
	for i := range countLines {
		if err := writeOptionalStatusCountLine(w, countLines[i].count, countLines[i].format); err != nil {
			return err
		}
	}

	return nil
}

func printSyncStateStoreLines(w io.Writer, ss *syncStateInfo) error {
	valueLines := []struct {
		value  string
		format string
	}{
		{value: ss.LastError, format: "    Last error: %s\n"},
	}
	for i := range valueLines {
		if err := writeOptionalStatusValueLine(w, valueLines[i].value, valueLines[i].format); err != nil {
			return err
		}
	}

	return nil
}

func printStatusLastSyncLine(w io.Writer, ss *syncStateInfo) error {
	if ss.LastSyncTime == "" {
		return writef(w, "    Last sync: never\n")
	}

	return writef(w, "    Last sync: %s\n", ss.LastSyncTime)
}

func writeOptionalStatusCountLine(w io.Writer, count int, format string) error {
	if count <= 0 {
		return nil
	}

	return writef(w, format, count)
}

func writeOptionalStatusValueLine(w io.Writer, value string, format string) error {
	if value == "" {
		return nil
	}

	return writef(w, format, value)
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

	extra := fmt.Sprintf("%d issues", s.TotalIssues)

	if s.TotalRemoteDrift > 0 {
		extra += fmt.Sprintf(", %d remote drift", s.TotalRemoteDrift)
	}

	if s.TotalRetrying > 0 {
		extra += fmt.Sprintf(", %d retrying", s.TotalRetrying)
	}

	if stateStr == "" {
		return writef(w, "Summary: %d drives, %s\n", s.TotalDrives, extra)
	}

	return writef(w, "Summary: %d drives (%s), %s\n", s.TotalDrives, stateStr, extra)
}
