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

// InflightParent tracks a non-root item seen in the current delta batch,
// allowing children later in the same batch to materialize paths before
// the baseline is updated.
type InflightParent struct {
	Name          string
	ParentID      string
	ParentDriveID driveid.ID // drive containing this item's parent
	IsRoot        bool
	IsVault       bool // true for Personal Vault folder (B-271)
}

// ItemConverter converts []graph.Item into []ChangeEvent with full path
// materialization, NFC normalization, move detection, and deleted-item name
// recovery. Both RemoteObserver (primary drive) and shortcut observation share
// this single conversion pipeline, configured via boolean flags.
//
// Design: the inflight map is a parameter, not a field. RemoteObserver
// accumulates inflight across delta pages; shortcuts populate it once per
// batch. Same methods, different lifetime.
type ItemConverter struct {
	Baseline *Baseline
	DriveID  driveid.ID
	Logger   *slog.Logger
	Stats    *ObserverCounters // nil-safe: primary observer provides this

	// PathPrefix is prepended to materialized paths. Empty for the primary
	// drive; set to sc.LocalPath for shortcut scopes.
	PathPrefix string

	// ScopeRootID is the shortcut's RemoteItem ID. Items with this ID are
	// the scope root and should be skipped (equivalent to IsRoot for shortcuts).
	// Empty for primary drive.
	ScopeRootID string

	// SkipNestedShortcuts skips items with a non-empty RemoteItemID. Enabled
	// for shortcut scopes to avoid recursing into nested shortcuts.
	SkipNestedShortcuts bool

	// EnableVaultFilter enables Personal Vault exclusion (B-271). Only
	// applicable to the primary drive — shortcuts never contain vault folders.
	EnableVaultFilter bool

	// EnableShortcutDetect enables ChangeShortcut event emission for items
	// with a remoteItem facet (6.4a.2). Only the primary drive detects
	// shortcuts — within a shortcut scope, these are nested shortcuts and
	// are skipped instead.
	EnableShortcutDetect bool

	// ShortcutDriveID and ShortcutItemID identify the shortcut scope's
	// source drive and folder. Set for shortcut converters, empty for
	// primary drive. Propagated to content ChangeEvents so the planner
	// can populate Action.targetShortcutKey for scope-based failure
	// handling (D-5, R-6.8.12, R-6.8.13).
	ShortcutDriveID string
	ShortcutItemID  string
}

// NewPrimaryConverter creates an ItemConverter for primary drive observation.
// Vault filter and shortcut detection are enabled.
func NewPrimaryConverter(baseline *Baseline, driveID driveid.ID, logger *slog.Logger, stats *ObserverCounters) *ItemConverter {
	return &ItemConverter{
		Baseline:             baseline,
		DriveID:              driveID,
		Logger:               logger,
		Stats:                stats,
		EnableVaultFilter:    true,
		EnableShortcutDetect: true,
	}
}

// NewShortcutConverter creates an ItemConverter for shortcut scope observation.
// Path prefix, scope root skip, and nested shortcut skip are enabled.
// A nil logger is replaced with slog.Default() to prevent panics.
func NewShortcutConverter(
	baseline *Baseline, remoteDriveID driveid.ID, logger *slog.Logger, sc *Shortcut,
) *ItemConverter {
	if logger == nil {
		logger = slog.Default()
	}

	return &ItemConverter{
		Baseline:            baseline,
		DriveID:             remoteDriveID,
		Logger:              logger,
		PathPrefix:          sc.LocalPath,
		ScopeRootID:         sc.RemoteItem,
		SkipNestedShortcuts: true,
		ShortcutDriveID:     sc.RemoteDrive,
		ShortcutItemID:      sc.RemoteItem,
	}
}

