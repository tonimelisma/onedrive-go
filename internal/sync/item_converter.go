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
// recovery. Drive-root and mount-root observation both use this single
// conversion pipeline, shaped by path-prefix and root-item fields.
//
// Design: the inflight map is a parameter, not a field. RemoteObserver
// accumulates inflight across delta pages; mount-root callers populate it once per
// batch. Same methods, different lifetime.
type ItemConverter struct {
	Baseline              *Baseline
	DriveID               driveid.ID
	Logger                *slog.Logger
	Stats                 *ObserverCounters // nil-safe: primary observer provides this
	Items                 ItemClient        // nil-safe: sparse parent enrich is best-effort
	ShortcutTopology      *shortcutTopologyBatch
	ProtectedRootBindings map[string]ProtectedRoot

	// PathPrefix is prepended to materialized paths. Empty for the primary
	// drive; set for mount-root observation that maps a remote subtree into a
	// local subpath.
	PathPrefix string

	// RemoteRootItemID is the mounted remote root item for mount-root observation.
	// Items with this ID are the root itself and should be skipped because the
	// sync root owns that directory already. Empty for full-drive observation.
	RemoteRootItemID string

	// EnableVaultFilter enables Personal Vault exclusion (B-271). Only
	// applicable to the primary drive.
	EnableVaultFilter bool

	incompleteObservation bool
}

