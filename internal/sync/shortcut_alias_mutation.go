package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ShortcutAliasMutationKind identifies a parent-owned mutation of a OneDrive
// shortcut placeholder inside the parent engine's namespace.
type ShortcutAliasMutationKind string

const (
	ShortcutAliasMutationRename ShortcutAliasMutationKind = "rename"
	ShortcutAliasMutationDelete ShortcutAliasMutationKind = "delete"
)

// ShortcutAliasMutation is intentionally scoped to one shortcut placeholder by
// binding item ID. It is not a discovery API and cannot address content inside
// the child target.
type ShortcutAliasMutation struct {
	Kind              ShortcutAliasMutationKind
	BindingItemID     string
	RelativeLocalPath string
	LocalAlias        string
}

// ApplyShortcutAliasMutation mutates a shortcut placeholder in the parent
// drive namespace. Multisync may coordinate when parent/child runners stop, but
// the parent engine owns the actual placeholder mutation and parent shortcut
// root state.
func (e *Engine) ApplyShortcutAliasMutation(ctx context.Context, mutation ShortcutAliasMutation) error {
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
	case ShortcutAliasMutationRename:
		if mutation.LocalAlias == "" {
			return fmt.Errorf("sync: shortcut alias rename requires local alias")
		}
		if _, err := e.itemsClient.MoveItem(ctx, e.driveID, mutation.BindingItemID, "", mutation.LocalAlias); err != nil {
			return fmt.Errorf("sync: rename shortcut alias: %w", err)
		}
		return e.recordShortcutAliasRename(ctx, mutation)
	case ShortcutAliasMutationDelete:
		if err := e.itemsClient.DeleteItem(ctx, e.driveID, mutation.BindingItemID); err != nil && !errors.Is(err, graph.ErrNotFound) {
			return fmt.Errorf("sync: delete shortcut alias: %w", err)
		}
		return e.recordShortcutAliasDelete(ctx, mutation)
	default:
		return fmt.Errorf("sync: unsupported shortcut alias mutation %q", mutation.Kind)
	}
}

func (e *Engine) recordShortcutAliasRename(ctx context.Context, mutation ShortcutAliasMutation) error {
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias rename: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i].RelativeLocalPath = mutation.RelativeLocalPath
		records[i].LocalAlias = mutation.LocalAlias
		records[i].State = ShortcutRootStateActive
		records[i].BlockedDetail = ""
		records[i].ProtectedPaths = protectedPathsForShortcutRoot(mutation.RelativeLocalPath, records[i].ProtectedPaths)
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := e.baseline.ReplaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias rename: %w", err)
	}
	return nil
}

func (e *Engine) recordShortcutAliasDelete(ctx context.Context, mutation ShortcutAliasMutation) error {
	records, err := e.baseline.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read shortcut roots after alias delete: %w", err)
	}
	changed := false
	for i := range records {
		if records[i].BindingItemID != mutation.BindingItemID {
			continue
		}
		records[i].State = ShortcutRootStateRemovedFinalDrain
		records[i].BlockedDetail = ""
		records[i].ProtectedPaths = protectedPathsForShortcutRoot(records[i].RelativeLocalPath, records[i].ProtectedPaths)
		changed = true
		break
	}
	if !changed {
		return nil
	}
	if err := e.baseline.ReplaceShortcutRoots(ctx, records); err != nil {
		return fmt.Errorf("sync: persist shortcut roots after alias delete: %w", err)
	}
	return nil
}
