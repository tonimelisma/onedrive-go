package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// whoamiOutput is the JSON schema for `whoami --json`.
type whoamiOutput struct {
	User                  *whoamiUser              `json:"user,omitempty"`
	Drives                []whoamiDrive            `json:"drives,omitempty"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth,omitempty"`
	AccountsDegraded      []accountDegradedNotice  `json:"accounts_degraded,omitempty"`
}

type whoamiUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type whoamiDrive struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DriveType  string `json:"drive_type"`
	QuotaUsed  int64  `json:"quota_used"`
	QuotaTotal int64  `json:"quota_total"`
}

func printWhoamiJSON(
	w io.Writer,
	user *graph.User,
	drives []graph.Drive,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	out := whoamiOutput{
		AccountsRequiringAuth: authRequired,
		AccountsDegraded:      degraded,
	}

	if user != nil {
		out.User = &whoamiUser{
			ID:          user.ID,
			DisplayName: user.DisplayName,
			Email:       user.Email,
		}
		out.Drives = make([]whoamiDrive, 0, len(drives))

		for i := range drives {
			out.Drives = append(out.Drives, whoamiDrive{
				ID:         drives[i].ID.String(),
				Name:       drives[i].Name,
				DriveType:  drives[i].DriveType,
				QuotaUsed:  drives[i].QuotaUsed,
				QuotaTotal: drives[i].QuotaTotal,
			})
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printWhoamiText(
	w io.Writer,
	user *graph.User,
	drives []graph.Drive,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	hasPriorSection := false

	if user != nil {
		if err := printWhoamiIdentityText(w, user, drives); err != nil {
			return err
		}
		hasPriorSection = true
	}

	if len(authRequired) > 0 {
		if hasPriorSection {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := printAccountAuthRequirementsText(w, "Accounts requiring authentication:", authRequired); err != nil {
			return err
		}
		hasPriorSection = true
	}

	if len(degraded) > 0 {
		if hasPriorSection {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := printAccountDegradedText(w, "Accounts with degraded live discovery:", degraded); err != nil {
			return err
		}
	}

	return nil
}

func printWhoamiIdentityText(w io.Writer, user *graph.User, drives []graph.Drive) error {
	if err := writef(w, "User:  %s (%s)\n", user.DisplayName, user.Email); err != nil {
		return err
	}
	if err := writef(w, "ID:    %s\n", user.ID); err != nil {
		return err
	}

	for i := range drives {
		if err := writef(w, "\nDrive: %s (%s)\n", drives[i].Name, drives[i].DriveType); err != nil {
			return err
		}
		if err := writef(w, "  ID:    %s\n", drives[i].ID); err != nil {
			return err
		}
		if err := writef(w, "  Quota: %s / %s\n", formatSize(drives[i].QuotaUsed), formatSize(drives[i].QuotaTotal)); err != nil {
			return err
		}
	}

	return nil
}

func printWhoamiAuthRequiredText(w io.Writer, authRequired []accountAuthRequirement) error {
	return printAccountAuthRequirementsText(w, "Accounts requiring authentication:", authRequired)
}
