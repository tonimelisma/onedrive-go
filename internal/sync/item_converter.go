package sync

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// inflightParent tracks a non-root item seen in the current delta batch,
// allowing children later in the same batch to materialize paths before
// the baseline is updated.
type inflightParent struct {
	Name          string
	ParentID      string
	ParentDriveID driveid.ID // drive containing this item's parent
	IsRoot        bool
	IsVault       bool // true for Personal Vault folder (B-271)
	IsPackage     bool // true for packages (OneNote) and bundles (albums), which sync never manages

	// HasUnmaterializableName marks a folder whose Graph name cannot be one
	// local path segment (see unmaterializableRemoteNameReason). Its children
	// are skipped rather than placed under a folded or escaped path.
	HasUnmaterializableName bool
}

// itemConverter converts []graph.Item into []ChangeEvent with full path
// materialization, NFC normalization, move detection, and deleted-item name
// recovery. Drive-root and mount-root observation both use this single
// conversion pipeline, shaped by path-prefix and root-item fields.
//
// Design: the inflight map is a parameter, not a field. RemoteObserver
// accumulates inflight across delta pages; mount-root callers populate it once per
// batch. Same methods, different lifetime.
type itemConverter struct {
	Baseline              *Baseline
	DriveID               driveid.ID
	Logger                *slog.Logger
	Stats                 *observerCounters // nil-safe: primary observer provides this
	Items                 itemClient        // nil-safe: sparse parent enrich is best-effort
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

	// incompleteObservation, unreachableParents and ancestorFetches are
	// per-window state, reset by resetIncompleteObservation at the start of
	// every delta window or mount-root batch. The converter is owned by one
	// observer goroutine, so they need no lock.
	incompleteObservation bool

	// unreachableParents holds parent IDs that Graph answered as not found or
	// forbidden while resolving a window's missing ancestors. Children of such
	// a parent can never be placed, so they are skipped instead of holding
	// the delta cursor on a window that no replay can complete.
	unreachableParents map[string]struct{}

	// ancestorFetches counts GetItem calls spent resolving missing ancestors
	// in the current window, bounded by maxAncestorFetchesPerWindow.
	ancestorFetches int

	// sparseEnrichFailed holds, by item ID, the error of each GetItem
	// enrichment that failed in this window: for an item that arrived
	// without a parent reference, and for a new item that arrived without a
	// type facet (R-2.4.16). Such an item cannot be placed or classified
	// for now, and the window is replayed; one whose enrichment succeeded
	// and still lacks the fact is skipped for good, as is a facetless item
	// Graph answers as not found, since a replay cannot change either.
	sparseEnrichFailed map[string]error

	// carriedAncestors are the ancestors resolveAncestorChain read by ID in
	// windows that did not complete. A replay of the same window reuses them
	// without spending its fetch budget, so a window that needs more reads
	// than one budget allows still completes after a few replays instead of
	// failing the same way forever (R-2.15.4). They are path context only and
	// are forgotten once a window completes (forgetCarriedAncestors), so a
	// later window never places an item under a stale ancestor.
	carriedAncestors map[string]graph.Item
}

// maxCarriedAncestors bounds carriedAncestors.
const maxCarriedAncestors = 50 * maxAncestorFetchesPerWindow

// maxAncestorFetchesPerWindow bounds the item-by-ID reads one delta window may
// spend resolving parents that are neither in the window nor in the baseline.
// Orphans are rare, so the bound only stops a pathological window from
// turning into thousands of Graph calls; items it leaves unresolved keep the
// window incomplete and are retried on the next poll.
const maxAncestorFetchesPerWindow = 200

