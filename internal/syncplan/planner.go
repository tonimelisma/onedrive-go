package syncplan

import (
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Planner is a pure decision engine that transforms change events and
// baseline state into an ordered ActionPlan. It performs no I/O.
type Planner struct {
	logger *slog.Logger
}

// NewPlanner creates a Planner with the given logger.
func NewPlanner(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
}

// Plan takes buffered changes, the current baseline, sync mode, safety
// config, and denied prefixes, and produces an ActionPlan. Paths under
// deniedPrefixes are treated as download-only (remote writes suppressed).
// Returns ErrBigDeleteTriggered if the planned deletions exceed safety thresholds.
func (p *Planner) Plan(
	changes []synctypes.PathChanges, baseline *synctypes.Baseline, mode synctypes.SyncMode, config *synctypes.SafetyConfig,
	deniedPrefixes []string,
) (*synctypes.ActionPlan, error) {
	p.logger.Info("planning sync actions",
		slog.Int("changes", len(changes)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.String("mode", mode.String()),
		slog.Int("denied_prefixes", len(deniedPrefixes)),
	)

	views := buildPathViews(changes, baseline)

	var allActions []synctypes.Action

	// Step 1: detect and extract moves before per-path classification.
	allActions = append(allActions, detectMoves(views, changes, deniedPrefixes, baseline)...)

	// Step 2: classify each remaining path. Sort keys for deterministic
	// action order across runs with identical input (B-154).
	sortedPaths := make([]string, 0, len(views))
	for p := range views {
		sortedPaths = append(sortedPaths, p)
	}

	sort.Strings(sortedPaths)

	for _, p := range sortedPaths {
		allActions = append(allActions, classifyPathView(views[p], mode, deniedPrefixes)...)
	}

	// Step 2.5: Cascade folder deletions to baseline descendants. When the
	// delta API reports a parent folder as deleted, it does NOT report
	// individual child item deletions. This step synthesizes delete/cleanup
	// actions for all baseline descendants of deleted folders, ensuring the
	// executor can remove children before the parent directory.
	allActions = expandFolderDeleteCascades(allActions, baseline, views, mode, p.logger)

	// Step 3: build dependency edges and verify acyclicity.
	deps := buildDependencies(allActions)

	if err := detectDependencyCycle(deps); err != nil {
		return nil, err
	}

	plan := &synctypes.ActionPlan{
		Actions: allActions,
		Deps:    deps,
	}

	// Step 4: safety check for big deletes.
	counts := CountByType(plan.Actions)
	deleteCount := counts[synctypes.ActionLocalDelete] + counts[synctypes.ActionRemoteDelete]

	if exceedsDeleteThreshold(deleteCount, config.BigDeleteThreshold) {
		p.logger.Warn("big-delete protection triggered",
			slog.Int("delete_count", deleteCount),
			slog.Int("threshold", config.BigDeleteThreshold),
		)

		return nil, synctypes.ErrBigDeleteTriggered
	}

	p.logger.Info("plan complete",
		slog.Int("total_actions", len(plan.Actions)),
		slog.Int("folder_creates", counts[synctypes.ActionFolderCreate]),
		slog.Int("moves", counts[synctypes.ActionLocalMove]+counts[synctypes.ActionRemoteMove]),
		slog.Int("downloads", counts[synctypes.ActionDownload]),
		slog.Int("uploads", counts[synctypes.ActionUpload]),
		slog.Int("local_deletes", counts[synctypes.ActionLocalDelete]),
		slog.Int("remote_deletes", counts[synctypes.ActionRemoteDelete]),
		slog.Int("conflicts", counts[synctypes.ActionConflict]),
		slog.Int("synced_updates", counts[synctypes.ActionUpdateSynced]),
		slog.Int("cleanups", counts[synctypes.ActionCleanup]),
	)

	return plan, nil
}

// buildPathViews constructs a three-way PathView for each path appearing
// in change events. Paths with no local events but with a baseline entry
// derive their LocalState from the baseline (item is unchanged locally).
func buildPathViews(changes []synctypes.PathChanges, baseline *synctypes.Baseline) map[string]*synctypes.PathView {
	views := make(map[string]*synctypes.PathView, len(changes))

	for i := range changes {
		pc := &changes[i]
		view := &synctypes.PathView{Path: pc.Path}

		// Remote state from the latest remote event.
		if len(pc.RemoteEvents) > 0 {
			last := &pc.RemoteEvents[len(pc.RemoteEvents)-1]
			view.Remote = remoteStateFromEvent(last)
		}

		// Local state from the latest local event. ChangeDelete means absent.
		if len(pc.LocalEvents) > 0 {
			last := &pc.LocalEvents[len(pc.LocalEvents)-1]
			view.Local = localStateFromEvent(last)
		}

		// Baseline lookup.
		view.Baseline, _ = baseline.GetByPath(pc.Path)

		// If there are no local events but a baseline exists, derive local
		// state from baseline — the item is unchanged on disk.
		if len(pc.LocalEvents) == 0 && view.Baseline != nil {
			view.Local = localStateFromBaseline(view.Baseline)
		}

		views[pc.Path] = view
	}

	return views
}

// ---------------------------------------------------------------------------
// Cross-drive move guard
// ---------------------------------------------------------------------------

// resolvePathDriveID determines which drive owns a path by checking the
// baseline. If the path itself has no baseline entry, walks up parent
// directories until an ancestor with a baseline entry is found. Returns
// zero ID if no ancestry has a baseline entry.
func resolvePathDriveID(p string, bl *synctypes.Baseline) driveid.ID {
	// Check the path itself first.
	if entry, ok := bl.GetByPath(p); ok {
		return entry.DriveID
	}

	// Walk up parent directories.
	for dir := filepath.Dir(p); dir != "." && dir != "" && dir != "/"; dir = filepath.Dir(dir) {
		if entry, ok := bl.GetByPath(dir); ok {
			return entry.DriveID
		}
	}

	return driveid.ID{}
}

// isCrossDriveLocalMove returns true when a hash-correlated delete+create
// pair spans different drives (e.g., own drive → shortcut folder). The
// Graph API MoveItem is a single-drive operation, so cross-drive moves
// must be decomposed into a delete + upload.
func isCrossDriveLocalMove(deletePath, createPath string, views map[string]*synctypes.PathView, bl *synctypes.Baseline) bool {
	// Source drive comes from the deleted item's baseline.
	deleteView := views[deletePath]
	if deleteView == nil || deleteView.Baseline == nil {
		return false // no baseline → can't determine source drive; don't decompose
	}

	sourceDrive := deleteView.Baseline.DriveID
	destDrive := resolvePathDriveID(createPath, bl)

	// Conservative: if either drive is unknown, don't decompose — let the
	// normal move path handle it (it'll fail and get retried as separate ops).
	if sourceDrive.IsZero() || destDrive.IsZero() {
		return false
	}

	return !sourceDrive.Equal(destDrive)
}

// isCrossDriveRemoteMove returns true when a remote ChangeMove event
// has different drive IDs in the baseline (source) and remote (destination).
// Cross-drive remote moves from the API shouldn't happen in practice, but
// guard defensively.
func isCrossDriveRemoteMove(view *synctypes.PathView) bool {
	if view.Baseline == nil || view.Remote == nil {
		return false
	}

	sourceDrive := view.Baseline.DriveID
	destDrive := view.Remote.DriveID

	if sourceDrive.IsZero() || destDrive.IsZero() {
		return false
	}

	return !sourceDrive.Equal(destDrive)
}

// detectMoves finds remote and local moves, produces move actions, and
// removes matched paths from the views map so they do not enter per-path
// classification.
func detectMoves(
	views map[string]*synctypes.PathView, changes []synctypes.PathChanges, deniedPrefixes []string, bl *synctypes.Baseline,
) []synctypes.Action {
	var actions []synctypes.Action

	// Remote moves: scan for ChangeMove events in remote events.
	actions = append(actions, detectRemoteMoves(views, changes)...)

	// Local moves: hash-based correlation of delete+create pairs.
	actions = append(actions, detectLocalMoves(views, deniedPrefixes, bl)...)

	return actions
}

// detectRemoteMoves finds ChangeMove events in remote observations and
// produces ActionLocalMove actions (rename local file to match remote).
func detectRemoteMoves(views map[string]*synctypes.PathView, changes []synctypes.PathChanges) []synctypes.Action {
	var actions []synctypes.Action

	for i := range changes {
		pc := &changes[i]
		for j := range pc.RemoteEvents {
			ev := &pc.RemoteEvents[j]
			if ev.Type != synctypes.ChangeMove {
				continue
			}

			// The move event's Path is the new path; OldPath is where it was.
			view := views[pc.Path]
			if view == nil {
				continue
			}

			// Cross-drive guard: if the server reports a move across drives,
			// skip the move action and let the paths classify as separate
			// delete + download. This shouldn't happen in practice but
			// guards defensively.
			if isCrossDriveRemoteMove(view) {
				continue
			}

			action := MakeAction(synctypes.ActionLocalMove, view)
			action.OldPath = ev.OldPath
			// action.Path is already ev.Path (destination) via MakeAction.

			actions = append(actions, action)

			// Always remove the new path (fully handled by move action).
			delete(views, ev.Path)

			// Only remove old path if no new item appeared there.
			// If a new item exists (Remote.IsDeleted=false from a ChangeCreate
			// after the synthetic delete), keep it in views but clear Baseline
			// and Local so it classifies as a new item (EF14/ED3), not a
			// conflict against the moved item's stale baseline.
			oldView := views[ev.OldPath]
			if oldView == nil || (oldView.Remote != nil && oldView.Remote.IsDeleted) {
				delete(views, ev.OldPath)
			} else {
				oldView.Baseline = nil
				oldView.Local = nil
			}
		}
	}

	return actions
}

// detectLocalMoves correlates local deletes with local creates by hash
// to detect renames. Only unique matches (exactly one delete and one
// create with the same hash) produce move actions. Ambiguous cases are
// skipped and fall through to separate delete+create.
func detectLocalMoves(views map[string]*synctypes.PathView, deniedPrefixes []string, bl *synctypes.Baseline) []synctypes.Action {
	deletesByHash, createsByHash := buildLocalMoveHashMaps(views)

	// Sort hash keys for deterministic move detection order (B-154).
	sortedHashes := make([]string, 0, len(deletesByHash))
	for h := range deletesByHash {
		sortedHashes = append(sortedHashes, h)
	}

	sort.Strings(sortedHashes)

	var actions []synctypes.Action

	for _, hash := range sortedHashes {
		delPaths := deletesByHash[hash]
		crePaths, ok := createsByHash[hash]
		if !ok || len(delPaths) != 1 || len(crePaths) != 1 {
			continue // no match or ambiguous
		}

		deletePath := delPaths[0]
		createPath := crePaths[0]

		if shouldSkipLocalMove(deletePath, createPath, views, deniedPrefixes, bl) {
			continue
		}

		view := views[deletePath]

		action := MakeAction(synctypes.ActionRemoteMove, view)
		action.OldPath = deletePath
		action.Path = createPath

		actions = append(actions, action)

		// Remove both paths from classification.
		delete(views, deletePath)
		delete(views, createPath)
	}

	return actions
}

// buildLocalMoveHashMaps indexes local deletes and creates by content hash
// for move correlation.
func buildLocalMoveHashMaps(views map[string]*synctypes.PathView) (deletesByHash, createsByHash map[string][]string) {
	deletesByHash = make(map[string][]string)
	createsByHash = make(map[string][]string)

	for p, view := range views {
		if view.Local == nil && view.Baseline != nil && view.Baseline.LocalHash != "" {
			deletesByHash[view.Baseline.LocalHash] = append(deletesByHash[view.Baseline.LocalHash], p)
		}

		if view.Local != nil && view.Baseline == nil && view.Local.Hash != "" {
			createsByHash[view.Local.Hash] = append(createsByHash[view.Local.Hash], p)
		}
	}

	return deletesByHash, createsByHash
}

// shouldSkipLocalMove returns true if a hash-matched delete+create pair
// should NOT be treated as a move (permission denied or cross-drive).
func shouldSkipLocalMove(
	deletePath, createPath string, views map[string]*synctypes.PathView,
	deniedPrefixes []string, bl *synctypes.Baseline,
) bool {
	// Skip local moves under permission-denied folders — can't write to remote.
	if IsWriteDenied(deletePath, deniedPrefixes) || IsWriteDenied(createPath, deniedPrefixes) {
		return true
	}

	// Cross-drive guard: MoveItem is a single-drive API call. When source
	// and destination are on different drives, skip the move match — the
	// paths fall through to normal per-path classification which will
	// produce a delete + upload instead.
	return isCrossDriveLocalMove(deletePath, createPath, views, bl)
}

// classifyPathView determines actions for a single path view based on
// the item type and sync mode. Paths under deniedPrefixes are treated
// as download-only (remote writes suppressed).
func classifyPathView(view *synctypes.PathView, mode synctypes.SyncMode, deniedPrefixes []string) []synctypes.Action {
	// Under a denied prefix, behave as download-only: we cannot write to remote.
	effectiveMode := mode
	if IsWriteDenied(view.Path, deniedPrefixes) {
		effectiveMode = synctypes.SyncDownloadOnly
	}

	itemType := resolveItemType(view)

	if itemType == synctypes.ItemTypeFolder {
		return classifyFolder(view, effectiveMode)
	}

	return classifyFile(view, effectiveMode)
}

// IsWriteDenied checks if a path falls under a permission-denied folder.
func IsWriteDenied(filePath string, deniedPrefixes []string) bool {
	for _, prefix := range deniedPrefixes {
		if filePath == prefix || strings.HasPrefix(filePath, prefix+"/") {
			return true
		}
	}

	return false
}

// classifyFile dispatches to the appropriate file classification function
// based on whether a baseline entry exists.
func classifyFile(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	if view.Baseline != nil {
		return classifyFileWithBaseline(view, mode)
	}

	return classifyFileNoBaseline(view, mode)
}

// classifyFileWithBaseline handles EF1-EF10: files that have a baseline
// entry (previously synced).
func classifyFileWithBaseline(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	localChanged := detectLocalChange(view)
	remoteChanged := detectRemoteChange(view)

	// Mode filtering: suppress the side we are not syncing.
	if mode == synctypes.SyncDownloadOnly {
		localChanged = false
	}

	if mode == synctypes.SyncUploadOnly {
		remoteChanged = false
	}

	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted
	localDeleted := view.Baseline != nil && !hasLocal

	return classifyFileWithFlags(view, localChanged, remoteChanged, hasRemote, remoteDeleted, localDeleted)
}

// classifyFileWithFlags implements the EF1-EF10 decision matrix using
// pre-computed boolean flags. Dispatches to sub-functions to keep
// cyclomatic complexity under the threshold.
func classifyFileWithFlags(
	view *synctypes.PathView, localChanged, remoteChanged, hasRemote, remoteDeleted, localDeleted bool,
) []synctypes.Action {
	// EF1: both sides unchanged — no-op.
	if !localChanged && !remoteChanged {
		return nil
	}

	// When local is deleted, use the delete-specific decision paths.
	if localDeleted {
		return classifyFileLocalDeleted(view, remoteChanged, hasRemote, remoteDeleted)
	}

	return classifyFileLocalPresent(view, localChanged, remoteChanged, hasRemote, remoteDeleted)
}

// classifyFileLocalDeleted handles EF6, EF7, EF10: the local side has
// been deleted (baseline exists but file is absent locally).
func classifyFileLocalDeleted(view *synctypes.PathView, remoteChanged, hasRemote, remoteDeleted bool) []synctypes.Action {
	switch {
	case !remoteChanged && !remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionRemoteDelete, view)} // EF6
	case remoteChanged && hasRemote:
		return []synctypes.Action{MakeAction(synctypes.ActionDownload, view)} // EF7: remote wins
	case remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionCleanup, view)} // EF10
	}

	return nil
}

