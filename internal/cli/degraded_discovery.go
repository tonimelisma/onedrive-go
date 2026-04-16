package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const (
	driveCatalogUnavailableReason    = "drive_catalog_unavailable"
	sharedDiscoveryUnavailableReason = "shared_discovery_unavailable"
	graphMeDrivesEndpoint            = "/me/drives"
)

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
		return "This command could not complete live drive discovery for this account."
	case sharedDiscoveryUnavailableReason:
		return "This command could not complete live shared-item discovery for this account."
	default:
		return ""
	}
}

func degradedAction(reason string) string {
	switch reason {
	case driveCatalogUnavailableReason:
		return "No manual repair is required. Retry 'onedrive-go whoami' or " +
			"'onedrive-go drive list' later; this warning clears automatically " +
			"after a successful discovery. Configured drives and direct " +
			"commands against known drives may still work."
	case sharedDiscoveryUnavailableReason:
		return "No manual repair is required. Retry 'onedrive-go shared' or " +
			"'onedrive-go drive list' later; this warning clears automatically " +
			"after a successful discovery. Configured drives and direct commands " +
			"against already-known shared targets may still work."
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

func sharedDiscoveryDegradedNotice(email, displayName, driveType string) accountDegradedNotice {
	return accountDegradedNotice{
		Email:       email,
		DisplayName: displayName,
		DriveType:   driveType,
		Reason:      sharedDiscoveryUnavailableReason,
		Action:      degradedAction(sharedDiscoveryUnavailableReason),
	}
}

func mergeDegradedNotices(groups ...[]accountDegradedNotice) []accountDegradedNotice {
	type degradedKey struct {
		email  string
		reason string
	}

	merged := make(map[degradedKey]accountDegradedNotice)

	for _, group := range groups {
		for i := range group {
			if group[i].Email == "" {
				continue
			}

			key := degradedKey{email: group[i].Email, reason: group[i].Reason}

			if existing, ok := merged[key]; ok {
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
				merged[key] = existing
				continue
			}

			merged[key] = group[i]
		}
	}

	result := make([]accountDegradedNotice, 0, len(merged))
	for _, item := range merged {
		result = append(result, item)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Email != result[j].Email {
			return result[i].Email < result[j].Email
		}

		return result[i].Reason < result[j].Reason
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

func degradedDiscoveryLogAttrs(account, endpoint string, err error) []any {
	attrs := []any{
		"account", account,
		"endpoint", endpoint,
		"error", err,
	}

	evidence, ok := graph.ExtractQuirkEvidence(err)
	if !ok {
		return attrs
	}

	return append(attrs,
		"graph_quirk", evidence.Quirk,
		"graph_quirk_attempt_count", len(evidence.Attempts),
		"graph_quirk_attempts", evidence.Attempts,
	)
}
