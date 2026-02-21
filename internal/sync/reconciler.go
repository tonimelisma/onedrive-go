package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Reconciler compares three-state items (local current, remote current, synced base)
// and produces an ordered ActionPlan. It is the heart of the sync algorithm:
// after the delta processor and local scanner have updated item state in the DB,
// the reconciler reads every active item and classifies what action is needed.
//
// Change detection uses per-side hash baselines to handle SharePoint enrichment
// correctly (sharepoint-enrichment.md §4). See detectLocalChange and
// detectRemoteChange for the implementation.
type Reconciler struct {
	store  ReconcilerStore
	logger *slog.Logger
}

// NewReconciler creates a Reconciler that reads item state from the given store
// and logs decisions at debug level.
func NewReconciler(store ReconcilerStore, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}

	return &Reconciler{
		store:  store,
		logger: logger,
	}
}

// Reconcile iterates all active items in the state database, classifies each
// using the three-way merge decision matrix (sync-algorithm.md §5.2), and
// returns an ordered ActionPlan.
func (r *Reconciler) Reconcile(ctx context.Context, mode SyncMode) (*ActionPlan, error) {
	r.logger.Info("reconciliation started", "mode", mode)

	items, err := r.store.ListAllActiveItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("list active items: %w", err)
	}

	plan := &ActionPlan{}

	// Detect local moves before classifying individual items.
	// Local moves are hash-based: a locally-deleted item whose SyncedHash matches
	// a new local item's LocalHash is treated as a move rather than delete+create.
	localMoves := r.detectLocalMoves(items)

	for i := range items {
		item := items[i]

		// Skip items that were classified as part of a local move.
		if localMoves.isSource(item) || localMoves.isTarget(item) {
			continue
		}

		actions := r.reconcileItem(item, mode)
		appendActions(plan, actions)
	}

	// Add local move actions.
	plan.Moves = append(plan.Moves, localMoves.moves...)

	// Order the plan per sync-algorithm.md §5.3.
	orderPlan(plan)

	r.logger.Info("reconciliation complete",
		"total_actions", plan.TotalActions(),
		"downloads", len(plan.Downloads),
		"uploads", len(plan.Uploads),
		"local_deletes", len(plan.LocalDeletes),
		"remote_deletes", len(plan.RemoteDeletes),
		"conflicts", len(plan.Conflicts),
		"moves", len(plan.Moves),
		"folder_creates", len(plan.FolderCreates),
		"synced_updates", len(plan.SyncedUpdates),
		"cleanups", len(plan.Cleanups),
	)

	return plan, nil
}

// reconcileItem classifies a single item and returns the resulting actions.
func (r *Reconciler) reconcileItem(item *Item, mode SyncMode) []Action {
	if item.ItemType == ItemTypeFolder || item.ItemType == ItemTypeRoot {
		return r.reconcileFolder(item, mode)
	}

	return r.reconcileFile(item, mode)
}

// --- File reconciliation ---

// reconcileFile implements the 14-row file decision matrix from sync-algorithm.md §5.2.
func (r *Reconciler) reconcileFile(item *Item, mode SyncMode) []Action {
	localChanged := detectLocalChange(item)
	remoteChanged := detectRemoteChange(item)

	// Mode-specific filtering (sync-algorithm.md §5.1).
	if mode == SyncDownloadOnly {
		localChanged = false
	}

	if mode == SyncUploadOnly {
		remoteChanged = false
	}

	synced := isSynced(item)

	r.logger.Debug("classify file",
		"path", item.Path,
		"localChanged", localChanged,
		"remoteChanged", remoteChanged,
		"synced", synced,
		"localHash", item.LocalHash,
		"remoteHash", item.QuickXorHash,
		"syncedHash", item.SyncedHash,
	)

	return r.applyFileMatrix(item, localChanged, remoteChanged, synced)
}

// applyFileMatrix maps the (localChanged, remoteChanged, synced) triple to actions.
func (r *Reconciler) applyFileMatrix(
	item *Item, localChanged, remoteChanged, synced bool,
) []Action {
	localPresent := item.LocalHash != ""
	remotePresent := item.QuickXorHash != "" && !item.IsDeleted

	// F14: Both absent, was synced → cleanup.
	if synced && !localPresent && !remotePresent {
		r.logger.Debug("F14: both absent, cleanup", "path", item.Path)
		return []Action{r.cleanupAction(item)}
	}

	// Handle local-deletion rows (F6, F7).
	if a := r.classifyLocalDeletion(item, remoteChanged, synced); a != nil {
		return a
	}

	// Handle remote-tombstone rows (F8, F9).
	// classifyRemoteTombstone returns (actions, handled). When handled is true,
	// the tombstone case applies and we return the actions.
	if a, handled := r.classifyRemoteTombstone(item, localChanged, synced); handled {
		return a
	}

	// F1-F5 and F10-F13: standard change matrix.
	return r.classifyStandardChange(item, localChanged, remoteChanged, synced)
}