// classifyFileLocalPresent handles EF2, EF3, EF4, EF5, EF8, EF9: the
// local file is still present (not deleted).
func classifyFileLocalPresent(
	view *synctypes.PathView, localChanged, remoteChanged, hasRemote, remoteDeleted bool,
) []synctypes.Action {
	switch {
	case !localChanged && remoteChanged && hasRemote:
		return []synctypes.Action{MakeAction(synctypes.ActionDownload, view)} // EF2
	case localChanged && !remoteChanged:
		return []synctypes.Action{MakeAction(synctypes.ActionUpload, view)} // EF3
	case localChanged && remoteChanged && hasRemote:
		if view.Local != nil && view.Local.Hash == view.Remote.Hash {
			return []synctypes.Action{MakeAction(synctypes.ActionUpdateSynced, view)} // EF4: convergent edit
		}
		return []synctypes.Action{makeConflictAction(view, synctypes.ConflictEditEdit)} // EF5
	case !localChanged && remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionLocalDelete, view)} // EF8
	case localChanged && remoteDeleted:
		return []synctypes.Action{makeConflictAction(view, synctypes.ConflictEditDelete)} // EF9
	}

	return nil
}

// classifyFileNoBaseline handles EF11-EF14: files that have no baseline
// entry (never synced before).
func classifyFileNoBaseline(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil

	// Mode filtering for no-baseline files.
	if mode == synctypes.SyncDownloadOnly {
		hasLocal = false
	}

	if mode == synctypes.SyncUploadOnly {
		hasRemote = false
	}

	switch {
	case hasLocal && hasRemote:
		if view.Local.Hash == view.Remote.Hash {
			return []synctypes.Action{MakeAction(synctypes.ActionUpdateSynced, view)} // EF11: convergent create
		}
		return []synctypes.Action{makeConflictAction(view, synctypes.ConflictCreateCreate)} // EF12

	case hasLocal && !hasRemote:
		return []synctypes.Action{MakeAction(synctypes.ActionUpload, view)} // EF13

	case !hasLocal && hasRemote:
		return []synctypes.Action{MakeAction(synctypes.ActionDownload, view)} // EF14
	}

	return nil
}

