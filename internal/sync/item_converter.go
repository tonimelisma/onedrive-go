package sync

import (
	"context"
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
// recovery. Drive-root and rooted-subtree observation both use this single
// conversion pipeline, configured by path-prefix and root-item fields.
//
// Design: the inflight map is a parameter, not a field. RemoteObserver
// accumulates inflight across delta pages; rooted-subtree callers populate it once per
// batch. Same methods, different lifetime.
type ItemConverter struct {
	Baseline *Baseline
	DriveID  driveid.ID
	Logger   *slog.Logger
	Stats    *ObserverCounters // nil-safe: primary observer provides this
	Items    ItemClient        // nil-safe: sparse parent enrich is best-effort

	// PathPrefix is prepended to materialized paths. Empty for the primary
	// drive; set for rooted-subtree observation that maps a remote subtree into a
	// local subpath.
	PathPrefix string

	// RootItemID is the configured remote root item for rooted-subtree observation.
	// Items with this ID are the root itself and should be skipped because the
	// sync root owns that directory already. Empty for full-drive observation.
	RootItemID string

	// EnableVaultFilter enables Personal Vault exclusion (B-271). Only
	// applicable to the primary drive.
	EnableVaultFilter bool
}

// NewPrimaryConverter creates an ItemConverter for primary-drive or
// rooted-subtree observation. Embedded shared-folder items
// are ignored; shared content syncs only when configured as its own drive.
func NewPrimaryConverter(
	baseline *Baseline,
	driveID driveid.ID,
	logger *slog.Logger,
	stats *ObserverCounters,
	items ItemClient,
) *ItemConverter {
	return &ItemConverter{
		Baseline:          baseline,
		DriveID:           driveID,
		Logger:            logger,
		Stats:             stats,
		Items:             items,
		EnableVaultFilter: true,
	}
}

// ConvertItems converts a batch of graph.Items into ChangeEvents using
// two-pass processing: register all items in inflight, then classify all.
// Used by rooted-subtree observation where all items arrive in a single batch.
func (c *ItemConverter) ConvertItems(ctx context.Context, items []graph.Item) []ChangeEvent {
	c.enrichSparseParentRefs(ctx, items)

	inflight := make(map[string]InflightParent, len(items))

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

func (c *ItemConverter) enrichSparseParentRefs(ctx context.Context, items []graph.Item) {
	if c == nil || c.Items == nil || len(items) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(items))
	for i := range items {
		item := &items[i]
		if !c.shouldEnrichSparseParent(item) {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}

		itemDriveID := c.resolveItemDriveID(item)
		enriched, err := c.Items.GetItem(ctx, itemDriveID, item.ID)
		if err != nil {
			if c.Logger != nil {
				c.Logger.Debug("sparse remote item parent enrich failed",
					slog.String("item_id", item.ID),
					slog.String("drive_id", itemDriveID.String()),
					slog.String("error", err.Error()),
				)
			}

			continue
		}
		if enriched == nil || enriched.ParentID == "" {
			continue
		}

		for j := range items {
			if items[j].ID != item.ID || items[j].ParentID != "" {
				continue
			}

			items[j].ParentID = enriched.ParentID
			if items[j].ParentDriveID.IsZero() {
				items[j].ParentDriveID = enriched.ParentDriveID
			}
		}
	}
}

func (c *ItemConverter) shouldEnrichSparseParent(item *graph.Item) bool {
	if c == nil || c.Items == nil || item == nil {
		return false
	}
	if item.IsDeleted || item.IsRoot || item.ID == "" || item.ParentID != "" {
		return false
	}

	if c.Baseline == nil {
		return true
	}

	existing, found := c.Baseline.GetByID(item.ID)
	return !found || existing.ParentID == ""
}

// registerInflight adds an item to the inflight parent map without
// classification. Called in pass 1 to ensure the full page/batch is
// registered before any vault/classification checks (B-281).
func (c *ItemConverter) registerInflight(item *graph.Item, inflight map[string]InflightParent) {
	itemDriveID := c.resolveItemDriveID(item)
	var existing *BaselineEntry
	if baselineEntry, found := c.Baseline.GetByID(item.ID); found {
		existing = baselineEntry
	}

	name := effectiveItemName(item, existing)
	parentID := item.ParentID
	parentDriveID := resolveParentDriveID(item, itemDriveID)
	if parentID == "" && existing != nil {
		parentID = existing.ParentID
		parentDriveID = itemDriveID
	}

	inflight[item.ID] = InflightParent{
		Name:          name,
		ParentID:      parentID,
		ParentDriveID: parentDriveID,
		IsRoot:        item.IsRoot,
		IsVault:       item.SpecialFolderName == specialFolderVault,
	}
}

// ClassifyItem converts a single graph.Item into a ChangeEvent. Returns nil
// for items that should be skipped (root, vault descendants, configured root,
// embedded shared-folder items).
func (c *ItemConverter) ClassifyItem(item *graph.Item, inflight map[string]InflightParent) *ChangeEvent {
	itemDriveID := c.resolveItemDriveID(item)

	// Skip root items for both full-drive and rooted-subtree observation.
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

	var existing *BaselineEntry
	if baselineEntry, found := c.Baseline.GetByID(item.ID); found {
		existing = baselineEntry
	}

	name := effectiveItemName(item, existing)
	if IsAlwaysExcluded(name) {
		c.Logger.Debug("ignoring symmetric snapshot-junk item",
			slog.String("item_id", item.ID),
			slog.String("name", name),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
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

	// Skip the configured observation root itself.
	if c.RootItemID != "" && item.ID == c.RootItemID {
		c.Logger.Debug("skipping configured rooted-subtree root item", slog.String("item_id", item.ID))

		return nil
	}

	// Embedded shared-folder items are never synced inside another drive.
	// Shared content must be configured as its own drive root instead.
	if item.RemoteDriveID != "" || item.RemoteItemID != "" {
		c.Logger.Debug("ignoring embedded shared-folder item",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
			slog.String("remote_drive", item.RemoteDriveID),
		)

		return nil
	}

	// Personal Vault exclusion (B-271): skip the vault folder itself and
	// any items whose parent chain includes a vault folder.
	if c.shouldSkipVaultItem(item, inflight) {
		return nil
	}

	return c.classifyAndConvert(item, inflight, itemDriveID, existing)
}

// classifyAndConvert classifies the change type and builds a ChangeEvent.
// Handles NFC normalization, move detection, and deleted-item name recovery.
func (c *ItemConverter) classifyAndConvert(
	item *graph.Item, inflight map[string]InflightParent, itemDriveID driveid.ID, existing *BaselineEntry,
) *ChangeEvent {
	name := effectiveItemName(item, existing)

	hash := driveops.SelectHash(item)
	if hash != "" && c.Stats != nil {
		c.Stats.hashesComputed.Add(1)
	}

	ev := ChangeEvent{
		Source:    SourceRemote,
		ItemID:    item.ID,
		ParentID:  item.ParentID,
		DriveID:   itemDriveID,
		ItemType:  ClassifyItemType(item),
		Name:      name,
		Size:      item.Size,
		Hash:      hash,
		Mtime:     ToUnixNano(item.ModifiedAt),
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

func effectiveItemName(item *graph.Item, existing *BaselineEntry) string {
	name := nfcNormalize(item.Name)
	if name != "" || existing == nil {
		return name
	}

	return path.Base(existing.Path)
}

func (c *ItemConverter) materializePathWithBaselineFallback(
	item *graph.Item,
	inflight map[string]InflightParent,
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
	item *graph.Item, inflight map[string]InflightParent, itemDriveID driveid.ID,
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
	inflight map[string]InflightParent,
) string {
	segments := []string{name}

	for range maxPathDepth {
		if parentID == "" {
			break
		}

		// Check inflight map first (items from current delta batch).
		if p, ok := inflight[parentID]; ok {
			if p.IsRoot {
				break
			}

			// Stop at the configured observation root — it is the sync root,
			// not a real parent segment in the local relative path.
			if c.RootItemID != "" && parentID == c.RootItemID {
				break
			}

			segments = append(segments, p.Name)
			parentDriveID = p.ParentDriveID
			parentID = p.ParentID

			continue
		}

		// Baseline entry found: prepend this item's full stored path.
		if entry, ok := c.Baseline.GetByID(parentID); ok && entry.Path != "" {
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

// applyPrefix prepends the configured local root prefix to a relative path.
// Returns the path unchanged when no prefix is configured.
func (c *ItemConverter) applyPrefix(relPath string) string {
	if c.PathPrefix == "" {
		return relPath
	}

	return path.Join(c.PathPrefix, relPath)
}

func (c *ItemConverter) shouldSkipVaultItem(
	item *graph.Item, inflight map[string]InflightParent,
) bool {
	if !c.EnableVaultFilter {
		return false
	}

	isVault := item.SpecialFolderName == specialFolderVault
	if !isVault && !c.isDescendantOfVault(item, inflight) {
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
	item *graph.Item, inflight map[string]InflightParent,
) bool {
	parentID := item.ParentID

	for range maxPathDepth {
		if parentID == "" {
			return false
		}

		p, ok := inflight[parentID]
		if !ok {
			return false
		}

		if p.IsVault {
			return true
		}

		if p.IsRoot {
			return false
		}

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
// otherwise returns the fallback.
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