// newPrimaryConverter creates an ItemConverter for primary-drive or
// mount-root observation. Embedded shared-folder items are ignored here;
// shared content syncs through explicit standalone mounts or managed child
// mounts.
func newPrimaryConverter(
	baseline *Baseline,
	driveID driveid.ID,
	logger *slog.Logger,
	stats *observerCounters,
	items itemClient,
) *itemConverter {
	return &itemConverter{
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
func (c *itemConverter) ConvertItems(ctx context.Context, items []graph.Item) []changeEvent {
	c.resetIncompleteObservation()
	c.enrichSparseParentRefs(ctx, items)

	inflight := make(map[string]inflightParent, len(items))

	// Pass 1: register all items so parent-chain walks see every item.
	for i := range items {
		c.registerInflight(&items[i], inflight)
	}
	c.resolveMissingAncestors(ctx, items, inflight)

	// Pass 2: classify and emit events.
	var events []changeEvent

	for i := range items {
		if ev := c.ClassifyItem(&items[i], inflight); ev != nil {
			events = append(events, *ev)
		}
	}
	if !c.observationIncomplete() {
		c.forgetCarriedAncestors()
	}

	return events
}

func (c *itemConverter) resetIncompleteObservation() {
	if c != nil {
		c.incompleteObservation = false
		c.unreachableParents = nil
		c.ancestorFetches = 0
		c.sparseEnrichFailed = nil
	}
}

func (c *itemConverter) markIncompleteObservation() {
	if c != nil {
		c.incompleteObservation = true
	}
}

func (c *itemConverter) observationIncomplete() bool {
	return c != nil && c.incompleteObservation
}

func (c *itemConverter) enrichSparseParentRefs(ctx context.Context, items []graph.Item) {
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
					slog.Any("error", err),
				)
			}
			if c.sparseEnrichFailed == nil {
				c.sparseEnrichFailed = make(map[string]error)
			}
			c.sparseEnrichFailed[item.ID] = err

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

func (c *itemConverter) shouldEnrichSparseRemoteItem(item *graph.Item) bool {
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
		return item.ParentID == "" || item.TypeFacetMissing
	}

	existing, found := c.Baseline.GetByID(item.ID)
	if item.TypeFacetMissing && !found {
		// One re-read for an item whose kind delta did not say and the
		// baseline cannot supply (R-2.4.16).
		return true
	}
	return item.ParentID == "" && (!found || existing.ParentID == "")
}

func (c *itemConverter) shouldEnrichSparseShortcut(item *graph.Item) bool {
	if c == nil || item == nil || c.ProtectedRootBindings == nil {
		return false
	}
	binding, known := c.ProtectedRootBindings[item.ID]
	if !known {
		return false
	}
	return item.Name == "" ||
		item.ParentID == "" ||
		item.RemoteDriveID == "" ||
		item.RemoteItemID == "" ||
		(!item.RemoteIsFolder && binding.RemoteIsFolder)
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
	if dst.TypeFacetMissing && !enriched.TypeFacetMissing {
		// The re-read says what the item is: take its kind and content facts.
		dst.TypeFacetMissing = false
		dst.IsFolder = enriched.IsFolder
		dst.IsPackage = enriched.IsPackage
		dst.IsBundle = enriched.IsBundle
		dst.QuickXorHash = enriched.QuickXorHash
		dst.SHA1Hash = enriched.SHA1Hash
		dst.SHA256Hash = enriched.SHA256Hash
		dst.Size = enriched.Size
	}
}

// resolveMissingAncestors reads, by ID, the ancestors of window items whose
// parent chain leaves both the window (inflight) and the baseline, and
// registers them in inflight so path materialization can place the item.
//
// An incremental delta window carries only changed items. A change inside a
// folder that this client never put in its baseline -- a OneNote package or
// Personal Vault folder that observation suppresses, or a folder whose own
// download has not succeeded yet -- arrives without its parent. Without this
// step that item marks the window incomplete, the cursor never advances, and
// every later poll replays the same window: remote sync stalls for good.
//
// Fetched ancestors are path context only; they are never emitted as events.
// A parent Graph answers as not found or forbidden is recorded in
// unreachableParents so its children are skipped rather than stalling the
// cursor. Any other read failure leaves the item unresolved, which keeps the
// window incomplete and retries it on the next poll.
func (c *itemConverter) resolveMissingAncestors(
	ctx context.Context,
	items []graph.Item,
	inflight map[string]inflightParent,
) {
	if c == nil || c.Items == nil || len(items) == 0 {
		return
	}

	for i := range items {
		item := &items[i]
		if item.IsRoot || item.ID == "" {
			continue
		}
		// A delete of a synced item takes its path from the baseline. One the
		// baseline never held may still have a remote_state row (a file never
		// downloaded, a held remote twin), and without a placed path the
		// delete was dropped, leaving that row, and a twin issue with it, in
		// place until the next full rescan.
		if item.IsDeleted {
			if _, synced := c.Baseline.GetByID(item.ID); synced {
				continue
			}
		}

		c.resolveAncestorChain(ctx, item.ParentID, resolveParentDriveID(item, c.resolveItemDriveID(item)), inflight)
	}
}