// classifyFolder handles ED1-ED8: folder decision matrix. Dispatches
// to sub-functions based on baseline presence to keep complexity down.
func classifyFolder(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	hasBaseline := view.Baseline != nil

	if hasBaseline {
		return classifyFolderWithBaseline(view, mode)
	}

	return classifyFolderNoBaseline(view, mode)
}

// classifyFolderWithBaseline handles ED1, ED4, ED6, ED7, ED8: folders
// that have a baseline entry (previously synced).
func classifyFolderWithBaseline(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted
	localDeleted := !hasLocal // baseline exists (we're in WithBaseline)

	// Upfront mode filtering — parallel to classifyFileWithBaseline.
	// Defense in depth: the engine already skips observers for suppressed
	// sides, but the planner should be self-contained.
	if mode == synctypes.SyncDownloadOnly {
		localDeleted = false
	}

	if mode == synctypes.SyncUploadOnly {
		hasRemote = false
		remoteDeleted = false
	}

	return classifyFolderWithFlags(view, localDeleted, hasRemote, remoteDeleted)
}

// classifyFolderWithFlags implements the ED1, ED4, ED6, ED7, ED8 decision
// matrix using pre-computed boolean flags.
func classifyFolderWithFlags(view *synctypes.PathView, localDeleted, hasRemote, remoteDeleted bool) []synctypes.Action {
	switch {
	case !localDeleted && hasRemote:
		return nil // ED1: in sync

	case localDeleted && hasRemote:
		return []synctypes.Action{makeFolderCreate(view, synctypes.CreateLocal)} // ED4: remote wins

	case !localDeleted && remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionLocalDelete, view)} // ED6

	case localDeleted && remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionCleanup, view)} // ED7: both deleted

	case localDeleted && !hasRemote && !remoteDeleted:
		return []synctypes.Action{MakeAction(synctypes.ActionRemoteDelete, view)} // ED8: propagate delete
	}

	return nil
}

