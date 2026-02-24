package sync

import (
	"errors"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// SafetyConfig controls big-delete protection thresholds.
type SafetyConfig struct {
	BigDeleteMinItems   int     // baseline must have at least this many items before big-delete check applies
	BigDeleteMaxCount   int     // max number of delete actions before triggering
	BigDeleteMaxPercent float64 // max percentage of baseline items being deleted
}

// Named constants for safety defaults (avoids mnd lint).
const (
	defaultBigDeleteMinItems   = 10
	defaultBigDeleteMaxCount   = 1000
	defaultBigDeleteMaxPercent = 50.0
	percentMultiplier          = 100.0
)

// DefaultSafetyConfig returns a SafetyConfig with sensible defaults:
// min 10 items, max 1000 deletes, max 50% of baseline.
func DefaultSafetyConfig() *SafetyConfig {
	return &SafetyConfig{
		BigDeleteMinItems:   defaultBigDeleteMinItems,
		BigDeleteMaxCount:   defaultBigDeleteMaxCount,
		BigDeleteMaxPercent: defaultBigDeleteMaxPercent,
	}
}

// ErrBigDeleteTriggered indicates that the planned number of deletions
// exceeds safety thresholds. The sync cycle should halt and require
// user confirmation before proceeding.
var ErrBigDeleteTriggered = errors.New("sync: big-delete protection triggered")

// Planner is a pure decision engine that transforms change events and
// baseline state into an ordered ActionPlan. It performs no I/O.
type Planner struct {
	logger *slog.Logger
}

// NewPlanner creates a Planner with the given logger.
func NewPlanner(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
}

// Plan takes buffered changes, the current baseline, sync mode, and safety
// config, and produces an ActionPlan. Returns ErrBigDeleteTriggered if
// the planned deletions exceed safety thresholds.
func (p *Planner) Plan(
	changes []PathChanges, baseline *Baseline, mode SyncMode, config *SafetyConfig,
) (*ActionPlan, error) {
	p.logger.Info("planning sync actions",
		slog.Int("changes", len(changes)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.String("mode", mode.String()),
	)

	views := buildPathViews(changes, baseline)

	var allActions []Action

	// Step 1: detect and extract moves before per-path classification.
	allActions = append(allActions, detectMoves(views, changes)...)

	// Step 2: classify each remaining path.
	for _, view := range views {
		allActions = append(allActions, classifyPathView(view, mode)...)
	}

	// Step 3: build dependency edges.
	deps := buildDependencies(allActions)

	plan := &ActionPlan{
		Actions: allActions,
		Deps:    deps,
		CycleID: uuid.New().String(),
	}

	// Step 4: safety check for big deletes.
	counts := countByType(plan.Actions)
	deleteCount := counts[ActionLocalDelete] + counts[ActionRemoteDelete]

	if bigDeleteTriggered(deleteCount, baseline, config) {
		p.logger.Warn("big-delete protection triggered",
			slog.Int("delete_count", deleteCount),
			slog.Int("baseline_count", baseline.Len()),
			slog.Int("max_count", config.BigDeleteMaxCount),
			slog.Float64("max_percent", config.BigDeleteMaxPercent),
		)

		return nil, ErrBigDeleteTriggered
	}

	p.logger.Info("plan complete",
		slog.Int("total_actions", len(plan.Actions)),
		slog.Int("folder_creates", counts[ActionFolderCreate]),
		slog.Int("moves", counts[ActionLocalMove]+counts[ActionRemoteMove]),
		slog.Int("downloads", counts[ActionDownload]),
		slog.Int("uploads", counts[ActionUpload]),
		slog.Int("local_deletes", counts[ActionLocalDelete]),
		slog.Int("remote_deletes", counts[ActionRemoteDelete]),
		slog.Int("conflicts", counts[ActionConflict]),
		slog.Int("synced_updates", counts[ActionUpdateSynced]),
		slog.Int("cleanups", counts[ActionCleanup]),
	)

	return plan, nil
}

// buildPathViews constructs a three-way PathView for each path appearing
// in change events. Paths with no local events but with a baseline entry
// derive their LocalState from the baseline (item is unchanged locally).
func buildPathViews(changes []PathChanges, baseline *Baseline) map[string]*PathView {
	views := make(map[string]*PathView, len(changes))

	for i := range changes {
		pc := &changes[i]
		view := &PathView{Path: pc.Path}

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

// detectMoves finds remote and local moves, produces move actions, and
// removes matched paths from the views map so they do not enter per-path
// classification.
func detectMoves(views map[string]*PathView, changes []PathChanges) []Action {
	var actions []Action

	// Remote moves: scan for ChangeMove events in remote events.
	actions = append(actions, detectRemoteMoves(views, changes)...)

	// Local moves: hash-based correlation of delete+create pairs.
	actions = append(actions, detectLocalMoves(views)...)

	return actions
}

// detectRemoteMoves finds ChangeMove events in remote observations and
// produces ActionLocalMove actions (rename local file to match remote).
func detectRemoteMoves(views map[string]*PathView, changes []PathChanges) []Action {
	var actions []Action

	for i := range changes {
		pc := &changes[i]
		for j := range pc.RemoteEvents {
			ev := &pc.RemoteEvents[j]
			if ev.Type != ChangeMove {
				continue
			}

			// The move event's Path is the new path; OldPath is where it was.
			view := views[pc.Path]
			if view == nil {
				continue
			}

			action := makeAction(ActionLocalMove, view)
			action.Path = ev.OldPath
			action.NewPath = ev.Path

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
func detectLocalMoves(views map[string]*PathView) []Action {
	// Build hash-keyed maps of candidates.
	deletesByHash := make(map[string][]string) // hash -> [paths]
	createsByHash := make(map[string][]string) // hash -> [paths]

	for p, view := range views {
		if view.Local == nil && view.Baseline != nil && view.Baseline.LocalHash != "" {
			deletesByHash[view.Baseline.LocalHash] = append(deletesByHash[view.Baseline.LocalHash], p)
		}

		if view.Local != nil && view.Baseline == nil && view.Local.Hash != "" {
			createsByHash[view.Local.Hash] = append(createsByHash[view.Local.Hash], p)
		}
	}

	var actions []Action

	for hash, delPaths := range deletesByHash {
		crePaths, ok := createsByHash[hash]
		if !ok {
			continue
		}

		// Unique match constraint: exactly one of each.
		if len(delPaths) != 1 || len(crePaths) != 1 {
			continue
		}

		deletePath := delPaths[0]
		createPath := crePaths[0]
		view := views[deletePath]

		action := makeAction(ActionRemoteMove, view)
		action.Path = deletePath
		action.NewPath = createPath

		actions = append(actions, action)

		// Remove both paths from classification.
		delete(views, deletePath)
		delete(views, createPath)
	}

	return actions
}

// classifyPathView determines actions for a single path view based on
// the item type and sync mode.
func classifyPathView(view *PathView, mode SyncMode) []Action {
	itemType := resolveItemType(view)

	if itemType == ItemTypeFolder {
		return classifyFolder(view, mode)
	}

	return classifyFile(view, mode)
}

// classifyFile dispatches to the appropriate file classification function
// based on whether a baseline entry exists.
func classifyFile(view *PathView, mode SyncMode) []Action {
	if view.Baseline != nil {
		return classifyFileWithBaseline(view, mode)
	}

	return classifyFileNoBaseline(view, mode)
}

// classifyFileWithBaseline handles EF1-EF10: files that have a baseline
// entry (previously synced).
func classifyFileWithBaseline(view *PathView, mode SyncMode) []Action {
	localChanged := detectLocalChange(view)
	remoteChanged := detectRemoteChange(view)

	// Mode filtering: suppress the side we are not syncing.
	if mode == SyncDownloadOnly {
		localChanged = false
	}

	if mode == SyncUploadOnly {
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
	view *PathView, localChanged, remoteChanged, hasRemote, remoteDeleted, localDeleted bool,
) []Action {
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
func classifyFileLocalDeleted(view *PathView, remoteChanged, hasRemote, remoteDeleted bool) []Action {
	switch {
	case !remoteChanged && !remoteDeleted:
		return []Action{makeAction(ActionRemoteDelete, view)} // EF6
	case remoteChanged && hasRemote:
		return []Action{makeAction(ActionDownload, view)} // EF7: remote wins
	case remoteDeleted:
		return []Action{makeAction(ActionCleanup, view)} // EF10
	}

	return nil
}

// classifyFileLocalPresent handles EF2, EF3, EF4, EF5, EF8, EF9: the
// local file is still present (not deleted).
func classifyFileLocalPresent(
	view *PathView, localChanged, remoteChanged, hasRemote, remoteDeleted bool,
) []Action {
	switch {
	case !localChanged && remoteChanged && hasRemote:
		return []Action{makeAction(ActionDownload, view)} // EF2
	case localChanged && !remoteChanged:
		return []Action{makeAction(ActionUpload, view)} // EF3
	case localChanged && remoteChanged && hasRemote:
		if view.Local != nil && view.Local.Hash == view.Remote.Hash {
			return []Action{makeAction(ActionUpdateSynced, view)} // EF4: convergent edit
		}
		return []Action{makeConflictAction(view, ConflictEditEdit)} // EF5
	case !localChanged && remoteDeleted:
		return []Action{makeAction(ActionLocalDelete, view)} // EF8
	case localChanged && remoteDeleted:
		return []Action{makeConflictAction(view, ConflictEditDelete)} // EF9
	}

	return nil
}

// classifyFileNoBaseline handles EF11-EF14: files that have no baseline
// entry (never synced before).
func classifyFileNoBaseline(view *PathView, mode SyncMode) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil

	// Mode filtering for no-baseline files.
	if mode == SyncDownloadOnly {
		hasLocal = false
	}

	if mode == SyncUploadOnly {
		hasRemote = false
	}

	switch {
	case hasLocal && hasRemote:
		if view.Local.Hash == view.Remote.Hash {
			return []Action{makeAction(ActionUpdateSynced, view)} // EF11: convergent create
		}
		return []Action{makeConflictAction(view, ConflictCreateCreate)} // EF12

	case hasLocal && !hasRemote:
		return []Action{makeAction(ActionUpload, view)} // EF13

	case !hasLocal && hasRemote:
		return []Action{makeAction(ActionDownload, view)} // EF14
	}

	return nil
}

// classifyFolder handles ED1-ED8: folder decision matrix. Dispatches
// to sub-functions based on baseline presence to keep complexity down.
func classifyFolder(view *PathView, mode SyncMode) []Action {
	hasBaseline := view.Baseline != nil

	if hasBaseline {
		return classifyFolderWithBaseline(view, mode)
	}

	return classifyFolderNoBaseline(view, mode)
}

// classifyFolderWithBaseline handles ED1, ED4, ED6, ED7, ED8: folders
// that have a baseline entry (previously synced).
func classifyFolderWithBaseline(view *PathView, mode SyncMode) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted
	localDeleted := !hasLocal // baseline exists (we're in WithBaseline)

	// Upfront mode filtering — parallel to classifyFileWithBaseline.
	// Defense in depth: the engine already skips observers for suppressed
	// sides, but the planner should be self-contained.
	if mode == SyncDownloadOnly {
		localDeleted = false
	}

	if mode == SyncUploadOnly {
		hasRemote = false
		remoteDeleted = false
	}

	return classifyFolderWithFlags(view, localDeleted, hasRemote, remoteDeleted)
}

// classifyFolderWithFlags implements the ED1, ED4, ED6, ED7, ED8 decision
// matrix using pre-computed boolean flags.
func classifyFolderWithFlags(view *PathView, localDeleted, hasRemote, remoteDeleted bool) []Action {
	switch {
	case !localDeleted && hasRemote:
		return nil // ED1: in sync

	case localDeleted && hasRemote:
		return []Action{makeFolderCreate(view, CreateLocal)} // ED4: remote wins

	case !localDeleted && remoteDeleted:
		return []Action{makeAction(ActionLocalDelete, view)} // ED6

	case localDeleted && remoteDeleted:
		return []Action{makeAction(ActionCleanup, view)} // ED7: both deleted

	case localDeleted && !hasRemote && !remoteDeleted:
		return []Action{makeAction(ActionRemoteDelete, view)} // ED8: propagate delete
	}

	return nil
}

// classifyFolderNoBaseline handles ED2, ED3, ED5: folders that have
// no baseline entry (never synced before).
func classifyFolderNoBaseline(view *PathView, mode SyncMode) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted

	// Upfront mode filtering — parallel to classifyFileNoBaseline.
	if mode == SyncDownloadOnly {
		hasLocal = false
	}

	if mode == SyncUploadOnly {
		hasRemote = false
	}

	switch {
	case hasLocal && hasRemote:
		return []Action{makeAction(ActionUpdateSynced, view)} // ED2: adopt

	case !hasLocal && hasRemote:
		return []Action{makeFolderCreate(view, CreateLocal)} // ED3

	case hasLocal && !hasRemote && !remoteDeleted:
		return []Action{makeFolderCreate(view, CreateRemote)} // ED5
	}

	return nil
}

// ---------------------------------------------------------------------------
// Pure helper functions
// ---------------------------------------------------------------------------

// remoteStateFromEvent constructs a RemoteState from a ChangeEvent.
func remoteStateFromEvent(ev *ChangeEvent) *RemoteState {
	return &RemoteState{
		ItemID:    ev.ItemID,
		DriveID:   ev.DriveID,
		ParentID:  ev.ParentID,
		Name:      ev.Name,
		ItemType:  ev.ItemType,
		Size:      ev.Size,
		Hash:      ev.Hash,
		Mtime:     ev.Mtime,
		ETag:      ev.ETag,
		CTag:      ev.CTag,
		IsDeleted: ev.IsDeleted,
	}
}

// localStateFromEvent constructs a LocalState from a ChangeEvent.
// Returns nil if the event is a deletion (item is absent locally).
func localStateFromEvent(ev *ChangeEvent) *LocalState {
	if ev.Type == ChangeDelete {
		return nil
	}

	return &LocalState{
		Name:     ev.Name,
		ItemType: ev.ItemType,
		Size:     ev.Size,
		Hash:     ev.Hash,
		Mtime:    ev.Mtime,
	}
}

// localStateFromBaseline derives a LocalState from a baseline entry for
// paths with no local events (item is unchanged on disk).
func localStateFromBaseline(entry *BaselineEntry) *LocalState {
	return &LocalState{
		Name:     path.Base(entry.Path),
		ItemType: entry.ItemType,
		Size:     entry.Size,
		Hash:     entry.LocalHash,
		Mtime:    entry.Mtime,
	}
}

// detectLocalChange returns true if the local state differs from the
// baseline. A missing local state (deleted file) counts as changed.
func detectLocalChange(view *PathView) bool {
	if view.Baseline == nil {
		return view.Local != nil
	}

	// A nil local state means the file was deleted, which counts as a change.
	if view.Local == nil {
		return true
	}

	// Folders have no content hash; existence is the only signal.
	if view.Baseline.ItemType == ItemTypeFolder {
		return false
	}

	return view.Local.Hash != view.Baseline.LocalHash
}

// detectRemoteChange returns true if the remote state differs from the
// baseline. A nil Remote means no observation (not "unchanged").
func detectRemoteChange(view *PathView) bool {
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
	if view.Baseline.ItemType == ItemTypeFolder {
		return false
	}

	return view.Remote.Hash != view.Baseline.RemoteHash
}

// resolveItemType determines the item type by checking Remote, Local,
// then Baseline. Defaults to ItemTypeFile if none provide a type.
func resolveItemType(view *PathView) ItemType {
	if view == nil {
		return ItemTypeFile
	}

	if view.Remote != nil {
		return view.Remote.ItemType
	}

	if view.Local != nil {
		return view.Local.ItemType
	}

	if view.Baseline != nil {
		return view.Baseline.ItemType
	}

	return ItemTypeFile
}

// makeAction constructs an Action with type, path, and IDs populated from
// the PathView.
//
// DriveID propagation contract:
//   - Remote.DriveID is authoritative for cross-drive items (shared folders
//     from Drive A appearing in Drive B's delta carry Drive A's DriveID).
//   - Baseline.DriveID is the fallback for items with no remote observation.
//   - Empty DriveID for new local items (EF13, ED5) — the executor fills
//     this from its per-drive Engine context before making API calls.
//   - Empty ItemID for new items — assigned by the API on creation.
func makeAction(actionType ActionType, view *PathView) Action {
	a := Action{
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

	return a
}

// makeConflictAction constructs an ActionConflict with ConflictInfo populated.
func makeConflictAction(view *PathView, conflictType string) Action {
	a := makeAction(ActionConflict, view)

	record := &ConflictRecord{
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
func makeFolderCreate(view *PathView, side FolderCreateSide) Action {
	a := makeAction(ActionFolderCreate, view)
	a.CreateSide = side

	return a
}

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) move target parent must exist first.
func buildDependencies(actions []Action) [][]int {
	deps := make([][]int, len(actions))

	// Index folder creates by path for quick lookup.
	folderCreateIdx := make(map[string]int)
	// Index all deletes by path for child→parent edges.
	deleteIdx := make(map[string]int)

	for i := range actions {
		if actions[i].Type == ActionFolderCreate {
			folderCreateIdx[actions[i].Path] = i
		}

		if actions[i].Type == ActionLocalDelete || actions[i].Type == ActionRemoteDelete || actions[i].Type == ActionCleanup {
			deleteIdx[actions[i].Path] = i
		}
	}

	for i := range actions {
		deps[i] = addParentFolderDep(deps[i], i, &actions[i], folderCreateIdx)
		deps[i] = addChildDeleteDeps(deps[i], i, &actions[i], deleteIdx)
		deps[i] = addMoveTargetDep(deps[i], &actions[i], folderCreateIdx)
	}

	return deps
}

// addParentFolderDep adds a dependency on a parent folder create if present.
func addParentFolderDep(deps []int, idx int, a *Action, folderCreateIdx map[string]int) []int {
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
func addChildDeleteDeps(deps []int, idx int, a *Action, deleteIdx map[string]int) []int {
	if a.Type != ActionLocalDelete && a.Type != ActionRemoteDelete {
		return deps
	}

	if resolveItemType(a.View) != ItemTypeFolder {
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

// addMoveTargetDep adds a dependency on a folder create for the move target parent.
func addMoveTargetDep(deps []int, a *Action, folderCreateIdx map[string]int) []int {
	if a.Type != ActionLocalMove && a.Type != ActionRemoteMove {
		return deps
	}

	targetParent := filepath.Dir(a.NewPath)
	if targetParent == "." || targetParent == "" {
		return deps
	}

	targetParent = filepath.ToSlash(targetParent)

	if fcIdx, ok := folderCreateIdx[targetParent]; ok {
		deps = append(deps, fcIdx)
	}

	return deps
}

// countByType counts actions grouped by ActionType.
func countByType(actions []Action) map[ActionType]int {
	counts := make(map[ActionType]int)
	for i := range actions {
		counts[actions[i].Type]++
	}

	return counts
}

// ActionsOfType filters a flat action list to a single type.
func ActionsOfType(actions []Action, t ActionType) []Action {
	var result []Action

	for i := range actions {
		if actions[i].Type == t {
			result = append(result, actions[i])
		}
	}

	return result
}

// bigDeleteTriggered returns true if the planned deletions exceed the
// safety thresholds defined in the config.
func bigDeleteTriggered(deleteCount int, baseline *Baseline, config *SafetyConfig) bool {
	baselineCount := baseline.Len()

	// Below minimum items threshold — big-delete check does not apply.
	if baselineCount < config.BigDeleteMinItems {
		return false
	}

	if deleteCount > config.BigDeleteMaxCount {
		return true
	}

	percentage := float64(deleteCount) / float64(baselineCount) * percentMultiplier

	return percentage > config.BigDeleteMaxPercent
}