func (c *itemConverter) resolveAncestorChain(
	ctx context.Context,
	parentID string,
	parentDriveID driveid.ID,
	inflight map[string]inflightParent,
) {
	for range maxPathDepth {
		if parentID == "" || (c.RemoteRootItemID != "" && parentID == c.RemoteRootItemID) {
			return
		}
		if p, ok := inflight[parentID]; ok {
			if p.IsRoot {
				return
			}
			parentID, parentDriveID = p.ParentID, p.ParentDriveID

			continue
		}
		if entry, ok := c.Baseline.GetByID(parentID); ok && entry.Path != "" {
			return
		}
		if _, unreachable := c.unreachableParents[parentID]; unreachable {
			return
		}
		ancestor, ok := c.readAncestor(ctx, parentID, parentDriveID)
		if !ok {
			return
		}
		c.registerInflight(ancestor, inflight)
	}
}

// readAncestor returns a missing ancestor carried from an earlier attempt at
// this window, or reads it by ID while the window's budget lasts.
func (c *itemConverter) readAncestor(ctx context.Context, parentID string, parentDriveID driveid.ID) (*graph.Item, bool) {
	if carried, ok := c.carriedAncestors[parentID]; ok {
		return &carried, true
	}
	if c.ancestorFetches >= maxAncestorFetchesPerWindow || ctx.Err() != nil {
		return nil, false
	}

	c.ancestorFetches++
	ancestor, err := c.Items.GetItem(ctx, parentDriveID, parentID)
	if err != nil || ancestor == nil || ancestor.ID == "" {
		c.recordAncestorFetchFailure(parentID, parentDriveID, err)

		return nil, false
	}
	c.carryAncestor(ancestor)

	return ancestor, true
}

func (c *itemConverter) carryAncestor(ancestor *graph.Item) {
	if c.carriedAncestors == nil {
		c.carriedAncestors = make(map[string]graph.Item)
	}
	if len(c.carriedAncestors) >= maxCarriedAncestors {
		return
	}
	c.carriedAncestors[ancestor.ID] = *ancestor
}

// forgetCarriedAncestors drops the ancestors carried for replays; a window
// completed, so nothing replays it.
func (c *itemConverter) forgetCarriedAncestors() {
	if c != nil {
		c.carriedAncestors = nil
	}
}

func (c *itemConverter) recordAncestorFetchFailure(parentID string, parentDriveID driveid.ID, err error) {
	if errors.Is(err, graph.ErrNotFound) || errors.Is(err, graph.ErrForbidden) {
		if c.unreachableParents == nil {
			c.unreachableParents = make(map[string]struct{})
		}
		c.unreachableParents[parentID] = struct{}{}
	}

	if c.Logger == nil {
		return
	}

	errText := "empty item"
	if err != nil {
		errText = err.Error()
	}
	c.Logger.Debug("missing remote ancestor read failed",
		slog.String("parent_id", parentID),
		slog.String("parent_drive_id", parentDriveID.String()),
		slog.String("error", errText),
	)
}

// registerInflight adds an item to the inflight parent map without
// classification. Called in pass 1 to ensure the full page/batch is
// registered before any vault/classification checks (B-281).
func (c *itemConverter) registerInflight(item *graph.Item, inflight map[string]inflightParent) {
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

	inflight[item.ID] = inflightParent{
		Name:          name,
		ParentID:      parentID,
		ParentDriveID: parentDriveID,
		IsRoot:        item.IsRoot,
		IsVault:       item.SpecialFolderName == specialFolderVault,
		IsPackage:     item.IsPackage || item.IsBundle,

		HasUnmaterializableName: name != "" && unmaterializableRemoteNameReason(name) != "",
	}
}