// classifyFolderNoBaseline handles ED2, ED3, ED5: folders that have
// no baseline entry (never synced before).
func classifyFolderNoBaseline(view *synctypes.PathView, mode synctypes.SyncMode) []synctypes.Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted

	// Upfront mode filtering — parallel to classifyFileNoBaseline.
	if mode == synctypes.SyncDownloadOnly {
		hasLocal = false
	}

	if mode == synctypes.SyncUploadOnly {
		hasRemote = false
	}

	switch {
	case hasLocal && hasRemote:
		return []synctypes.Action{MakeAction(synctypes.ActionUpdateSynced, view)} // ED2: adopt

	case !hasLocal && hasRemote:
		return []synctypes.Action{makeFolderCreate(view, synctypes.CreateLocal)} // ED3

	case hasLocal && !hasRemote && !remoteDeleted:
		return []synctypes.Action{makeFolderCreate(view, synctypes.CreateRemote)} // ED5
	}

	return nil
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// remoteStateFromEvent constructs a RemoteState from a ChangeEvent.
func remoteStateFromEvent(ev *synctypes.ChangeEvent) *synctypes.RemoteState {
	return &synctypes.RemoteState{
		ItemID:        ev.ItemID,
		DriveID:       ev.DriveID,
		ParentID:      ev.ParentID,
		Name:          ev.Name,
		ItemType:      ev.ItemType,
		Size:          ev.Size,
		Hash:          ev.Hash,
		Mtime:         ev.Mtime,
		ETag:          ev.ETag,
		CTag:          ev.CTag,
		IsDeleted:     ev.IsDeleted,
		RemoteDriveID: ev.RemoteDriveID,
		RemoteItemID:  ev.RemoteItemID,
	}
}