// classifyLocalDeletion handles F6 (remote delete) and F7 (re-download) when the
// local file was deleted by the user.
func (r *Reconciler) classifyLocalDeletion(item *Item, remoteChanged, synced bool) []Action {
	localDeleted := synced && item.LocalHash == ""
	if !localDeleted {
		return nil
	}

	if !remoteChanged {
		// F6: Local deleted, remote unchanged → propagate deletion.
		r.logger.Debug("F6: local deleted, remote unchanged → remote delete", "path", item.Path)
		return []Action{r.remoteDeleteAction(item)}
	}

	// F7: Local deleted, remote changed → remote wins, re-download.
	r.logger.Debug("F7: local deleted, remote changed → download", "path", item.Path)
	return []Action{r.downloadAction(item)}
}

// classifyRemoteTombstone handles F8 (local delete) and F9 (edit-delete conflict) when
// the remote item was tombstoned. The safety checker (S2) handles delta-completeness
// filtering — the reconciler always emits the action and lets the safety layer decide.
// Returns (actions, true) when the tombstone case applies, or (nil, false) when it does not.
func (r *Reconciler) classifyRemoteTombstone(
	item *Item, localChanged, synced bool,
) ([]Action, bool) {
	if !item.IsDeleted || !synced {
		return nil, false
	}

	if !localChanged {
		// F8: Remote tombstoned, local unchanged → delete locally.
		r.logger.Debug("F8: remote tombstoned, local unchanged → local delete", "path", item.Path)
		return []Action{r.localDeleteAction(item)}, true
	}

	// F9: Remote tombstoned, local changed → edit-delete conflict.
	r.logger.Debug("F9: edit-delete conflict", "path", item.Path)
	return []Action{r.conflictAction(item, ConflictEditDelete)}, true
}

// classifyStandardChange handles F1-F5 (synced) and F10-F13 (unsynced) for items
// that are not in a deletion/tombstone state.
func (r *Reconciler) classifyStandardChange(item *Item, localChanged, remoteChanged, synced bool) []Action {
	switch {
	case !localChanged && !remoteChanged:
		// F1 or F12/F13 both absent+unsynced: no action.
		return nil

	case !localChanged && remoteChanged:
		// F2: Only remote changed → download. F13: new remote.
		r.logger.Debug("F2/F13: remote changed → download", "path", item.Path)
		return []Action{r.downloadAction(item)}

	case localChanged && !remoteChanged:
		// F3: Only local changed → upload. F12: new local.
		r.logger.Debug("F3/F12: local changed → upload", "path", item.Path)
		return []Action{r.uploadAction(item)}

	default:
		// Both changed: F4/F5 or F10/F11.
		return r.classifyBothChanged(item, synced)
	}
}

// classifyBothChanged handles F4/F5 (synced) and F10/F11 (unsynced) when both sides changed.
func (r *Reconciler) classifyBothChanged(item *Item, synced bool) []Action {
	hashMatch := item.LocalHash == item.QuickXorHash

	if synced {
		if hashMatch {
			// F4: False conflict — both converged to same content.
			r.logger.Debug("F4: false conflict → update synced", "path", item.Path)
			return []Action{r.updateSyncedAction(item)}
		}
		// F5: True conflict.
		r.logger.Debug("F5: conflict", "path", item.Path)

		return []Action{r.conflictAction(item, ConflictEditEdit)}
	}

	// Unsynced (new).
	if hashMatch {
		// F10: Both created identical file.
		r.logger.Debug("F10: identical new → update synced", "path", item.Path)
		return []Action{r.updateSyncedAction(item)}
	}

	// F11: Create-create conflict.
	r.logger.Debug("F11: create-create conflict", "path", item.Path)

	return []Action{r.conflictAction(item, ConflictCreateCreate)}
}

// --- Folder reconciliation ---

// reconcileFolder implements the 7-row folder decision matrix (sync-algorithm.md §5.2).
// The state classification is extracted into folderState to keep cyclomatic complexity
// within linter limits.
func (r *Reconciler) reconcileFolder(item *Item, mode SyncMode) []Action {
	fs := newFolderState(item)

	// D1: Both exist, was synced → no action.
	if fs.localExists && fs.remoteExists && fs.synced {
		return nil
	}

	return r.dispatchFolder(item, fs, mode)
}

