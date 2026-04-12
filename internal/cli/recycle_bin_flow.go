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
			Deleted: item.ModifiedAt.Format("2006-01-02T15:04:05Z"),
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

	if len(items) == 0 {
		cc.Statusf("Recycle bin is already empty\n")
		return nil
	}

	cc.Statusf("Permanently deleting %d items...\n", len(items))

	var failed int
	for i := range items {
		deleteErr := session.PermanentDeleteItem(ctx, items[i].ID)
		if deleteErr != nil && errors.Is(deleteErr, graph.ErrMethodNotAllowed) {
			deleteErr = session.DeleteItem(ctx, items[i].ID)
		}

		if deleteErr != nil {
			cc.Statusf("  Failed to delete %q: %v\n", items[i].Name, deleteErr)
			failed++
			continue
		}

		cc.Statusf("  Deleted %q\n", items[i].Name)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d items failed to delete", failed, len(items))
	}

	cc.Statusf("Recycle bin emptied\n")

	return nil
}