// localStateFromEvent constructs a LocalState from a ChangeEvent.
// Returns nil if the event is a deletion (item is absent locally).
func localStateFromEvent(ev *synctypes.ChangeEvent) *synctypes.LocalState {
	if ev.Type == synctypes.ChangeDelete {
		return nil
	}

	return &synctypes.LocalState{
		Name:     ev.Name,
		ItemType: ev.ItemType,
		Size:     ev.Size,
		Hash:     ev.Hash,
		Mtime:    ev.Mtime,
	}
}

// localStateFromBaseline derives a LocalState from a baseline entry for
// paths with no local events (item is unchanged on disk).
func localStateFromBaseline(entry *synctypes.BaselineEntry) *synctypes.LocalState {
	return &synctypes.LocalState{
		Name:     path.Base(entry.Path),
		ItemType: entry.ItemType,
		Size:     entry.LocalSize,
		Hash:     entry.LocalHash,
		Mtime:    entry.LocalMtime,
	}
}

// detectLocalChange returns true if the local state differs from the
// baseline. A missing local state (deleted file) counts as changed.
func detectLocalChange(view *synctypes.PathView) bool {
	if view.Baseline == nil {
		return view.Local != nil
	}

	// A nil local state means the file was deleted, which counts as a change.
	if view.Local == nil {
		return true
	}

	// Folders have no content hash; existence is the only signal.
	if view.Baseline.ItemType == synctypes.ItemTypeFolder {
		return false
	}

	return fileSideChanged(
		view.Local.Hash,
		view.Baseline.LocalHash,
		view.Local.Size,
		true,
		view.Baseline.LocalSize,
		view.Baseline.LocalSizeKnown,
		view.Local.Mtime,
		view.Baseline.LocalMtime,
		"",
		"",
		false,
	)
}