// folderState holds precomputed booleans for folder reconciliation.
type folderState struct {
	localExists  bool
	remoteExists bool
	synced       bool
	tombstoned   bool
}

func newFolderState(item *Item) folderState {
	return folderState{
		localExists:  item.LocalHash != "" || item.LocalMtime != nil,
		remoteExists: !item.IsDeleted && item.ItemID != "",
		synced:       isSynced(item),
		tombstoned:   item.IsDeleted,
	}
}

// dispatchFolder handles D2-D6 folder rows (D1 is filtered before this call).
func (r *Reconciler) dispatchFolder(
	item *Item, fs folderState, mode SyncMode,
) []Action {
	switch {
	// D2: Both exist, not synced → adopt.
	case fs.localExists && fs.remoteExists:
		return r.folderAdopt(item, mode)

	// D3: Missing locally, exists remotely → create locally.
	case !fs.localExists && fs.remoteExists:
		return r.folderCreateLocal(item, mode)

	// D4: Exists locally, tombstoned remotely → delete locally.
	case fs.localExists && fs.tombstoned && fs.synced:
		return r.folderDeleteLocal(item, mode)

	// D5: Exists locally, missing remotely, not synced → create remotely.
	case fs.localExists && !fs.remoteExists && !fs.synced:
		return r.folderCreateRemote(item, mode)

	// D6: Missing locally, missing remotely, was synced → cleanup.
	case !fs.localExists && !fs.remoteExists && fs.synced:
		r.logger.Debug("D6: folder cleanup", "path", item.Path)
		return []Action{r.cleanupAction(item)}

	default:
		return nil
	}
}

// folderAdopt handles D2: both sides exist, not yet synced → adopt in DB.
func (r *Reconciler) folderAdopt(item *Item, mode SyncMode) []Action {
	if mode == SyncUploadOnly {
		return nil
	}
	r.logger.Debug("D2: adopt folder", "path", item.Path)
	return []Action{r.updateSyncedAction(item)}
}

// folderCreateLocal handles D3: missing locally, exists remotely → mkdir.
func (r *Reconciler) folderCreateLocal(item *Item, mode SyncMode) []Action {
	if mode == SyncUploadOnly {
		return nil
	}
	r.logger.Debug("D3: create folder locally", "path", item.Path)
	return []Action{r.folderCreateAction(item, true)}
}

// folderDeleteLocal handles D4: exists locally, tombstoned remotely → rmdir.
// Delta-completeness filtering is handled by the safety checker (S2), not here.
func (r *Reconciler) folderDeleteLocal(item *Item, mode SyncMode) []Action {
	if mode == SyncUploadOnly {
		return nil
	}

	r.logger.Debug("D4: delete folder locally", "path", item.Path)

	return []Action{r.localDeleteAction(item)}
}

// folderCreateRemote handles D5: exists locally, missing remotely → API mkdir.
func (r *Reconciler) folderCreateRemote(item *Item, mode SyncMode) []Action {
	if mode == SyncDownloadOnly {
		return nil
	}
	r.logger.Debug("D5: create folder remotely", "path", item.Path)
	return []Action{r.folderCreateAction(item, false)}
}

// --- Change detection (per-side baselines, sharepoint-enrichment.md §4) ---

// detectLocalChange reports whether the item's local state differs from the synced base.
//
// The scanner updates LocalHash only when it detects an actual content change on disk
// (via the mtime fast-path). So LocalHash in the DB is stable across cycles unless the
// file was truly modified. Change detection compares against SyncedHash with an enrichment
// guard: if the local file's mtime has not advanced past LastSyncedAt, any hash difference
// is due to SharePoint enrichment (LocalHash=AAA, SyncedHash=BBB after enriched upload)
// and should not be treated as a change.
func detectLocalChange(item *Item) bool {
	// Never synced → "changed" if we have local data (new local file).
	if item.SyncedHash == "" {
		return item.LocalHash != ""
	}

	// Locally deleted: scanner cleared LocalHash for missing files.
	if item.LocalHash == "" {
		return true
	}

	// Hash match → definitely no change.
	if item.LocalHash == item.SyncedHash {
		return false
	}

	// Hash differs. Could be a real edit or enrichment divergence.
	// If local mtime has not advanced past last sync time, the file was not
	// touched on disk → the difference is from enrichment.
	if item.LocalMtime != nil && item.LastSyncedAt != nil {
		if TruncateToSeconds(*item.LocalMtime) <= TruncateToSeconds(*item.LastSyncedAt) {
			return false // Enrichment-stable: file unchanged since sync.
		}
	}

	return true
}

