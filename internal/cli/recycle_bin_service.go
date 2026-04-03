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

type recycleBinService struct {
	cc      *CLIContext
	session func(context.Context) (recycleBinSession, error)
}

func newRecycleBinService(cc *CLIContext) *recycleBinService {
	return &recycleBinService{
		cc: cc,
		session: func(ctx context.Context) (recycleBinSession, error) {
			return cc.Session(ctx)
		},
	}
}

func (s *recycleBinService) runList(ctx context.Context) error {
	session, err := s.session(ctx)
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

	if s.cc.Flags.JSON {
		return formatRecycleBinJSON(s.cc.Output(), items)
	}

	return formatRecycleBinTable(s.cc.Output(), items)
}

func (s *recycleBinService) runRestore(ctx context.Context, itemID string) error {
	session, err := s.session(ctx)
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

	if s.cc.Flags.JSON {
		return printRecycleBinRestoreJSON(s.cc.Output(), recycleBinJSONItem{
			ID:      item.ID,
			Name:    item.Name,
			Size:    item.Size,
			Type:    itemType(item),
			Deleted: item.ModifiedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	s.cc.Statusf("Restored %q (id: %s)\n", item.Name, item.ID)

	return nil
}

func (s *recycleBinService) runEmpty(ctx context.Context, confirm bool) error {
	if !confirm {
		return fmt.Errorf("--confirm flag required to permanently delete all recycle bin items")
	}

	session, err := s.session(ctx)
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
		s.cc.Statusf("Recycle bin is already empty\n")
		return nil
	}

	s.cc.Statusf("Permanently deleting %d items...\n", len(items))

	var failed int
	for i := range items {
		deleteErr := session.PermanentDeleteItem(ctx, items[i].ID)
		if deleteErr != nil && errors.Is(deleteErr, graph.ErrMethodNotAllowed) {
			deleteErr = session.DeleteItem(ctx, items[i].ID)
		}

		if deleteErr != nil {
			s.cc.Statusf("  Failed to delete %q: %v\n", items[i].Name, deleteErr)
			failed++
			continue
		}

		s.cc.Statusf("  Deleted %q\n", items[i].Name)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d items failed to delete", failed, len(items))
	}

	s.cc.Statusf("Recycle bin emptied\n")

	return nil
}
