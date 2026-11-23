package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// applyShortcutAliasMutation mutates a shortcut placeholder in the parent
// drive namespace. The parent engine owns the Graph mutation and parent
// shortcut-root state; multisync is never a caller.
func (s *shortcutRoots) applyShortcutAliasMutation(ctx context.Context, mutation syncshortcut.ShortcutAliasMutation) error {
	if ctx == nil {
		return errors.New("sync: shortcut alias mutation context is required")
	}
	if s == nil || s.items == nil {
		return errors.New("sync: shortcut alias mutation requires parent item client")
	}
	if mutation.BindingItemID == "" {
		return errors.New("sync: shortcut alias mutation requires binding item ID")
	}

	switch mutation.Kind {
	case syncshortcut.ShortcutAliasMutationRename:
		if mutation.LocalAlias == "" {
			return errors.New("sync: shortcut alias rename requires local alias")
		}
		// No time to keep: a shortcut's own modification time is never
		// compared or synced.
		if _, err := s.items.MoveItem(ctx, s.driveID, mutation.BindingItemID, "", mutation.LocalAlias, time.Time{}); err != nil {
			return fmt.Errorf("sync: rename shortcut alias: %w", err)
		}
		return s.recordShortcutAliasRename(ctx, mutation)
	case syncshortcut.ShortcutAliasMutationDelete:
		if err := s.items.DeleteItem(ctx, s.driveID, mutation.BindingItemID); err != nil && !errors.Is(err, graph.ErrNotFound) {
			return fmt.Errorf("sync: delete shortcut alias: %w", err)
		}
		return s.recordShortcutAliasDelete(ctx, mutation)
	default:
		return fmt.Errorf("sync: unsupported shortcut alias mutation %q", mutation.Kind)
	}
}

func (s *shortcutRoots) recordShortcutAliasRename(ctx context.Context, mutation syncshortcut.ShortcutAliasMutation) error {
	records, err := s.store.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias rename: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i] = syncshortcut.PlanShortcutAliasRenameSuccess(&records[i], mutation)
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := s.store.ReplaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias rename: %w", err)
	}
	return nil
}

func (s *shortcutRoots) recordShortcutAliasDelete(ctx context.Context, mutation syncshortcut.ShortcutAliasMutation) error {
	records, err := s.store.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias delete: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i] = syncshortcut.PlanShortcutAliasDeleteSuccess(&records[i])
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := s.store.ReplaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias delete: %w", err)
	}
	return nil
}