// NewPrimaryConverter creates an ItemConverter for primary-drive or
// mount-root observation. Embedded shared-folder items are ignored here;
// shared content syncs through explicit standalone mounts or managed child
// mounts.
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
// Used by mount-root observation where all items arrive in a single batch.
func (c *ItemConverter) ConvertItems(ctx context.Context, items []graph.Item) []ChangeEvent {
	c.resetIncompleteObservation()
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

func (c *ItemConverter) resetIncompleteObservation() {
	if c != nil {
		c.incompleteObservation = false
	}
}

func (c *ItemConverter) markIncompleteObservation() {
	if c != nil {
		c.incompleteObservation = true
	}
}

func (c *ItemConverter) observationIncomplete() bool {
	return c != nil && c.incompleteObservation
}

func (c *ItemConverter) enrichSparseParentRefs(ctx context.Context, items []graph.Item) {
	if c == nil || c.Items == nil || len(items) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(items))
	for i := range items {
		item := &items[i]
		if !c.shouldEnrichSparseRemoteItem(item) {
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
		if enriched == nil {
			continue
		}

		for j := range items {
			if items[j].ID != item.ID {
				continue
			}

			mergeSparseRemoteItem(&items[j], enriched)
		}
	}
}

func (c *ItemConverter) shouldEnrichSparseRemoteItem(item *graph.Item) bool {
	if c == nil || c.Items == nil || item == nil {
		return false
	}
	if item.IsDeleted || item.IsRoot || item.ID == "" {
		return false
	}
	if c.shouldEnrichSparseShortcut(item) {
		return true
	}

	if c.Baseline == nil {
		return item.ParentID == ""
	}

	existing, found := c.Baseline.GetByID(item.ID)
	return item.ParentID == "" && (!found || existing.ParentID == "")
}

func (c *ItemConverter) shouldEnrichSparseShortcut(item *graph.Item) bool {
	if c == nil || item == nil || c.ProtectedRootBindings == nil {
		return false
	}
	protectedRoot, known := c.ProtectedRootBindings[item.ID]
	if !known {
		return false
	}
	return item.Name == "" ||
		item.ParentID == "" ||
		item.RemoteDriveID == "" ||
		item.RemoteItemID == "" ||
		(!item.RemoteIsFolder && protectedRoot.RemoteIsFolder)
}

func mergeSparseRemoteItem(dst *graph.Item, enriched *graph.Item) {
	if dst == nil || enriched == nil {
		return
	}
	if dst.Name == "" {
		dst.Name = enriched.Name
	}
	if dst.ParentID == "" {
		dst.ParentID = enriched.ParentID
	}
	if dst.ParentDriveID.IsZero() {
		dst.ParentDriveID = enriched.ParentDriveID
	}
	if dst.DriveID.IsZero() {
		dst.DriveID = enriched.DriveID
	}
	if dst.RemoteDriveID == "" {
		dst.RemoteDriveID = enriched.RemoteDriveID
	}
	if dst.RemoteItemID == "" {
		dst.RemoteItemID = enriched.RemoteItemID
	}
	if !dst.RemoteIsFolder {
		dst.RemoteIsFolder = enriched.RemoteIsFolder
	}
	if !dst.IsFolder {
		dst.IsFolder = enriched.IsFolder
	}
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
// for items that should be skipped (root, vault descendants, mount root,
// embedded shared-folder items).
func (c *ItemConverter) ClassifyItem(item *graph.Item, inflight map[string]InflightParent) *ChangeEvent {
	itemDriveID := c.resolveItemDriveID(item)

	// Skip root items for both full-drive and mount-root observation.
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

	// Non-deleted items need a materializable leaf name. Deleted items may
	// recover their name from the baseline later in classifyAndConvert.
	// Delta query updates are allowed to omit unchanged properties, so an
	// existing baseline entry can supply the missing name safely.
	_, knownProtectedRoot := c.ProtectedRootBindings[item.ID]
	if !item.IsDeleted && item.Name == "" && existing == nil && !knownProtectedRoot {
		c.Logger.Warn("skipping remote item with empty name",
			slog.String("item_id", item.ID),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
	}

	// Skip the mount observation root itself.
	if c.RemoteRootItemID != "" && item.ID == c.RemoteRootItemID {
		c.Logger.Debug("skipping mount root item", slog.String("item_id", item.ID))

		return nil
	}

	// OneNote/package items are valid Graph objects, but they are compound
	// provider objects rather than normal file/folder content sync can manage.
	if item.IsPackage {
		c.Logger.Debug("skipping package item",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
		)

		return nil
	}

	// Embedded shared-folder items are mount boundaries, not ordinary files or
	// folders in this engine's content root.
	if c.isShortcutTopologyItem(item) {
		c.emitShortcutTopologyFact(item, inflight, itemDriveID, existing)
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

func (c *ItemConverter) isShortcutTopologyItem(item *graph.Item) bool {
	if item == nil {
		return false
	}
	if item.RemoteDriveID != "" || item.RemoteItemID != "" || item.RemoteIsFolder {
		return true
	}
	if c.ProtectedRootBindings == nil {
		return false
	}
	_, known := c.ProtectedRootBindings[item.ID]
	return known
}

func (c *ItemConverter) emitShortcutTopologyFact(
	item *graph.Item,
	inflight map[string]InflightParent,
	itemDriveID driveid.ID,
	existing *BaselineEntry,
) {
	if c == nil || c.ShortcutTopology == nil || item == nil || item.ID == "" {
		return
	}

	if item.IsDeleted {
		c.ShortcutTopology.appendDelete(shortcutBindingDelete{BindingItemID: item.ID})
		return
	}

	fact := c.shortcutTopologyFact(item, inflight, itemDriveID, existing)
	remoteDriveID := item.RemoteDriveID
	if remoteDriveID == "" {
		remoteDriveID = fact.RemoteDriveID
	}
	remoteItemID := item.RemoteItemID
	if remoteItemID == "" {
		remoteItemID = fact.RemoteItemID
	}
	remoteIsFolder := item.RemoteIsFolder || item.IsFolder || fact.RemoteIsFolder
	if remoteDriveID != "" && remoteItemID != "" && remoteIsFolder && fact.RelativeLocalPath != "" {
		c.ShortcutTopology.appendUpsert(shortcutBindingUpsert{
			BindingItemID:     item.ID,
			RelativeLocalPath: fact.RelativeLocalPath,
			LocalAlias:        fact.LocalAlias,
			RemoteDriveID:     remoteDriveID,
			RemoteItemID:      remoteItemID,
			RemoteIsFolder:    remoteIsFolder,
			Complete:          true,
		})
		return
	}

	if fact.HasEvidence {
		c.ShortcutTopology.appendUnavailable(shortcutBindingUnavailable{
			BindingItemID:     item.ID,
			RelativeLocalPath: fact.RelativeLocalPath,
			LocalAlias:        fact.LocalAlias,
			RemoteDriveID:     remoteDriveID,
			RemoteItemID:      remoteItemID,
			RemoteIsFolder:    remoteIsFolder,
			Reason:            "shortcut_binding_unavailable",
		})
	}
}

type shortcutTopologyItemFact struct {
	RelativeLocalPath string
	LocalAlias        string
	RemoteDriveID     string
	RemoteItemID      string
	RemoteIsFolder    bool
	HasEvidence       bool
}

func (c *ItemConverter) shortcutTopologyFact(
	item *graph.Item,
	inflight map[string]InflightParent,
	itemDriveID driveid.ID,
	existing *BaselineEntry,
) shortcutTopologyItemFact {
	protectedRoot, known := c.ProtectedRootBindings[item.ID]
	name := effectiveItemName(item, existing)
	relPath := c.materializePathWithBaselineFallback(item, inflight, itemDriveID, existing, name)
	if relPath == "" && known {
		relPath = protectedRoot.Path
	}
	if name == "" && relPath != "" {
		name = protectedRootPrimaryName(relPath)
	}

	return shortcutTopologyItemFact{
		RelativeLocalPath: relPath,
		LocalAlias:        name,
		RemoteDriveID:     protectedRoot.RemoteDriveID.String(),
		RemoteItemID:      protectedRoot.RemoteItemID,
		RemoteIsFolder:    protectedRoot.RemoteIsFolder,
		HasEvidence: known ||
			item.RemoteDriveID != "" ||
			item.RemoteItemID != "" ||
			item.RemoteIsFolder ||
			item.IsFolder,
	}
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
			ev.Path = c.materializePathForUntrackedDelete(item, inflight, itemDriveID)
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

	if ev.Path == "" {
		c.Logger.Warn("skipping remote item without materialized path",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
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
	if graphPath := c.materializePathFromGraphParentPath(item, name); graphPath != "" {
		return graphPath
	}

	return c.materializePathFromParts(item.ID, name, item.ParentID, resolveParentDriveID(item, itemDriveID), inflight, true)
}

// materializePath builds the full relative path by walking the parent chain.
// Checks inflight first, then baseline. Applies PathPrefix after resolution.
func (c *ItemConverter) materializePath(
	item *graph.Item, inflight map[string]InflightParent, itemDriveID driveid.ID,
) string {
	if graphPath := c.materializePathFromGraphParentPath(item, nfcNormalize(item.Name)); graphPath != "" {
		return graphPath
	}

	return c.materializePathFromParts(
		item.ID,
		nfcNormalize(item.Name),
		item.ParentID,
		resolveParentDriveID(item, itemDriveID),
		inflight,
		true,
	)
}

func (c *ItemConverter) materializePathForUntrackedDelete(
	item *graph.Item, inflight map[string]InflightParent, itemDriveID driveid.ID,
) string {
	if graphPath := c.materializePathFromGraphParentPath(item, nfcNormalize(item.Name)); graphPath != "" {
		return graphPath
	}

	return c.materializePathFromParts(
		item.ID,
		nfcNormalize(item.Name),
		item.ParentID,
		resolveParentDriveID(item, itemDriveID),
		inflight,
		false,
	)
}

func (c *ItemConverter) materializePathFromGraphParentPath(item *graph.Item, name string) string {
	if c == nil || item == nil || name == "" || c.RemoteRootItemID != "" {
		return ""
	}
	if item.ParentPath == "" && !item.ParentPathKnown {
		return ""
	}
	if item.ParentPath == "" {
		return c.applyPrefix(name)
	}

	return c.applyPrefix(path.Join(item.ParentPath, name))
}

func (c *ItemConverter) materializePathFromParts(
	itemID string,
	name string,
	parentID string,
	parentDriveID driveid.ID,
	inflight map[string]InflightParent,
	markIncomplete bool,
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

			// Stop at the mount observation root: it is the sync root,
			// not a real parent segment in the local relative path.
			if c.RemoteRootItemID != "" && parentID == c.RemoteRootItemID {
				break
			}

			segments = append(segments, p.Name)
			parentDriveID = p.ParentDriveID
			parentID = p.ParentID

			continue
		}

		// Stop at a configured mount root even when Graph did not include that
		// root row in the current batch. The mount root is the observation
		// boundary, not a materialized path segment.
		if c.RemoteRootItemID != "" && parentID == c.RemoteRootItemID {
			break
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
		if markIncomplete {
			c.markIncompleteObservation()
		}

		return ""
	}

	slices.Reverse(segments)
	relPath := strings.Join(segments, "/")

	return c.applyPrefix(relPath)
}

// applyPrefix prepends the mount local root prefix to a relative path.
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