// detectRemoteChange returns true if the remote state differs from the
// baseline. A nil Remote means no observation (not "unchanged").
func detectRemoteChange(view *synctypes.PathView) bool {
	if view.Baseline == nil {
		return view.Remote != nil && !view.Remote.IsDeleted
	}

	if view.Remote == nil {
		return false // no observation = no change
	}

	if view.Remote.IsDeleted {
		return true
	}

	// Folders have no content hash; existence is the only signal.
	if view.Baseline.ItemType == synctypes.ItemTypeFolder {
		return false
	}

	return fileSideChanged(
		view.Remote.Hash,
		view.Baseline.RemoteHash,
		view.Remote.Size,
		true,
		view.Baseline.RemoteSize,
		view.Baseline.RemoteSizeKnown,
		view.Remote.Mtime,
		view.Baseline.RemoteMtime,
		view.Remote.ETag,
		view.Baseline.ETag,
		true,
	)
}

func fileSideChanged(
	currentHash, baselineHash string,
	currentSize int64, currentSizeKnown bool,
	baselineSize int64, baselineSizeKnown bool,
	currentMtime, baselineMtime int64,
	currentETag, baselineETag string,
	requireETag bool,
) bool {
	if currentHash != "" || baselineHash != "" {
		return currentHash != baselineHash
	}

	if !currentSizeKnown || !baselineSizeKnown {
		return true
	}

	if currentSize != baselineSize {
		return true
	}

	// Unknown timestamps are never treated as equality for hashless files.
	if currentMtime == 0 || baselineMtime == 0 {
		return true
	}

	if currentMtime != baselineMtime {
		return true
	}

	if requireETag {
		// Unknown eTags are never equality signals for remote hashless files.
		if currentETag == "" || baselineETag == "" {
			return true
		}

		if currentETag != baselineETag {
			return true
		}
	}

	return false
}

// resolveItemType determines the item type by checking Remote, Local,
// then Baseline. Defaults to ItemTypeFile if none provide a type.
//
// Special case: when the delta API reports a deleted item, it may strip
// the folder facet — making item.IsFolder=false even for folders. When
// Remote is deleted and its ItemType is the default (ItemTypeFile), we
// fall through to Baseline which has the correct type from when the item
// was alive. This ensures folder deletes are correctly identified for
// dependency ordering in buildDependencies/addChildDeleteDeps.
func resolveItemType(view *synctypes.PathView) synctypes.ItemType {
	if view == nil {
		return synctypes.ItemTypeFile
	}

	if view.Remote != nil {
		// When the remote item is deleted, the delta API may omit the folder
		// facet, causing ItemType to default to ItemTypeFile. If a baseline
		// exists with a non-file type, prefer it — the baseline recorded the
		// correct type when the item was still alive.
		if view.Remote.IsDeleted &&
			view.Remote.ItemType == synctypes.ItemTypeFile &&
			view.Baseline != nil &&
			view.Baseline.ItemType != synctypes.ItemTypeFile {
			return view.Baseline.ItemType
		}

		return view.Remote.ItemType
	}

	if view.Local != nil {
		return view.Local.ItemType
	}

	if view.Baseline != nil {
		return view.Baseline.ItemType
	}

	return synctypes.ItemTypeFile
}

// MakeAction constructs an Action with type, path, and IDs populated from
// the PathView.
//
// DriveID propagation contract:
//   - Remote.DriveID is authoritative for cross-drive items (shared folders
//     from Drive A appearing in Drive B's delta carry Drive A's DriveID).
//   - Baseline.DriveID is the fallback for items with no remote observation.
//   - Empty DriveID for new local items (EF13, ED5) — the executor fills
//     this from its per-drive Engine context before making API calls.
//   - Empty ItemID for new items — assigned by the API on creation.
func MakeAction(actionType synctypes.ActionType, view *synctypes.PathView) synctypes.Action {
	a := synctypes.Action{
		Type: actionType,
		Path: view.Path,
		View: view,
	}

	// Remote provides ItemID and DriveID.
	if view.Remote != nil {
		a.ItemID = view.Remote.ItemID
	}

	// DriveID: prefer Remote (handles cross-drive items correctly),
	// fall back to Baseline (for items with no remote observation).
	if view.Remote != nil && !view.Remote.DriveID.IsZero() {
		a.DriveID = view.Remote.DriveID
	}

	if a.DriveID.IsZero() && view.Baseline != nil {
		a.DriveID = view.Baseline.DriveID
	}

	// Baseline provides a fallback ItemID when Remote is absent.
	if a.ItemID == "" && view.Baseline != nil {
		a.ItemID = view.Baseline.ItemID
	}

	// Shortcut scope enrichment (D-5): flow shortcut identity from
	// observation through to the action so active-scope matching can
	// distinguish own-drive vs shortcut-scoped failures (R-6.8.12, R-6.8.13).
	if view.Remote != nil && view.Remote.RemoteDriveID != "" {
		a.TargetShortcutKey = view.Remote.RemoteDriveID + ":" + view.Remote.RemoteItemID
		a.TargetDriveID = driveid.New(view.Remote.RemoteDriveID)
	}

	return a
}

