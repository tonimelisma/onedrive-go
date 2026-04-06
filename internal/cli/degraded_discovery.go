package cli

import (
	"fmt"
	"io"
	"sort"
)

const driveCatalogUnavailableReason = "drive_catalog_unavailable"

type accountDegradedNotice struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	DriveType   string `json:"drive_type,omitempty"`
	Reason      string `json:"reason"`
	Action      string `json:"action,omitempty"`
}

func degradedReasonText(reason string) string {
	switch reason {
	case driveCatalogUnavailableReason:
		return "Live drive discovery is temporarily unavailable even though the saved login still works."
	default:
		return ""
	}
}

func degradedAction(reason string) string {
	switch reason {
	case driveCatalogUnavailableReason:
		return "Retry 'onedrive-go whoami' or 'onedrive-go drive list' in a few seconds. " +
			"Direct commands against configured drives may still work."
	default:
		return ""
	}
}

func driveCatalogDegradedNotice(email, displayName, driveType string) accountDegradedNotice {
	return accountDegradedNotice{
		Email:       email,
		DisplayName: displayName,
		DriveType:   driveType,
		Reason:      driveCatalogUnavailableReason,
		Action:      degradedAction(driveCatalogUnavailableReason),
	}
}

func mergeDegradedNotices(groups ...[]accountDegradedNotice) []accountDegradedNotice {
	merged := make(map[string]accountDegradedNotice)

	for _, group := range groups {
		for i := range group {
			if group[i].Email == "" {
				continue
			}

			if existing, ok := merged[group[i].Email]; ok {
				if existing.DisplayName == "" {
					existing.DisplayName = group[i].DisplayName
				}
				if existing.DriveType == "" {
					existing.DriveType = group[i].DriveType
				}
				if existing.Reason == "" {
					existing.Reason = group[i].Reason
				}
				if existing.Action == "" {
					existing.Action = group[i].Action
				}
				merged[group[i].Email] = existing
				continue
			}

			merged[group[i].Email] = group[i]
		}
	}

	result := make([]accountDegradedNotice, 0, len(merged))
	for _, item := range merged {
		result = append(result, item)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Email < result[j].Email
	})

	return result
}

func printAccountDegradedText(w io.Writer, header string, items []accountDegradedNotice) error {
	if len(items) == 0 {
		return nil
	}

	if err := writeln(w, header); err != nil {
		return err
	}

	for _, acct := range items {
		nameLabel := acct.Email
		if acct.DisplayName != "" {
			nameLabel = fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
		}

		if err := writef(w, "  %s — %s\n", nameLabel, acct.DriveType); err != nil {
			return err
		}
		if reasonText := degradedReasonText(acct.Reason); reasonText != "" {
			if err := writef(w, "    %s\n", reasonText); err != nil {
				return err
			}
		}
		if acct.Action != "" {
			if err := writef(w, "    %s\n", acct.Action); err != nil {
				return err
			}
		}
	}

	return nil
}