// detectRemoteChange reports whether the item's remote state differs from the synced base.
//
// The delta processor updates QuickXorHash when the server reports a new hash.
// After an enriched upload, QuickXorHash == SyncedHash (both are the server's enriched
// hash), so no false positive occurs.
func detectRemoteChange(item *Item) bool {
	// Never synced → "changed" if we have remote data (new remote file).
	if item.SyncedHash == "" {
		return item.QuickXorHash != ""
	}

	// Tombstoned → remotely deleted.
	if item.IsDeleted {
		return true
	}

	// Compare remote hash to synced base.
	return item.QuickXorHash != item.SyncedHash
}

// isSynced returns true if the item has been successfully synced at least once.
func isSynced(item *Item) bool {
	return item.SyncedHash != "" || item.LastSyncedAt != nil
}

// --- Local move detection (hash-based, sync-algorithm.md §5.4) ---

// localMoveSet tracks matched local move pairs to prevent double-processing.
type localMoveSet struct {
	moves     []Action
	sourceIDs map[string]bool // driveID/itemID of deleted (source) items
	targetIDs map[string]bool // driveID/itemID of new (target) items
}

// isSource returns true if the item is the source (deleted) side of a local move.
func (lm *localMoveSet) isSource(item *Item) bool {
	return lm.sourceIDs[item.DriveID+"/"+item.ItemID]
}

// isTarget returns true if the item is the target (new) side of a local move.
func (lm *localMoveSet) isTarget(item *Item) bool {
	return lm.targetIDs[item.DriveID+"/"+item.ItemID]
}

// detectLocalMoves finds hash-based move candidates among active items.
// A locally-deleted item (LocalHash cleared, SyncedHash set) whose SyncedHash
// matches a new local item's LocalHash (SyncedHash empty) is a move candidate,
// but only when the match is unique (one deleted, one new).
func (r *Reconciler) detectLocalMoves(items []*Item) *localMoveSet {
	result := &localMoveSet{
		sourceIDs: make(map[string]bool),
		targetIDs: make(map[string]bool),
	}

	// Build maps of locally-deleted and new-local items by hash.
	type candidate struct {
		item *Item
	}

	deletedByHash := make(map[string][]candidate) // SyncedHash → deleted items
	newByHash := make(map[string][]candidate)     // LocalHash → new items

	for i := range items {
		item := items[i]

		if item.ItemType != ItemTypeFile {
			continue
		}

		// Locally deleted: was synced (SyncedHash set) and scanner cleared LocalHash.
		if item.SyncedHash != "" && item.LocalHash == "" && !item.IsDeleted {
			deletedByHash[item.SyncedHash] = append(deletedByHash[item.SyncedHash], candidate{item: item})
		}

		// New local: has LocalHash but never synced (no SyncedHash).
		if item.LocalHash != "" && item.SyncedHash == "" {
			newByHash[item.LocalHash] = append(newByHash[item.LocalHash], candidate{item: item})
		}
	}

	// Match: unique deleted hash == unique new hash → move.
	for hash, deleted := range deletedByHash {
		newItems, ok := newByHash[hash]
		if !ok || len(deleted) != 1 || len(newItems) != 1 {
			continue // Ambiguous or no match.
		}

		src := deleted[0].item
		dst := newItems[0].item

		r.logger.Debug("local move detected",
			"from", src.Path,
			"to", dst.Path,
			"hash", hash,
		)

		result.moves = append(result.moves, Action{
			Type:    ActionRemoteMove,
			DriveID: src.DriveID,
			ItemID:  src.ItemID,
			Path:    src.Path,
			NewPath: dst.Path,
			Item:    src,
		})

		key := func(item *Item) string { return item.DriveID + "/" + item.ItemID }
		result.sourceIDs[key(src)] = true
		result.targetIDs[key(dst)] = true
	}

	return result
}

// --- Action constructors ---