// makeConflictAction constructs an ActionConflict with ConflictInfo populated.
func makeConflictAction(view *synctypes.PathView, conflictType string) synctypes.Action {
	a := MakeAction(synctypes.ActionConflict, view)

	record := &synctypes.ConflictRecord{
		Path:         view.Path,
		ConflictType: conflictType,
	}

	if view.Local != nil {
		record.LocalHash = view.Local.Hash
		record.LocalMtime = view.Local.Mtime
	}

	if view.Remote != nil {
		record.RemoteHash = view.Remote.Hash
		record.RemoteMtime = view.Remote.Mtime
		record.ItemID = view.Remote.ItemID
	}

	record.DriveID = a.DriveID
	a.ConflictInfo = record

	return a
}

// makeFolderCreate constructs an ActionFolderCreate action with the
// specified creation side (local or remote).
func makeFolderCreate(view *synctypes.PathView, side synctypes.FolderCreateSide) synctypes.Action {
	a := MakeAction(synctypes.ActionFolderCreate, view)
	a.CreateSide = side

	return a
}

// expandFolderDeleteCascades synthesizes delete/cleanup actions for baseline
// descendants of deleted folders. The Graph API delta endpoint only reports
// the parent folder deletion — children are silently dropped. Without this
// expansion, the executor's DeleteLocalFolder sees a non-empty directory and
// refuses to remove it ("blocked by non-disposable files").
//
// Logic:
//  1. Build existingPaths set from current actions (prevents duplicates when
//     delta reports both parent and child).
//  2. For each folder delete/cleanup action, walk baseline.DescendantsOf.
//  3. For each descendant not already in existingPaths, synthesize:
//     - ActionLocalDelete if the item exists locally (preserves hash-before-delete S4)
//     - ActionCleanup if already gone locally
//  4. Return append(actions, cascaded...)
func expandFolderDeleteCascades(
	actions []synctypes.Action,
	baseline *synctypes.Baseline,
	views map[string]*synctypes.PathView,
	mode synctypes.SyncMode,
	logger *slog.Logger,
) []synctypes.Action {
	// Upload-only mode never deletes locally — skip cascade entirely.
	if mode == synctypes.SyncUploadOnly {
		return actions
	}

	// Collect paths that already have actions to avoid duplicates.
	existingPaths := make(map[string]struct{}, len(actions))
	for i := range actions {
		existingPaths[actions[i].Path] = struct{}{}
	}

	var cascaded []synctypes.Action

	for i := range actions {
		a := &actions[i]

		// Only cascade for folder delete/cleanup actions.
		isDelete := a.Type == synctypes.ActionLocalDelete || a.Type == synctypes.ActionCleanup
		if !isDelete {
			continue
		}

		// Verify this is a folder action.
		if resolveItemType(a.View) != synctypes.ItemTypeFolder {
			continue
		}

		descendants := baseline.DescendantsOf(a.Path)
		if len(descendants) == 0 {
			continue
		}

		logger.Debug("cascading folder delete to descendants",
			slog.String("folder", a.Path),
			slog.Int("descendant_count", len(descendants)),
		)

		for _, desc := range descendants {
			if _, exists := existingPaths[desc.Path]; exists {
				continue
			}

			// Build a synthetic PathView for the descendant. The remote
			// side is deleted (inherited from parent), and the local side
			// is derived from baseline (item may or may not still exist
			// on disk — the executor's hash-before-delete check handles
			// the case where it was locally modified).
			descView := &synctypes.PathView{
				Path:     desc.Path,
				Baseline: desc,
				Remote: &synctypes.RemoteState{
					ItemID:    desc.ItemID,
					DriveID:   desc.DriveID,
					ItemType:  desc.ItemType,
					IsDeleted: true,
				},
			}

			// Derive local state from baseline — item is assumed unchanged
			// locally (the planner had no local events for it).
			if existingView, ok := views[desc.Path]; ok && existingView.Local != nil {
				descView.Local = existingView.Local
			} else {
				descView.Local = localStateFromBaseline(desc)
			}

			// Choose action type: if the item exists locally (Local != nil),
			// use ActionLocalDelete (executor verifies hash before deleting).
			// If already absent locally, use ActionCleanup (baseline removal only).
			actionType := synctypes.ActionLocalDelete
			if descView.Local == nil {
				actionType = synctypes.ActionCleanup
			}

			cascaded = append(cascaded, MakeAction(actionType, descView))
			existingPaths[desc.Path] = struct{}{}
		}
	}

	if len(cascaded) > 0 {
		logger.Info("folder delete cascade expanded",
			slog.Int("cascaded_actions", len(cascaded)),
		)
	}

	return append(actions, cascaded...)
}

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) move target parent must exist first.
func buildDependencies(actions []synctypes.Action) [][]int {
	deps := make([][]int, len(actions))

	// Index folder creates by path for quick lookup.
	folderCreateIdx := make(map[string]int)
	// Index all deletes by path for child→parent edges.
	deleteIdx := make(map[string]int)

	for i := range actions {
		if actions[i].Type == synctypes.ActionFolderCreate {
			folderCreateIdx[actions[i].Path] = i
		}

		isDelete := actions[i].Type == synctypes.ActionLocalDelete ||
			actions[i].Type == synctypes.ActionRemoteDelete ||
			actions[i].Type == synctypes.ActionCleanup
		if isDelete {
			deleteIdx[actions[i].Path] = i
		}
	}

	for i := range actions {
		deps[i] = addParentFolderDep(deps[i], i, &actions[i], folderCreateIdx)
		deps[i] = addChildDeleteDeps(deps[i], i, &actions[i], deleteIdx)
		// Sort dependency indices for reproducible ordering (B-154).
		sort.Ints(deps[i])
	}

	return deps
}

