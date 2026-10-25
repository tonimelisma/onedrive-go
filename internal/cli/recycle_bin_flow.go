package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type recycleBinSession interface {
	ListRecycleBinItems(ctx context.Context) ([]graph.Item, error)
	RestoreItem(ctx context.Context, itemID string) (*graph.Item, error)
	PermanentDeleteItem(ctx context.Context, itemID string) error
	DeleteItem(ctx context.Context, itemID string) error
}

type recycleBinSessionFactory func(context.Context) (recycleBinSession, error)

func defaultRecycleBinSessionFactory(cc *CLIContext) recycleBinSessionFactory {
	return func(ctx context.Context) (recycleBinSession, error) {
		return cc.Session(ctx)
	}
}

func runRecycleBinListWithFactory(
	ctx context.Context,
	cc *CLIContext,
	sessionFactory recycleBinSessionFactory,
) error {
	session, err := sessionFactory(ctx)
	if err != nil {
		return err
	}

	items, err := session.ListRecycleBinItems(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrBadRequest) {
			return fmt.Errorf("recycle bin listing is not available on Personal OneDrive accounts")
		}

		return fmt.Errorf("listing recycle bin: %w", err)
	}

	if cc.Flags.JSON {
		return formatRecycleBinJSON(cc.Output(), items)
	}

	return formatRecycleBinTable(cc.Output(), items)
}

func runRecycleBinRestoreWithFactory(
	ctx context.Context,
	cc *CLIContext,
	itemID string,
	sessionFactory recycleBinSessionFactory,
) error {
	session, err := sessionFactory(ctx)
	if err != nil {
		return err
	}

	item, err := session.RestoreItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, graph.ErrConflict) {
			return fmt.Errorf("cannot restore %q: a file with the same name already exists at the original location", itemID)
		}

		return fmt.Errorf("restoring item: %w", err)
	}

	if cc.Flags.JSON {
		return printRecycleBinRestoreJSON(cc.Output(), recycleBinJSONItem{
			ID:      item.ID,
			Name:    item.Name,
			Size:    item.Size,
			Type:    itemType(item),
			Deleted: formatAPITime(item.ModifiedAt),
		})
	}

	cc.Statusf("Restored %q (id: %s)\n", item.Name, item.ID)

	return nil
}

func runRecycleBinEmptyWithFactory(
	ctx context.Context,
	cc *CLIContext,
	confirm bool,
	sessionFactory recycleBinSessionFactory,
) error {
	if !confirm {
		return fmt.Errorf("--confirm flag required to permanently delete all recycle bin items")
	}

	session, err := sessionFactory(ctx)
	if err != nil {
		return err
	}

	items, err := session.ListRecycleBinItems(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrBadRequest) {
			return fmt.Errorf("recycle bin listing is not available on Personal OneDrive accounts")
		}

		return fmt.Errorf("listing recycle bin: %w", err)
	}

	report := recycleBinEmptyJSONOutput{Deleted: []recycleBinEmptyJSONItem{}, Failed: []recycleBinEmptyJSONItem{}}

	if len(items) == 0 {
		if cc.Flags.JSON {
			return writeJSONDocument(cc.Output(), report)
		}

		cc.Statusf("Recycle bin is already empty\n")
		return nil
	}

	if !cc.Flags.JSON {
		cc.Statusf("Permanently deleting %d items...\n", len(items))
	}

	deleteRecycleBinItems(ctx, cc, session, items, &report)

	if cc.Flags.JSON {
		if err := writeJSONDocument(cc.Output(), report); err != nil {
			return err
		}
	}

	if failed := len(report.Failed); failed > 0 {
		return fmt.Errorf("%d of %d items failed to delete", failed, len(items))
	}

	if !cc.Flags.JSON {
		cc.Statusf("Recycle bin emptied\n")
	}

	return nil
}

// deleteRecycleBinItems permanently deletes each item (falling back to a
// plain delete where permanent delete is not allowed), recording the outcome
// in report. A failure is a warning in both modes; --json also records it.
func deleteRecycleBinItems(
	ctx context.Context,
	cc *CLIContext,
	session recycleBinSession,
	items []graph.Item,
	report *recycleBinEmptyJSONOutput,
) {
	for i := range items {
		deleteErr := session.PermanentDeleteItem(ctx, items[i].ID)
		if deleteErr != nil && errors.Is(deleteErr, graph.ErrMethodNotAllowed) {
			deleteErr = session.DeleteItem(ctx, items[i].ID)
		}

		if deleteErr != nil {
			cc.Statusf("  Failed to delete %q: %v\n", items[i].Name, deleteErr)
			report.Failed = append(report.Failed, recycleBinEmptyJSONItem{
				ID: items[i].ID, Name: items[i].Name, Error: deleteErr.Error(),
			})
			continue
		}

		report.Deleted = append(report.Deleted, recycleBinEmptyJSONItem{ID: items[i].ID, Name: items[i].Name})
		if !cc.Flags.JSON {
			cc.Statusf("  Deleted %q\n", items[i].Name)
		}
	}
}

// recycleBinEmptyJSONOutput is `recycle-bin empty --json`: the items deleted
// and the ones that could not be. The document is printed even when some
// deletions failed; the exit status reports the failure.
type recycleBinEmptyJSONOutput struct {
	Deleted []recycleBinEmptyJSONItem `json:"deleted"`
	Failed  []recycleBinEmptyJSONItem `json:"failed"`
}

type recycleBinEmptyJSONItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}
