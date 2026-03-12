package sync

import (
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// inflightParent tracks a non-root item seen in the current delta batch,
// allowing children later in the same batch to materialize paths before
// the baseline is updated.
type inflightParent struct {
	name          string
	parentID      string
	parentDriveID driveid.ID // drive containing this item's parent
	isRoot        bool
	isVault       bool // true for Personal Vault folder (B-271)
}

// itemConverter converts []graph.Item into []ChangeEvent with full path
// materialization, NFC normalization, move detection, and deleted-item name
// recovery. Both RemoteObserver (primary drive) and shortcut observation share
// this single conversion pipeline, configured via boolean flags.
//
// Design: the inflight map is a parameter, not a field. RemoteObserver
// accumulates inflight across delta pages; shortcuts populate it once per
// batch. Same methods, different lifetime.
type itemConverter struct {
	baseline *Baseline
	driveID  driveid.ID
	logger   *slog.Logger
	stats    *observerCounters // nil-safe: primary observer provides this

	// pathPrefix is prepended to materialized paths. Empty for the primary
	// drive; set to sc.LocalPath for shortcut scopes.
	pathPrefix string

	// scopeRootID is the shortcut's RemoteItem ID. Items with this ID are
	// the scope root and should be skipped (equivalent to IsRoot for shortcuts).
	// Empty for primary drive.
	scopeRootID string

	// skipNestedShortcuts skips items with a non-empty RemoteItemID. Enabled
	// for shortcut scopes to avoid recursing into nested shortcuts.
	skipNestedShortcuts bool

	// enableVaultFilter enables Personal Vault exclusion (B-271). Only
	// applicable to the primary drive — shortcuts never contain vault folders.
	enableVaultFilter bool

	// enableShortcutDetect enables ChangeShortcut event emission for items
	// with a remoteItem facet (6.4a.2). Only the primary drive detects
	// shortcuts — within a shortcut scope, these are nested shortcuts and
	// are skipped instead.
	enableShortcutDetect bool
}

// newPrimaryConverter creates an itemConverter for primary drive observation.
// Vault filter and shortcut detection are enabled.
func newPrimaryConverter(baseline *Baseline, driveID driveid.ID, logger *slog.Logger, stats *observerCounters) *itemConverter {
	return &itemConverter{
		baseline:             baseline,
		driveID:              driveID,
		logger:               logger,
		stats:                stats,
		enableVaultFilter:    true,
		enableShortcutDetect: true,
	}
}

// newShortcutConverter creates an itemConverter for shortcut scope observation.
// Path prefix, scope root skip, and nested shortcut skip are enabled.
func newShortcutConverter(baseline *Baseline, remoteDriveID driveid.ID, logger *slog.Logger, sc *Shortcut) *itemConverter {
	return &itemConverter{
		baseline:            baseline,
		driveID:             remoteDriveID,
		logger:              logger,
		pathPrefix:          sc.LocalPath,
		scopeRootID:         sc.RemoteItem,
		skipNestedShortcuts: true,
	}
}

// ConvertItems converts a batch of graph.Items into ChangeEvents using
// two-pass processing: register all items in inflight, then classify all.
// Used by shortcut observation where all items arrive in a single batch.
func (c *itemConverter) ConvertItems(items []graph.Item) []ChangeEvent {
	inflight := make(map[driveid.ItemKey]inflightParent, len(items))

	// Pass 1: register all items so parent-chain walks see every item.
	for i := range items {
		c.registerInflight(&items[i], inflight)
	}

	// Pass 2: classify and emit events.
	var events []ChangeEvent

	for i := range items {
		if ev := c.classifyItem(&items[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}

	return events
}

// registerInflight adds an item to the inflight parent map without
// classification. Called in pass 1 to ensure the full page/batch is
// registered before any vault/classification checks (B-281).
func (c *itemConverter) registerInflight(item *graph.Item, inflight map[driveid.ItemKey]inflightParent) {
	itemDriveID := c.resolveItemDriveID(item)
	key := driveid.NewItemKey(itemDriveID, item.ID)
	inflight[key] = inflightParent{
		name:          nfcNormalize(item.Name),
		parentID:      item.ParentID,
		parentDriveID: resolveParentDriveID(item, itemDriveID),
		isRoot:        item.IsRoot,
		isVault:       item.SpecialFolderName == specialFolderVault,
	}
}

// classifyItem converts a single graph.Item into a ChangeEvent. Returns nil
// for items that should be skipped (root, vault descendants, scope root,
// nested shortcuts).
func (c *itemConverter) classifyItem(item *graph.Item, inflight map[driveid.ItemKey]inflightParent) *ChangeEvent {
	itemDriveID := c.resolveItemDriveID(item)

	// Skip root items (applies to both primary and shortcut scopes).
	if item.IsRoot {
		c.logger.Debug("skipping root item", slog.String("item_id", item.ID))

		return nil
	}

	// Skip scope root for shortcut scopes (the shortcut folder itself).
	if c.scopeRootID != "" && item.ID == c.scopeRootID {
		c.logger.Debug("skipping scope root item", slog.String("item_id", item.ID))

		return nil
	}

	// Skip nested shortcuts in shortcut scopes — items that themselves
	// reference another drive. These need their own separate observation.
	if c.skipNestedShortcuts && item.RemoteItemID != "" {
		c.logger.Warn("nested shortcut detected in observation, skipping",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
		)

		return nil
	}

	// Personal Vault exclusion (B-271): skip the vault folder itself and
	// any items whose parent chain includes a vault folder.
	if c.enableVaultFilter {
		isVault := item.SpecialFolderName == specialFolderVault
		if isVault || c.isDescendantOfVault(item, inflight, itemDriveID) {
			c.logger.Info("skipping vault item",
				slog.String("item_id", item.ID),
				slog.String("name", item.Name),
			)

			return nil
		}
	}

	// Shortcut detection (6.4a.2): items with a remoteItem facet pointing to
	// another drive AND IsFolder are shortcuts.
	if c.enableShortcutDetect && item.IsFolder && item.RemoteDriveID != "" && !item.IsDeleted {
		return c.classifyShortcut(item, inflight, itemDriveID)
	}

	return c.classifyAndConvert(item, inflight, itemDriveID)
}

// classifyAndConvert classifies the change type and builds a ChangeEvent.
// Handles NFC normalization, move detection, and deleted-item name recovery.
func (c *itemConverter) classifyAndConvert(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) *ChangeEvent {
	name := nfcNormalize(item.Name)
	baselineKey := driveid.NewItemKey(itemDriveID, item.ID)
	existing, _ := c.baseline.GetByID(baselineKey)

	hash := driveops.SelectHash(item)
	if hash != "" && c.stats != nil {
		c.stats.hashesComputed.Add(1)
	}

	ev := ChangeEvent{
		Source:    SourceRemote,
		ItemID:    item.ID,
		ParentID:  item.ParentID,
		DriveID:   itemDriveID,
		ItemType:  classifyItemType(item),
		Name:      name,
		Size:      item.Size,
		Hash:      hash,
		Mtime:     toUnixNano(item.ModifiedAt),
		ETag:      item.ETag,
		CTag:      item.CTag,
		IsDeleted: item.IsDeleted,
	}

	switch {
	case item.IsDeleted:
		ev.Type = ChangeDelete
		// Business API: deleted items may lack Name — recover from baseline.
		if ev.Name == "" && existing != nil {
			ev.Name = path.Base(existing.Path)
		}

		if existing != nil {
			ev.Path = existing.Path
		}

	case existing != nil:
		ev.Path = c.materializePath(item, inflight, itemDriveID)
		if ev.Path != existing.Path {
			ev.Type = ChangeMove
			ev.OldPath = existing.Path
		} else {
			ev.Type = ChangeModify
		}

	default:
		ev.Type = ChangeCreate
		ev.Path = c.materializePath(item, inflight, itemDriveID)
	}

	return &ev
}

// classifyShortcut builds a ChangeShortcut event for a shortcut/shared folder.
func (c *itemConverter) classifyShortcut(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) *ChangeEvent {
	relPath := c.materializePath(item, inflight, itemDriveID)

	c.logger.Info("detected shortcut",
		slog.String("item_id", item.ID),
		slog.String("name", item.Name),
		slog.String("path", relPath),
		slog.String("remote_drive", item.RemoteDriveID),
		slog.String("remote_item", item.RemoteItemID),
	)

	return &ChangeEvent{
		Source:        SourceRemote,
		Type:          ChangeShortcut,
		Path:          relPath,
		ItemID:        item.ID,
		ParentID:      item.ParentID,
		DriveID:       itemDriveID,
		ItemType:      ItemTypeFolder,
		Name:          nfcNormalize(item.Name),
		Mtime:         toUnixNano(item.ModifiedAt),
		ETag:          item.ETag,
		CTag:          item.CTag,
		RemoteDriveID: item.RemoteDriveID,
		RemoteItemID:  item.RemoteItemID,
	}
}

// materializePath builds the full relative path by walking the parent chain.
// Checks inflight first, then baseline. Applies pathPrefix after resolution.
func (c *itemConverter) materializePath(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) string {
	segments := []string{nfcNormalize(item.Name)}
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	parentID := item.ParentID

	for range maxPathDepth {
		if parentID == "" {
			break
		}

		parentKey := driveid.NewItemKey(parentDriveID, parentID)

		// Check inflight map first (items from current delta batch).
		if p, ok := inflight[parentKey]; ok {
			if p.isRoot {
				break
			}

			// Stop at scope root for shortcut scopes — the scope root
			// is the shortcut folder, not a real parent in the path.
			if c.scopeRootID != "" && parentID == c.scopeRootID {
				break
			}

			segments = append(segments, p.name)
			parentDriveID = p.parentDriveID
			parentID = p.parentID

			continue
		}

		// Baseline shortcut: prepend this item's full stored path.
		if entry, ok := c.baseline.GetByID(parentKey); ok && entry.Path != "" {
			slices.Reverse(segments)
			resolvedPath := entry.Path + "/" + strings.Join(segments, "/")

			return c.applyPrefix(resolvedPath)
		}

		// Parent not found — orphaned item.
		c.logger.Warn("orphaned item: parent not found in inflight or baseline",
			slog.String("item_id", item.ID),
			slog.String("parent_id", parentID),
			slog.String("parent_drive_id", parentDriveID.String()),
		)

		break
	}

	slices.Reverse(segments)
	relPath := strings.Join(segments, "/")

	return c.applyPrefix(relPath)
}

// applyPrefix prepends the path prefix (shortcut local path) to a relative
// path. Returns the path unchanged when no prefix is configured.
func (c *itemConverter) applyPrefix(relPath string) string {
	if c.pathPrefix == "" {
		return relPath
	}

	return path.Join(c.pathPrefix, relPath)
}

// isDescendantOfVault walks the parent chain in the inflight map to check
// whether any ancestor is a vault folder (B-271).
func (c *itemConverter) isDescendantOfVault(
	item *graph.Item, inflight map[driveid.ItemKey]inflightParent, itemDriveID driveid.ID,
) bool {
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	parentID := item.ParentID

	for range maxPathDepth {
		if parentID == "" {
			return false
		}

		parentKey := driveid.NewItemKey(parentDriveID, parentID)

		p, ok := inflight[parentKey]
		if !ok {
			return false
		}

		if p.isVault {
			return true
		}

		if p.isRoot {
			return false
		}

		parentDriveID = p.parentDriveID
		parentID = p.parentID
	}

	return false
}

// resolveItemDriveID returns the normalized driveID for an item, falling
// back to the converter's driveID when the item's DriveID is empty.
func (c *itemConverter) resolveItemDriveID(item *graph.Item) driveid.ID {
	return resolveItemDriveIDWithFallback(item, c.driveID)
}

// resolveItemDriveIDWithFallback returns the item's DriveID when non-zero,
// otherwise returns the fallback. This is the shared logic behind
// itemConverter.resolveItemDriveID and detectShortcutOrphans — both need
// the same "item DriveID or scope default" resolution.
func resolveItemDriveIDWithFallback(item *graph.Item, fallback driveid.ID) driveid.ID {
	if item.DriveID.IsZero() {
		return fallback
	}

	return item.DriveID
}

// resolveParentDriveID returns the normalized driveID for the parent of an
// item, handling cross-drive references (e.g. shared items).
func resolveParentDriveID(item *graph.Item, itemDriveID driveid.ID) driveid.ID {
	if !item.ParentDriveID.IsZero() {
		return item.ParentDriveID
	}

	return itemDriveID
}

// convertShortcutItems converts graph.Items from a shortcut scope into
// ChangeEvents using the unified item conversion pipeline. This is a thin
// wrapper creating a shortcut-configured itemConverter and calling ConvertItems.
func convertShortcutItems(
	items []graph.Item, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline, logger *slog.Logger,
) []ChangeEvent {
	if logger == nil {
		logger = slog.Default()
	}

	conv := newShortcutConverter(bl, remoteDriveID, logger, sc)

	return conv.ConvertItems(items)
}

// detectShortcutOrphans finds baseline entries belonging to a shortcut scope
// that are no longer present in the full enumeration. Delegates to
// Baseline.FindOrphans with a path prefix filter for the shortcut's local path.
func detectShortcutOrphans(
	sc *Shortcut, remoteDriveID driveid.ID, items []graph.Item, bl *Baseline,
) []ChangeEvent {
	seen := make(map[driveid.ItemKey]struct{}, len(items))
	for i := range items {
		itemDriveID := resolveItemDriveIDWithFallback(&items[i], remoteDriveID)
		seen[driveid.NewItemKey(itemDriveID, items[i].ID)] = struct{}{}
	}

	return bl.FindOrphans(seen, remoteDriveID, sc.LocalPath)
}