// ClassifyItem converts a single graph.Item into a ChangeEvent. Returns nil
// for items that should be skipped (root, vault descendants, mount root,
// embedded shared-folder items).
func (c *itemConverter) ClassifyItem(item *graph.Item, inflight map[string]inflightParent) *changeEvent {
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
		c.Logger.Debug("skipping remote item with empty id",
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
		c.Logger.Debug("skipping remote item with empty name",
			slog.String("item_id", item.ID),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
	}

	if c.hasUnmaterializableName(item, existing) {
		return nil
	}

	// Skip the mount observation root itself.
	if c.RemoteRootItemID != "" && item.ID == c.RemoteRootItemID {
		c.Logger.Debug("skipping mount root item", slog.String("item_id", item.ID))

		return nil
	}

	if c.isCompoundProviderItem(item, inflight) {
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

// isCompoundProviderItem reports OneNote/package items, bundles (albums) and
// their descendants: valid Graph objects, but compound provider objects
// rather than normal file/folder content sync can manage (R-2.4.16).
func (c *itemConverter) isCompoundProviderItem(item *graph.Item, inflight map[string]inflightParent) bool {
	return item.IsPackage || item.IsBundle || c.isDescendantOfPackage(item, inflight)
}

// hasUnmaterializableName reports, with a warning, an item whose Graph name
// is opaque but cannot be exactly one local path segment: path.Join would
// fold ".." into the parent and turn "a/b" into a nested path. A delete of a
// synced item keeps its baseline path and needs no name.
func (c *itemConverter) hasUnmaterializableName(item *graph.Item, existing *BaselineEntry) bool {
	if item.Name == "" || (item.IsDeleted && existing != nil) {
		return false
	}

	reason := unmaterializableRemoteNameReason(item.Name)
	if reason == "" {
		return false
	}

	c.Logger.Warn("Not syncing a OneDrive item whose name cannot be a local file name",
		slog.String("item_id", item.ID),
		slog.String("name", item.Name),
		slog.String("reason", reason),
	)

	return true
}

func (c *itemConverter) isShortcutTopologyItem(item *graph.Item) bool {
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

func (c *itemConverter) emitShortcutTopologyFact(
	item *graph.Item,
	inflight map[string]inflightParent,
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

func (c *itemConverter) shortcutTopologyFact(
	item *graph.Item,
	inflight map[string]inflightParent,
	itemDriveID driveid.ID,
	existing *BaselineEntry,
) shortcutTopologyItemFact {
	binding, known := c.ProtectedRootBindings[item.ID]
	name := effectiveItemName(item, existing)
	relPath := c.materializePathWithBaselineFallback(item, inflight, itemDriveID, existing, name)
	if relPath == "" && known {
		relPath = binding.Path
	}
	if name == "" && relPath != "" {
		name = protectedRootPrimaryName(relPath)
	}

	return shortcutTopologyItemFact{
		RelativeLocalPath: relPath,
		LocalAlias:        name,
		RemoteDriveID:     binding.RemoteDriveID.String(),
		RemoteItemID:      binding.RemoteItemID,
		RemoteIsFolder:    binding.RemoteIsFolder,
		HasEvidence: known ||
			item.RemoteDriveID != "" ||
			item.RemoteItemID != "" ||
			item.RemoteIsFolder ||
			item.IsFolder,
	}
}

// classifyAndConvert classifies the change type and builds a ChangeEvent.
// Handles NFC normalization, move detection, and deleted-item name recovery.
func (c *itemConverter) classifyAndConvert(
	item *graph.Item, inflight map[string]inflightParent, itemDriveID driveid.ID, existing *BaselineEntry,
) *changeEvent {
	name := effectiveItemName(item, existing)

	hash := driveops.SelectHash(item)
	if hash != "" && c.Stats != nil {
		c.Stats.hashesComputed.Add(1)
	}

	ev := changeEvent{
		Source:    sourceRemote,
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
	if !c.normalizeRemoteFacts(&ev, item, existing) {
		return nil
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
			// A delete of an item this client never synced: there is nothing
			// local to remove, so this is expected and needs no user action.
			c.Logger.Debug("skipping remote delete without recoverable path",
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
		ev.Type = changeCreate
		ev.Path = c.materializePath(item, inflight, itemDriveID)
	}

	if ev.Path == "" {
		// Debug: the reason was already reported where materialization failed
		// (an unreachable parent warns once; an unresolved one fails the
		// window and is retried).
		c.Logger.Debug("skipping remote item without materialized path",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
			slog.String("parent_id", item.ParentID),
			slog.String("drive_id", itemDriveID.String()),
		)

		return nil
	}

	return &ev
}

// normalizeRemoteFacts repairs facts Graph reports badly (R-2.4.16): a
// negative size is unknown, and a missing type facet takes the baseline's
// kind. It reports false when the item cannot be classified at all.
func (c *itemConverter) normalizeRemoteFacts(ev *changeEvent, item *graph.Item, existing *BaselineEntry) bool {
	if item.Size < 0 {
		ev.Size = 0
		ev.SizeUnknown = true
	}
	if item.IsDeleted || !item.TypeFacetMissing {
		return true
	}

	return c.applyBaselineKind(ev, item, existing)
}

// applyBaselineKind settles the kind of an item delta reported without any
// type facet (R-2.4.16). Read as a file by default, a sparse update of a
// synced folder planned a folder-to-file type change that deleted the local
// folder. The baseline's kind is kept, and for a file so are its content
// facts, because an update without a file facet carries no hashes: it is not
// evidence the content changed. Without a baseline the one re-read
// enrichSparseParentRefs makes decides: a re-read that failed for any reason
// but "not found" marks the observation incomplete, so the window is replayed
// instead of the item being skipped for good behind a persisted cursor (the
// same rule as a parentless item, materializePathPreferringIDChain); a
// re-read that answered and still names no kind, or found no item, skips it.
func (c *itemConverter) applyBaselineKind(ev *changeEvent, item *graph.Item, existing *BaselineEntry) bool {
	if existing == nil {
		if enrichErr, failed := c.sparseEnrichFailed[item.ID]; failed && !errors.Is(enrichErr, graph.ErrNotFound) {
			c.Logger.Debug("remote item kind unknown and its re-read failed; the change window will be replayed",
				slog.String("item_id", item.ID),
				slog.String("name", item.Name),
				slog.Any("error", enrichErr),
			)
			c.markIncompleteObservation()
			return false
		}
		c.Logger.Debug("skipping remote item that does not say whether it is a file or a folder",
			slog.String("item_id", item.ID),
			slog.String("name", item.Name),
		)
		return false
	}

	ev.ItemType = existing.ItemType
	if existing.ItemType == ItemTypeFile && ev.Hash == "" {
		ev.Hash = existing.RemoteHash
		ev.Size = existing.RemoteSize
		ev.SizeUnknown = !existing.RemoteSizeKnown
	}

	return true
}

func effectiveItemName(item *graph.Item, existing *BaselineEntry) string {
	name := nfcNormalize(item.Name)
	if name != "" || existing == nil {
		return name
	}

	return path.Base(existing.Path)
}

func (c *itemConverter) materializePathWithBaselineFallback(
	item *graph.Item,
	inflight map[string]inflightParent,
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
	return c.materializePathPreferringIDChain(item, name, itemDriveID, inflight, true)
}

// materializePath builds the full relative path by walking the parent chain.
// Checks inflight first, then baseline. Applies PathPrefix after resolution.
func (c *itemConverter) materializePath(
	item *graph.Item, inflight map[string]inflightParent, itemDriveID driveid.ID,
) string {
	return c.materializePathPreferringIDChain(item, nfcNormalize(item.Name), itemDriveID, inflight, true)
}

func (c *itemConverter) materializePathForUntrackedDelete(
	item *graph.Item, inflight map[string]inflightParent, itemDriveID driveid.ID,
) string {
	name := nfcNormalize(item.Name)
	if name == "" {
		return ""
	}
	return c.materializePathPreferringIDChain(item, name, itemDriveID, inflight, false)
}

// materializePathPreferringIDChain places an item by its parent-ID chain and
// uses parentReference.path only when that chain cannot be resolved. IDs are
// the authority (R-6.7.3); the path is a second rendering of the same names,
// and every segment of it is checked like a name before it is used.
//
// A live item with no parent reference at all is not placed at the drive
// root, which is where an omitted parentReference would otherwise put it. It
// is unplaceable, and its window is replayed only when the sparse-item read
// that should have supplied the parent failed (a transient condition).
// Untracked deletes (markIncomplete false) keep the leaf fallback: Business
// routinely omits parentReference on deletes, and such an event only retires
// state for an ID this client never synced.
func (c *itemConverter) materializePathPreferringIDChain(
	item *graph.Item,
	name string,
	itemDriveID driveid.ID,
	inflight map[string]inflightParent,
	markIncomplete bool,
) string {
	if item.ParentID == "" && markIncomplete {
		if graphPath := c.materializePathFromGraphParentPath(item, name); graphPath != "" {
			return graphPath
		}
		c.Logger.Debug("skipping remote item without a parent reference",
			slog.String("item_id", item.ID),
			slog.String("name", name),
		)
		if _, failed := c.sparseEnrichFailed[item.ID]; failed {
			c.markIncompleteObservation()
		}

		return ""
	}

	parentDriveID := resolveParentDriveID(item, itemDriveID)
	if idPath := c.materializePathFromParts(item.ID, name, item.ParentID, parentDriveID, inflight, false); idPath != "" {
		return idPath
	}
	if graphPath := c.materializePathFromGraphParentPath(item, name); graphPath != "" {
		return graphPath
	}

	return c.materializePathFromParts(item.ID, name, item.ParentID, parentDriveID, inflight, markIncomplete)
}

func (c *itemConverter) materializePathFromGraphParentPath(item *graph.Item, name string) string {
	if c == nil || item == nil || name == "" || c.RemoteRootItemID != "" {
		return ""
	}
	if item.ParentPath == "" && !item.ParentPathKnown {
		return ""
	}
	if item.ParentPath == "" {
		return c.applyPrefix(name)
	}
	// The path is verbatim (L-32), so each segment is a raw name: one that
	// cannot be a local path segment ("..", "") must not reach path.Join.
	for _, segment := range strings.Split(item.ParentPath, "/") {
		if segment == "" || unmaterializableRemoteNameReason(segment) != "" {
			return ""
		}
	}

	return c.applyPrefix(path.Join(item.ParentPath, name))
}

func (c *itemConverter) materializePathFromParts(
	itemID string,
	name string,
	parentID string,
	parentDriveID driveid.ID,
	inflight map[string]inflightParent,
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

			if p.HasUnmaterializableName {
				// The parent itself was skipped and warned about.
				return ""
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

		// Parent not found -- orphaned item.
		if _, unreachable := c.unreachableParents[parentID]; unreachable {
			// Graph itself says the parent is gone or not readable, so no
			// replay can ever place this item. Skip it and let the window
			// advance; one line here, because the window is not replayed.
			if markIncomplete {
				c.Logger.Warn("Not syncing a OneDrive item: its parent folder is not accessible",
					slog.String("name", name),
				)
				c.Logger.Debug("orphaned item skipped: parent unreachable",
					slog.String("item_id", itemID),
					slog.String("parent_id", parentID),
					slog.String("parent_drive_id", parentDriveID.String()),
				)
			}

			return ""
		}

		c.Logger.Debug("orphaned item: parent not found in inflight or baseline",
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
func (c *itemConverter) applyPrefix(relPath string) string {
	if c.PathPrefix == "" {
		return relPath
	}

	return path.Join(c.PathPrefix, relPath)
}

func (c *itemConverter) shouldSkipVaultItem(
	item *graph.Item, inflight map[string]inflightParent,
) bool {
	if !c.EnableVaultFilter {
		return false
	}

	isVault := item.SpecialFolderName == specialFolderVault
	if !isVault && !c.isDescendantOfVault(item, inflight) {
		return false
	}

	c.Logger.Debug("skipping vault item",
		slog.String("item_id", item.ID),
		slog.String("name", item.Name),
	)

	return true
}

// isDescendantOfVault walks the parent chain in the inflight map to check
// whether any ancestor is a vault folder (B-271).
func (c *itemConverter) isDescendantOfVault(
	item *graph.Item, inflight map[string]inflightParent,
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

// isDescendantOfPackage reports whether an item sits inside a package (a
// OneNote notebook, for example). Packages are skipped, so their contents
// must be too: resolveMissingAncestors can now place such a child by
// fetching the package as its parent, and without this check the notebook's
// sections would sync as ordinary files.
func (c *itemConverter) isDescendantOfPackage(
	item *graph.Item, inflight map[string]inflightParent,
) bool {
	parentID := item.ParentID

	for range maxPathDepth {
		p, ok := inflight[parentID]
		if parentID == "" || !ok || p.IsRoot {
			return false
		}

		if p.IsPackage {
			return true
		}

		parentID = p.ParentID
	}

	return false
}

// resolveItemDriveID returns the normalized driveID for an item, falling
// back to the converter's driveID when the item's DriveID is empty.
func (c *itemConverter) resolveItemDriveID(item *graph.Item) driveid.ID {
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

// parentDirSegment is the path segment that names a parent directory.
const parentDirSegment = ".."

// unmaterializableRemoteNameReason reports why a Graph item name cannot be
// used as one local path segment, or "" when it can. OneDrive itself rejects
// all of these, so they only arrive from a malformed or hostile response; the
// check exists so such a response cannot place content outside its folder.
func unmaterializableRemoteNameReason(name string) string {
	switch {
	case name == "." || name == parentDirSegment:
		return "dot segment"
	case strings.ContainsRune(name, '/'):
		return "contains a slash"
	case strings.ContainsRune(name, 0):
		return "contains NUL"
	case !utf8.ValidString(name):
		return "invalid UTF-8"
	default:
		return ""
	}
}