func (r *Reconciler) downloadAction(item *Item) Action {
	return Action{
		Type:    ActionDownload,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

func (r *Reconciler) uploadAction(item *Item) Action {
	return Action{
		Type:    ActionUpload,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

func (r *Reconciler) localDeleteAction(item *Item) Action {
	return Action{
		Type:    ActionLocalDelete,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

func (r *Reconciler) remoteDeleteAction(item *Item) Action {
	return Action{
		Type:    ActionRemoteDelete,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

// conflictAction creates an ActionConflict with the correct ConflictType tag.
// The type is determined by the reconciler's decision matrix row (F5/F9/F11),
// so the conflict handler in the executor can apply the right resolution strategy.
func (r *Reconciler) conflictAction(item *Item, conflictType ConflictType) Action {
	return Action{
		Type:    ActionConflict,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
		ConflictInfo: &ConflictRecord{
			DriveID:    item.DriveID,
			ItemID:     item.ItemID,
			Path:       item.Path,
			LocalHash:  item.LocalHash,
			RemoteHash: item.QuickXorHash,
			LocalMtime: item.LocalMtime,
			Type:       conflictType,
		},
	}
}

func (r *Reconciler) updateSyncedAction(item *Item) Action {
	return Action{
		Type:    ActionUpdateSynced,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

func (r *Reconciler) cleanupAction(item *Item) Action {
	return Action{
		Type:    ActionCleanup,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}
}

func (r *Reconciler) folderCreateAction(item *Item, local bool) Action {
	side := FolderCreateRemote
	if local {
		side = FolderCreateLocal
	}

	return Action{
		Type:       ActionFolderCreate,
		DriveID:    item.DriveID,
		ItemID:     item.ItemID,
		Path:       item.Path,
		CreateSide: side,
		Item:       item,
	}
}

// --- Action plan helpers ---

// appendActions distributes actions into the correct plan buckets.
func appendActions(plan *ActionPlan, actions []Action) {
	for i := range actions {
		a := actions[i]

		switch a.Type {
		case ActionFolderCreate:
			plan.FolderCreates = append(plan.FolderCreates, a)
		case ActionLocalMove, ActionRemoteMove:
			plan.Moves = append(plan.Moves, a)
		case ActionDownload:
			plan.Downloads = append(plan.Downloads, a)
		case ActionUpload:
			plan.Uploads = append(plan.Uploads, a)
		case ActionLocalDelete:
			plan.LocalDeletes = append(plan.LocalDeletes, a)
		case ActionRemoteDelete:
			plan.RemoteDeletes = append(plan.RemoteDeletes, a)
		case ActionConflict:
			plan.Conflicts = append(plan.Conflicts, a)
		case ActionUpdateSynced:
			plan.SyncedUpdates = append(plan.SyncedUpdates, a)
		case ActionCleanup:
			plan.Cleanups = append(plan.Cleanups, a)
		}
	}
}

// pathDepth returns the number of path separators, used for ordering folder operations.
func pathDepth(path string) int {
	if path == "" {
		return 0
	}

	return strings.Count(path, "/") + 1
}

// orderPlan sorts the action plan per sync-algorithm.md §5.3:
//   - FolderCreates: shallowest first (top-down)
//   - Moves: folder moves before file moves
//   - LocalDeletes/RemoteDeletes: files first, then folders deepest-first (bottom-up)
func orderPlan(plan *ActionPlan) {
	// Folder creates: shallowest first.
	sort.SliceStable(plan.FolderCreates, func(i, j int) bool {
		return pathDepth(plan.FolderCreates[i].Path) < pathDepth(plan.FolderCreates[j].Path)
	})

	// Moves: folder moves before file moves.
	sort.SliceStable(plan.Moves, func(i, j int) bool {
		iIsFolder := plan.Moves[i].Item != nil && plan.Moves[i].Item.ItemType == ItemTypeFolder
		jIsFolder := plan.Moves[j].Item != nil && plan.Moves[j].Item.ItemType == ItemTypeFolder

		if iIsFolder != jIsFolder {
			return iIsFolder // Folders first.
		}

		return false
	})

	// Deletes: files first, then folders bottom-up (deepest first).
	orderDeletes(plan.LocalDeletes)
	orderDeletes(plan.RemoteDeletes)
}

// orderDeletes sorts delete actions: files first, then folders deepest-first.
func orderDeletes(deletes []Action) {
	sort.SliceStable(deletes, func(i, j int) bool {
		iIsFolder := deletes[i].Item != nil &&
			(deletes[i].Item.ItemType == ItemTypeFolder || deletes[i].Item.ItemType == ItemTypeRoot)
		jIsFolder := deletes[j].Item != nil &&
			(deletes[j].Item.ItemType == ItemTypeFolder || deletes[j].Item.ItemType == ItemTypeRoot)

		// Files before folders.
		if iIsFolder != jIsFolder {
			return !iIsFolder
		}

		// Among folders: deepest first (bottom-up).
		if iIsFolder && jIsFolder {
			return pathDepth(deletes[i].Path) > pathDepth(deletes[j].Path)
		}

		return false
	})
}
