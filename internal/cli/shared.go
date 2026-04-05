package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	items := s.discoverSharedItems(ctx, snapshot.Catalog)
	if s.cc.Flags.JSON {
		return printSharedJSON(s.cc.Output(), items, authRequired)
	}

	return printSharedText(s.cc.Output(), items, authRequired)
}

func (s *sharedService) discoverSharedItems(ctx context.Context, catalog []accountCatalogEntry) []sharedListItem {
	logger := s.cc.Logger
	seen := make(map[string]struct{})
	var items []sharedListItem

	for i := range catalog {
		entry := catalog[i]
		if s.cc.Flags.Account != "" && entry.Email != s.cc.Flags.Account {
			continue
		}
		if entry.AuthHealth.State == authStateAuthenticationNeeded {
			continue
		}
		if entry.RepresentativeTokenID.IsZero() {
			continue
		}

		client, err := s.cc.sharedBootstrapMetaClient(ctx, entry.Email)
		if err != nil {
			logger.Debug("shared discovery skipped account",
				"email", entry.Email,
				"error", err,
			)
			continue
		}

		discovered := searchSharedItemsWithFallback(ctx, client, entry.Email, logger)
		for j := range discovered {
			item := discovered[j]
			if item.RemoteDriveID == "" || item.RemoteItemID == "" {
				continue
			}

			enrichSharedItem(ctx, client, &item, logger)

			key := entry.Email + "\x00" + item.RemoteDriveID + "\x00" + item.RemoteItemID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			itemType := typeFile
			if item.IsFolder {
				itemType = typeFolder
			}

			items = append(items, sharedListItem{
				Selector: sharedref.Ref{
					AccountEmail:  entry.Email,
					RemoteDriveID: item.RemoteDriveID,
					RemoteItemID:  item.RemoteItemID,
				}.String(),
				Type:          itemType,
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

	return items
}

func printSharedJSON(w io.Writer, items []sharedListItem, authRequired []accountAuthRequirement) error {
	out := sharedListJSONOutput{
		Items:                 items,
		AccountsRequiringAuth: authRequired,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode shared output: %w", err)
	}

	return nil
}

func printSharedText(w io.Writer, items []sharedListItem, authRequired []accountAuthRequirement) error {
	if len(items) == 0 && len(authRequired) == 0 {
		return writeln(w, "No shared items found.")
	}

	if len(items) > 0 {
		if err := writeln(w, "Shared items:"); err != nil {
			return err
		}

		maxType, maxName, maxOwner := len("TYPE"), len("NAME"), len("SHARED BY")
		for i := range items {
			maxType = max(maxType, len(items[i].Type))
			maxName = max(maxName, len(items[i].Name))
			owner := items[i].SharedByEmail
			if owner == "" {
				owner = items[i].AccountEmail
			}
			maxOwner = max(maxOwner, len(owner))
		}

		format := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%s\n", maxType, maxName, maxOwner)
		if err := writef(w, format, "TYPE", "NAME", "SHARED BY", "MODIFIED"); err != nil {
			return err
		}

		for i := range items {
			owner := items[i].SharedByEmail
			if owner == "" {
				owner = items[i].AccountEmail
			}
			if err := writef(w, format, items[i].Type, items[i].Name, owner, items[i].ModifiedAt); err != nil {
				return err
			}
			if err := writef(w, "    target: %s\n", items[i].Selector); err != nil {
				return err
			}
		}
	}

	if len(authRequired) > 0 {
		if len(items) > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		return printAccountAuthRequirementsText(w, "Authentication required:", authRequired)
	}

	return nil
}
