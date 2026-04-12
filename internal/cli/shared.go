package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type sharedListItem struct {
	Selector      string `json:"selector"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountEmail  string `json:"account_email"`
	SharedByName  string `json:"shared_by_name,omitempty"`
	SharedByEmail string `json:"shared_by_email,omitempty"`
	ModifiedAt    string `json:"modified_at"`
	Size          int64  `json:"size,omitempty"`
	RemoteDriveID string `json:"remote_drive_id"`
	RemoteItemID  string `json:"remote_item_id"`
}

type sharedListJSONOutput struct {
	Items                 []sharedListItem         `json:"items"`
	AccountsRequiringAuth []accountAuthRequirement `json:"accounts_requiring_auth"`
	AccountsDegraded      []accountDegradedNotice  `json:"accounts_degraded,omitempty"`
}

func newSharedCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "shared",
		Short:       "List files and folders shared with you",
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runShared,
		Args:        cobra.NoArgs,
	}
}

func runShared(cmd *cobra.Command, _ []string) error {
	return runSharedList(cmd.Context(), mustCLIContext(cmd.Context()))
}

func runSharedList(ctx context.Context, cc *CLIContext) error {
	snapshot, err := loadAccountCatalogSnapshotWithBestEffortIdentityRefresh(ctx, cc)
	if err != nil {
		return err
	}

	discovery := discoverSharedTargets(ctx, cc, filterAccountCatalog(snapshot.Catalog, cc.Flags.Account))
	items := sharedListItemsFromTargets(discovery.Targets)
	if cc.Flags.JSON {
		return printSharedJSON(cc.Output(), items, discovery.AccountsRequiringAuth, discovery.AccountsDegraded)
	}

	return printSharedText(cc.Output(), items, discovery.AccountsRequiringAuth, discovery.AccountsDegraded)
}

func sharedListItemsFromTargets(targets []sharedDiscoveryTarget) []sharedListItem {
	items := make([]sharedListItem, 0, len(targets))

	for i := range targets {
		items = append(items, sharedListItem{
			Selector:      targets[i].Selector,
			Type:          sharedItemType(targets[i].IsFolder),
			Name:          targets[i].Name,
			AccountEmail:  targets[i].AccountEmail,
			SharedByName:  targets[i].SharedByName,
			SharedByEmail: targets[i].SharedByEmail,
			ModifiedAt:    targets[i].ModifiedAt,
			Size:          targets[i].Size,
			RemoteDriveID: targets[i].RemoteDriveID,
			RemoteItemID:  targets[i].RemoteItemID,
		})
	}

	return items
}

func sharedItemType(isFolder bool) string {
	if isFolder {
		return typeFolder
	}

	return typeFile
}

func printSharedJSON(
	w io.Writer,
	items []sharedListItem,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	if items == nil {
		items = []sharedListItem{}
	}

	out := sharedListJSONOutput{
		Items:                 items,
		AccountsRequiringAuth: authRequired,
		AccountsDegraded:      degraded,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode shared output: %w", err)
	}

	return nil
}

func printSharedText(
	w io.Writer,
	items []sharedListItem,
	authRequired []accountAuthRequirement,
	degraded []accountDegradedNotice,
) error {
	if len(items) == 0 && len(authRequired) == 0 && len(degraded) == 0 {
		return writeln(w, "No shared items found.")
	}

	if err := printSharedItemsSection(w, items); err != nil {
		return err
	}

	if err := printSharedDegradedSection(w, len(items) > 0, degraded); err != nil {
		return err
	}

	if err := printSharedAuthSection(w, len(items) > 0 || len(degraded) > 0, authRequired); err != nil {
		return err
	}

	return nil
}

func filterAccountCatalog(catalog []accountCatalogEntry, account string) []accountCatalogEntry {
	if account == "" {
		return catalog
	}

	filtered := make([]accountCatalogEntry, 0, len(catalog))
	for i := range catalog {
		if catalog[i].Email != account {
			continue
		}
		filtered = append(filtered, catalog[i])
	}

	return filtered
}

func printSharedItemsSection(w io.Writer, items []sharedListItem) error {
	if len(items) == 0 {
		return nil
	}

	if err := writeln(w, "Shared items:"); err != nil {
		return err
	}

	maxType, maxName, maxOwner := len("TYPE"), len("NAME"), len("SHARED BY")
	for i := range items {
		maxType = max(maxType, len(items[i].Type))
		maxName = max(maxName, len(items[i].Name))
		owner := sharedOwnerLabel(&items[i])
		maxOwner = max(maxOwner, len(owner))
	}

	format := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%s\n", maxType, maxName, maxOwner)
	if err := writef(w, format, "TYPE", "NAME", "SHARED BY", "MODIFIED"); err != nil {
		return err
	}

	for i := range items {
		owner := sharedOwnerLabel(&items[i])
		if err := writef(w, format, items[i].Type, items[i].Name, owner, items[i].ModifiedAt); err != nil {
			return err
		}
		if err := writef(w, "    target: %s\n", items[i].Selector); err != nil {
			return err
		}
	}

	return nil
}

func printSharedDegradedSection(w io.Writer, needsSpacing bool, degraded []accountDegradedNotice) error {
	if len(degraded) == 0 {
		return nil
	}

	if err := maybeWriteBlankLine(w, needsSpacing); err != nil {
		return err
	}

	return printAccountDegradedText(w, "Accounts with degraded shared discovery:", degraded)
}

func printSharedAuthSection(w io.Writer, needsSpacing bool, authRequired []accountAuthRequirement) error {
	if len(authRequired) == 0 {
		return nil
	}

	if err := maybeWriteBlankLine(w, needsSpacing); err != nil {
		return err
	}

	return printAccountAuthRequirementsText(w, "Authentication required:", authRequired)
}

func maybeWriteBlankLine(w io.Writer, needsSpacing bool) error {
	if !needsSpacing {
		return nil
	}

	return writeln(w)
}

func sharedOwnerLabel(item *sharedListItem) string {
	if item.SharedByEmail != "" {
		return item.SharedByEmail
	}
	if item.SharedByName != "" {
		return item.SharedByName
	}

	return "unknown"
}
