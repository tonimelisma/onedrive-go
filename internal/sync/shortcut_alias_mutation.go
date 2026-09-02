package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// applyShortcutAliasMutation mutates a shortcut placeholder in the parent
// drive namespace. The parent engine owns the Graph mutation and parent
// shortcut-root state; multisync is never a caller.
func (e *Engine) applyShortcutAliasMutation(ctx context.Context, mutation shortcutAliasMutation) error {
	if ctx == nil {
		return fmt.Errorf("sync: shortcut alias mutation context is required")
	}
	if e == nil || e.itemsClient == nil {
		return fmt.Errorf("sync: shortcut alias mutation requires parent item client")
	}
	if mutation.BindingItemID == "" {
		return fmt.Errorf("sync: shortcut alias mutation requires binding item ID")
	}

	switch mutation.Kind {
	case shortcutAliasMutationRename:
		if mutation.LocalAlias == "" {
			return fmt.Errorf("sync: shortcut alias rename requires local alias")
		}
		if _, err := e.itemsClient.MoveItem(ctx, e.driveID, mutation.BindingItemID, "", mutation.LocalAlias); err != nil {
			return fmt.Errorf("sync: rename shortcut alias: %w", err)
		}
		return e.recordShortcutAliasRename(ctx, mutation)
	case shortcutAliasMutationDelete:
		if err := e.itemsClient.DeleteItem(ctx, e.driveID, mutation.BindingItemID); err != nil && !errors.Is(err, graph.ErrNotFound) {
			return fmt.Errorf("sync: delete shortcut alias: %w", err)
		}
		return e.recordShortcutAliasDelete(ctx, mutation)
	default:
		return fmt.Errorf("sync: unsupported shortcut alias mutation %q", mutation.Kind)
	}
}

func (e *Engine) recordShortcutAliasRename(ctx context.Context, mutation shortcutAliasMutation) error {
	records, err := e.baseline.listShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias rename: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i] = planShortcutAliasRenameSuccess(&records[i], mutation)
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := e.baseline.replaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias rename: %w", err)
	}
	return nil
}

func (e *Engine) recordShortcutAliasDelete(ctx context.Context, mutation shortcutAliasMutation) error {
	records, err := e.baseline.listShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias delete: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i] = planShortcutAliasDeleteSuccess(&records[i])
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := e.baseline.replaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias delete: %w", err)
	}
	return nil
}