// ConvertItems converts a batch of graph.Items into ChangeEvents using
// two-pass processing: register all items in inflight, then classify all.
// Used by shortcut observation where all items arrive in a single batch.
func (c *ItemConverter) ConvertItems(items []graph.Item) []ChangeEvent {
	inflight := make(map[driveid.ItemKey]InflightParent, len(items))

	// Pass 1: register all items so parent-chain walks see every item.
	for i := range items {
		c.registerInflight(&items[i], inflight)
	}

	// Pass 2: classify and emit events.
	var events []ChangeEvent

	for i := range items {
		if ev := c.ClassifyItem(&items[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}

	return events
}

// registerInflight adds an item to the inflight parent map without
// classification. Called in pass 1 to ensure the full page/batch is
// registered before any vault/classification checks (B-281).
func (c *ItemConverter) registerInflight(item *graph.Item, inflight map[driveid.ItemKey]InflightParent) {
	itemDriveID := c.resolveItemDriveID(item)
	key := driveid.NewItemKey(itemDriveID, item.ID)
	var existing *BaselineEntry
	if baselineEntry, found := c.Baseline.GetByID(key); found {
		existing = baselineEntry
	}

	name := effectiveItemName(item, existing)
	parentID := item.ParentID
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	if parentID == "" && existing != nil {
		parentID = existing.ParentID
		parentDriveID = itemDriveID
	}

	inflight[key] = InflightParent{
		Name:          name,
		ParentID:      parentID,
		ParentDriveID: parentDriveID,
		IsRoot:        item.IsRoot,
		IsVault:       item.SpecialFolderName == specialFolderVault,
	}
}

// ClassifyItem converts a single graph.Item into a ChangeEvent. Returns nil
// for items that should be skipped (root, vault descendants, scope root,
// nested shortcuts).
func (c *ItemConverter) ClassifyItem(item *graph.Item, inflight map[driveid.ItemKey]InflightParent) *ChangeEvent {
	itemDriveID := c.resolveItemDriveID(item)

	// Skip root items (applies to both primary and shortcut scopes).
	if item.IsRoot {
		c.Logger.Debug("skipping root item", slog.String("item_id", item.ID))

		return nil
	}

	// Malformed delta items without a stable ID cannot be materialized safely.
	// Emitting them would create buffer/planner entries the rest of the
	// pipeline cannot reconcile back to durable remote state.
	if item.ID == "" {
		c.Logger.Warn("skipping remote item with empty id",
			slog.String("name", item.Name),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
	}

	baselineKey := driveid.NewItemKey(itemDriveID, item.ID)
	var existing *BaselineEntry
	if baselineEntry, found := c.Baseline.GetByID(baselineKey); found {
		existing = baselineEntry
	}

	// Non-deleted items need a materializable leaf name. Deleted items may
	// recover their name from the baseline later in classifyAndConvert.
	// Delta query updates are allowed to omit unchanged properties, so an
	// existing baseline entry can supply the missing name safely.
	if !item.IsDeleted && item.Name == "" && existing == nil {
		c.Logger.Warn("skipping remote item with empty name",
			slog.String("item_id", item.ID),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
	}

	// Skip scope root for shortcut scopes (the shortcut folder itself).
	if c.ScopeRootID != "" && item.ID == c.ScopeRootID {
		c.Logger.Debug("skipping scope root item", slog.String("item_id", item.ID))

		return nil
	}

	// Skip nested shortcuts in shortcut scopes — items that themselves
	// reference another drive. These need their own separate observation.
	if c.SkipNestedShortcuts && item.RemoteItemID != "" {
		c.Logger.Warn("nested shortcut detected in observation, skipping",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
		)

		return nil
	}

	// Personal Vault exclusion (B-271): skip the vault folder itself and
	// any items whose parent chain includes a vault folder.
	if c.shouldSkipVaultItem(item, inflight, itemDriveID) {
		return nil
	}

	// Shortcut detection (6.4a.2): items with a remoteItem facet pointing to
	// another drive are shortcuts. The folder facet may be absent on shared
	// folder shortcuts returned by the delta endpoint — RemoteDriveID alone
	// is sufficient to identify a shortcut.
	if c.EnableShortcutDetect && item.RemoteDriveID != "" && !item.IsDeleted {
		return c.classifyShortcut(item, inflight, itemDriveID, existing)
	}

	return c.classifyAndConvert(item, inflight, itemDriveID, existing)
}

// classifyAndConvert classifies the change type and builds a ChangeEvent.
// Handles NFC normalization, move detection, and deleted-item name recovery.
func (c *ItemConverter) classifyAndConvert(
	item *graph.Item, inflight map[driveid.ItemKey]InflightParent, itemDriveID driveid.ID, existing *BaselineEntry,
) *ChangeEvent {
	name := effectiveItemName(item, existing)

	hash := driveops.SelectHash(item)
	if hash != "" && c.Stats != nil {
		c.Stats.hashesComputed.Add(1)
	}

	ev := ChangeEvent{
		Source:        SourceRemote,
		ItemID:        item.ID,
		ParentID:      item.ParentID,
		DriveID:       itemDriveID,
		ItemType:      ClassifyItemType(item),
		Name:          name,
		Size:          item.Size,
		Hash:          hash,
		Mtime:         ToUnixNano(item.ModifiedAt),
		ETag:          item.ETag,
		CTag:          item.CTag,
		IsDeleted:     item.IsDeleted,
		RemoteDriveID: c.ShortcutDriveID,
		RemoteItemID:  c.ShortcutItemID,
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
		if ev.Path == "" {
			// When the item is absent from the baseline but the delta payload still
			// carries enough name/parent-chain context, materialize the delete path
			// from the current item instead of emitting an empty-path event.
			ev.Path = c.materializePath(item, inflight, itemDriveID)
		}
		if ev.Path == "" {
			c.Logger.Warn("skipping remote delete without recoverable path",
				slog.String("item_id", item.ID),
				slog.String("name", item.Name),
				slog.String("drive_id", itemDriveID.String()),
			)

			return nil
		}

	case existing != nil:
		ev.Path = c.materializePathWithBaselineFallback(item, inflight, itemDriveID, existing, name)
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
func (c *ItemConverter) classifyShortcut(
	item *graph.Item,
	inflight map[driveid.ItemKey]InflightParent,
	itemDriveID driveid.ID,
	existing *BaselineEntry,
) *ChangeEvent {
	name := effectiveItemName(item, existing)
	relPath := c.materializePathWithBaselineFallback(item, inflight, itemDriveID, existing, name)

	c.Logger.Info("detected shortcut",
		slog.String("item_id", item.ID),
		slog.String("name", name),
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
		Name:          name,
		Mtime:         ToUnixNano(item.ModifiedAt),
		ETag:          item.ETag,
		CTag:          item.CTag,
		RemoteDriveID: item.RemoteDriveID,
		RemoteItemID:  item.RemoteItemID,
	}
}

func effectiveItemName(item *graph.Item, existing *BaselineEntry) string {
	name := nfcNormalize(item.Name)
	if name != "" || existing == nil {
		return name
	}

	return path.Base(existing.Path)
}

func (c *ItemConverter) materializePathWithBaselineFallback(
	item *graph.Item,
	inflight map[driveid.ItemKey]InflightParent,
	itemDriveID driveid.ID,
	existing *BaselineEntry,
	name string,
) string {
	if name == "" {
		return ""
	}

	if item.ParentID == "" && existing != nil && existing.Path != "" {
		parentDir := path.Dir(existing.Path)
		if parentDir == "." {
			return name
		}

		return path.Join(parentDir, name)
	}

	return c.materializePathFromParts(item.ID, name, item.ParentID, resolveParentDriveID(item, itemDriveID), inflight)
}

// materializePath builds the full relative path by walking the parent chain.
// Checks inflight first, then baseline. Applies PathPrefix after resolution.
func (c *ItemConverter) materializePath(
	item *graph.Item, inflight map[driveid.ItemKey]InflightParent, itemDriveID driveid.ID,
) string {
	return c.materializePathFromParts(
		item.ID,
		nfcNormalize(item.Name),
		item.ParentID,
		resolveParentDriveID(item, itemDriveID),
		inflight,
	)
}

func (c *ItemConverter) materializePathFromParts(
	itemID string,
	name string,
	parentID string,
	parentDriveID driveid.ID,
	inflight map[driveid.ItemKey]InflightParent,
) string {
	segments := []string{name}

	for range maxPathDepth {
		if parentID == "" {
			break
		}

		parentKey := driveid.NewItemKey(parentDriveID, parentID)

		// Check inflight map first (items from current delta batch).
		if p, ok := inflight[parentKey]; ok {
			if p.IsRoot {
				break
			}

			// Stop at scope root for shortcut scopes — the scope root
			// is the shortcut folder, not a real parent in the path.
			if c.ScopeRootID != "" && parentID == c.ScopeRootID {
				break
			}

			segments = append(segments, p.Name)
			parentDriveID = p.ParentDriveID
			parentID = p.ParentID

			continue
		}

		// Baseline shortcut: prepend this item's full stored path.
		if entry, ok := c.Baseline.GetByID(parentKey); ok && entry.Path != "" {
			slices.Reverse(segments)
			resolvedPath := entry.Path + "/" + strings.Join(segments, "/")

			return c.applyPrefix(resolvedPath)
		}

		// Parent not found — orphaned item.
		c.Logger.Warn("orphaned item: parent not found in inflight or baseline",
			slog.String("item_id", itemID),
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
func (c *ItemConverter) applyPrefix(relPath string) string {
	if c.PathPrefix == "" {
		return relPath
	}

	return path.Join(c.PathPrefix, relPath)
}

func (c *ItemConverter) shouldSkipVaultItem(
	item *graph.Item, inflight map[driveid.ItemKey]InflightParent, itemDriveID driveid.ID,
) bool {
	if !c.EnableVaultFilter {
		return false
	}

	isVault := item.SpecialFolderName == specialFolderVault
	if !isVault && !c.isDescendantOfVault(item, inflight, itemDriveID) {
		return false
	}

	c.Logger.Info("skipping vault item",
		slog.String("item_id", item.ID),
		slog.String("name", item.Name),
	)

	return true
}

// isDescendantOfVault walks the parent chain in the inflight map to check
// whether any ancestor is a vault folder (B-271).
func (c *ItemConverter) isDescendantOfVault(
	item *graph.Item, inflight map[driveid.ItemKey]InflightParent, itemDriveID driveid.ID,
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

		if p.IsVault {
			return true
		}

		if p.IsRoot {
			return false
		}

		parentDriveID = p.ParentDriveID
		parentID = p.ParentID
	}

	return false
}

// resolveItemDriveID returns the normalized driveID for an item, falling
// back to the converter's driveID when the item's DriveID is empty.
func (c *ItemConverter) resolveItemDriveID(item *graph.Item) driveid.ID {
	return resolveItemDriveIDWithFallback(item, c.DriveID)
}

// resolveItemDriveIDWithFallback returns the item's DriveID when non-zero,
// otherwise returns the fallback. This is the shared logic behind
// ItemConverter.resolveItemDriveID and detectShortcutOrphans — both need
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

// ConvertShortcutItems converts graph.Items from a shortcut scope into
// ChangeEvents using the unified item conversion pipeline. This is a thin
// wrapper creating a shortcut-configured ItemConverter and calling ConvertItems.
// Exported for use by the sync engine's shortcut observation logic.
func ConvertShortcutItems(
	items []graph.Item, sc *Shortcut, remoteDriveID driveid.ID, bl *Baseline, logger *slog.Logger,
) []ChangeEvent {
	conv := NewShortcutConverter(bl, remoteDriveID, logger, sc)

	return conv.ConvertItems(items)
}

// DetectShortcutOrphans finds baseline entries belonging to a shortcut scope
// that are no longer present in the full enumeration. Delegates to
// Baseline.FindOrphans with a path prefix filter for the shortcut's local path.
// Exported for use by the sync engine's shortcut observation logic.
func DetectShortcutOrphans(
	sc *Shortcut, remoteDriveID driveid.ID, items []graph.Item, bl *Baseline,
) []ChangeEvent {
	seen := make(map[driveid.ItemKey]struct{}, len(items))
	for i := range items {
		itemDriveID := resolveItemDriveIDWithFallback(&items[i], remoteDriveID)
		seen[driveid.NewItemKey(itemDriveID, items[i].ID)] = struct{}{}
	}

	return findBaselineOrphans(bl, seen, remoteDriveID, sc.LocalPath)
}
