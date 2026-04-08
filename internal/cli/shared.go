package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/sharedref"
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
	return newSharedService(mustCLIContext(cmd.Context())).runList(cmd.Context())
}

type sharedService struct {
	cc *CLIContext
}

func newSharedService(cc *CLIContext) *sharedService {
	return &sharedService{cc: cc}
}

func (s *sharedService) runList(ctx context.Context) error {
	readModel := newAccountReadModelService(s.cc)
	snapshot, err := readModel.loadLenientCatalog(ctx)
	if err != nil {
		return err
	}

	authRequired := readModel.authRequirements(snapshot, func(entry accountCatalogEntry) bool {
		if s.cc.Flags.Account != "" && entry.Email != s.cc.Flags.Account {
			return false
		}
		return true
	})

	items, degraded := s.discoverSharedItems(ctx, snapshot.Catalog)
	if s.cc.Flags.JSON {
		return printSharedJSON(s.cc.Output(), items, authRequired, degraded)
	}

	return printSharedText(s.cc.Output(), items, authRequired, degraded)
}

func (s *sharedService) discoverSharedItems(
	ctx context.Context,
	catalog []accountCatalogEntry,
) ([]sharedListItem, []accountDegradedNotice) {
	logger := s.cc.Logger
	seen := make(map[string]struct{})
	var items []sharedListItem
	var degraded []accountDegradedNotice

	for i := range catalog {
		entry := &catalog[i]
		accountItems, accountDegraded := s.discoverSharedItemsForAccount(ctx, entry, seen, logger)
		items = append(items, accountItems...)
		degraded = append(degraded, accountDegraded...)
	}

	slices.SortFunc(items, func(a, b sharedListItem) int {
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type)
		}
		if a.SharedByEmail != b.SharedByEmail {
			return strings.Compare(a.SharedByEmail, b.SharedByEmail)
		}
		if a.Name != b.Name {
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		return strings.Compare(a.Selector, b.Selector)
	})

	return items, mergeDegradedNotices(degraded)
}

func (s *sharedService) discoverSharedItemsForAccount(
	ctx context.Context,
	entry *accountCatalogEntry,
	seen map[string]struct{},
	logger *slog.Logger,
) ([]sharedListItem, []accountDegradedNotice) {
	if s.cc.Flags.Account != "" && entry.Email != s.cc.Flags.Account {
		return nil, nil
	}
	if entry.AuthHealth.State == authStateAuthenticationNeeded {
		return nil, nil
	}
	if entry.RepresentativeTokenID.IsZero() {
		return nil, nil
	}

	client, _, err := s.cc.sharedBootstrapMetaClient(ctx, entry.Email)
	if err != nil {
		logger.Debug("shared discovery skipped account",
			"email", entry.Email,
			"error", err,
		)

		return nil, nil
	}

	discovered, err := searchSharedItems(ctx, client, entry.Email, logger)
	if err != nil {
		logger.Warn("degrading shared listing after search failure",
			"account", entry.Email,
			"error", err,
		)
		return nil, []accountDegradedNotice{
			sharedDiscoveryDegradedNotice(entry.Email, entry.DisplayName, entry.DriveType),
		}
	}

	for i := range discovered {
		enrichSharedItem(ctx, client, &discovered[i], logger)
	}

	var items []sharedListItem

	for i := range discovered {
		item := discovered[i]
		if item.RemoteDriveID == "" || item.RemoteItemID == "" {
			continue
		}

		key := entry.Email + "\x00" + item.RemoteDriveID + "\x00" + item.RemoteItemID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		items = append(items, sharedListItem{
			Selector: sharedref.Ref{
				AccountEmail:  entry.Email,
				RemoteDriveID: item.RemoteDriveID,
				RemoteItemID:  item.RemoteItemID,
			}.String(),
			Type:          sharedItemType(item.IsFolder),
			Name:          item.Name,
			AccountEmail:  entry.Email,
			SharedByName:  item.SharedOwnerName,
			SharedByEmail: item.SharedOwnerEmail,
			ModifiedAt:    formatAPITime(item.ModifiedAt),
			Size:          item.Size,
			RemoteDriveID: item.RemoteDriveID,
			RemoteItemID:  item.RemoteItemID,
		})
	}

	return items, nil
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