// addParentFolderDep adds a dependency on a parent folder create if present.
func addParentFolderDep(deps []int, idx int, a *synctypes.Action, folderCreateIdx map[string]int) []int {
	parentDir := filepath.Dir(a.Path)
	if parentDir == "." || parentDir == "" {
		return deps
	}

	parentDir = filepath.ToSlash(parentDir)

	if fcIdx, ok := folderCreateIdx[parentDir]; ok && fcIdx != idx {
		deps = append(deps, fcIdx)
	}

	return deps
}

// addChildDeleteDeps makes folder deletes depend on child deletes at deeper paths.
func addChildDeleteDeps(deps []int, idx int, a *synctypes.Action, deleteIdx map[string]int) []int {
	if a.Type != synctypes.ActionLocalDelete && a.Type != synctypes.ActionRemoteDelete {
		return deps
	}

	if resolveItemType(a.View) != synctypes.ItemTypeFolder {
		return deps
	}

	prefix := a.Path + "/"

	for childPath, childIdx := range deleteIdx {
		if childIdx != idx && strings.HasPrefix(childPath, prefix) {
			deps = append(deps, childIdx)
		}
	}

	return deps
}

// CountByType counts actions grouped by ActionType. Exported for use by the
// sync engine when building pass reports from plan counts.
func CountByType(actions []synctypes.Action) map[synctypes.ActionType]int {
	counts := make(map[synctypes.ActionType]int)
	for i := range actions {
		counts[actions[i].Type]++
	}

	return counts
}

// detectDependencyCycle performs a DFS to check for cycles in the dependency
// graph. Returns ErrDependencyCycle if any cycle is found. Uses standard
// white/gray/black three-color marking (B-313).
func detectDependencyCycle(deps [][]int) error {
	const (
		white = 0 // unvisited
		gray  = 1 // in current DFS path
		black = 2 // fully explored, no cycle
	)

	color := make([]int, len(deps))

	var dfs func(node int) bool
	dfs = func(node int) bool {
		color[node] = gray

		for _, neighbor := range deps[node] {
			switch color[neighbor] {
			case gray:
				return true // back edge → cycle
			case white:
				if dfs(neighbor) {
					return true
				}
			}
		}

		color[node] = black

		return false
	}

	for i := range deps {
		if color[i] == white {
			if dfs(i) {
				return synctypes.ErrDependencyCycle
			}
		}
	}

	return nil
}

// exceedsDeleteThreshold returns true if the planned delete count exceeds
// the configured threshold. A threshold of 0 disables the check.
func exceedsDeleteThreshold(deleteCount, threshold int) bool {
	return threshold > 0 && deleteCount > threshold
}
