package sync

import (
	"errors"
	"log/slog"
	"path"
	"sort"
	"strings"
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
		slog.Int("baseline_entries", len(baseline.ByPath)),
		slog.String("mode", mode.String()),
	)

	views := buildPathViews(changes, baseline)
	plan := &ActionPlan{}

	// Step 1: detect and extract moves before per-path classification.
	moveActions := detectMoves(views, changes)
	appendActions(plan, moveActions)

	// Step 2: classify each remaining path.
	for _, view := range views {
		actions := classifyPathView(view, mode)
		appendActions(plan, actions)
	}

	// Step 3: order the plan (folder creates top-down, deletes bottom-up).
	orderPlan(plan)

	// Step 4: safety check for big deletes.
	if bigDeleteTriggered(plan, baseline, config) {
		deleteCount := len(plan.LocalDeletes) + len(plan.RemoteDeletes)
		p.logger.Warn("big-delete protection triggered",
			slog.Int("delete_count", deleteCount),
			slog.Int("baseline_count", len(baseline.ByPath)),
			slog.Int("max_count", config.BigDeleteMaxCount),
			slog.Float64("max_percent", config.BigDeleteMaxPercent),
		)

		return nil, ErrBigDeleteTriggered
	}

	p.logger.Info("plan complete",
		slog.Int("folder_creates", len(plan.FolderCreates)),
		slog.Int("moves", len(plan.Moves)),
		slog.Int("downloads", len(plan.Downloads)),
		slog.Int("uploads", len(plan.Uploads)),
		slog.Int("local_deletes", len(plan.LocalDeletes)),
		slog.Int("remote_deletes", len(plan.RemoteDeletes)),
		slog.Int("conflicts", len(plan.Conflicts)),
		slog.Int("synced_updates", len(plan.SyncedUpdates)),
		slog.Int("cleanups", len(plan.Cleanups)),
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
		view.Baseline = baseline.ByPath[pc.Path]

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

			// Remove both old and new paths from classification.
			delete(views, ev.Path)
			delete(views, ev.OldPath)
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
		return []Action{makeConflictAction(view, "edit_edit")} // EF5
	case !localChanged && remoteDeleted:
		return []Action{makeAction(ActionLocalDelete, view)} // EF8
	case localChanged && remoteDeleted:
		return []Action{makeConflictAction(view, "edit_delete")} // EF9
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
		return []Action{makeConflictAction(view, "create_create")} // EF12

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

	switch {
	case hasLocal && hasRemote:
		return nil // ED1: in sync

	case !hasLocal && hasRemote:
		if mode == SyncUploadOnly {
			return nil
		}
		return []Action{makeFolderCreate(view, CreateLocal)} // ED4: recreate

	case hasLocal && remoteDeleted:
		if mode == SyncUploadOnly {
			return nil
		}
		return []Action{makeAction(ActionLocalDelete, view)} // ED6

	case !hasLocal && remoteDeleted:
		return []Action{makeAction(ActionCleanup, view)} // ED7

	case !hasLocal && !hasRemote:
		return []Action{makeAction(ActionCleanup, view)} // ED8
	}

	return nil
}

// classifyFolderNoBaseline handles ED2, ED3, ED5: folders that have
// no baseline entry (never synced before).
func classifyFolderNoBaseline(view *PathView, mode SyncMode) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted

	switch {
	case hasLocal && hasRemote:
		return []Action{makeAction(ActionUpdateSynced, view)} // ED2: adopt

	case !hasLocal && hasRemote:
		if mode == SyncUploadOnly {
			return nil
		}
		return []Action{makeFolderCreate(view, CreateLocal)} // ED3

	case hasLocal && !hasRemote && !remoteDeleted:
		if mode == SyncDownloadOnly {
			return nil
		}
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
// the PathView. Uses Remote for ItemID (since RemoteState has it), and
// Baseline for DriveID (since RemoteState does not carry DriveID).
func makeAction(actionType ActionType, view *PathView) Action {
	a := Action{
		Type: actionType,
		Path: view.Path,
		View: view,
	}

	// Remote provides ItemID; DriveID is not available on RemoteState.
	if view.Remote != nil {
		a.ItemID = view.Remote.ItemID
	}

	// Baseline provides DriveID and a fallback ItemID.
	if view.Baseline != nil {
		a.DriveID = view.Baseline.DriveID

		if a.ItemID == "" {
			a.ItemID = view.Baseline.ItemID
		}
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

// appendActions routes each action to the correct slice in the ActionPlan.
func appendActions(plan *ActionPlan, actions []Action) {
	for i := range actions {
		a := &actions[i]

		switch a.Type {
		case ActionFolderCreate:
			plan.FolderCreates = append(plan.FolderCreates, *a)
		case ActionDownload:
			plan.Downloads = append(plan.Downloads, *a)
		case ActionUpload:
			plan.Uploads = append(plan.Uploads, *a)
		case ActionLocalDelete:
			plan.LocalDeletes = append(plan.LocalDeletes, *a)
		case ActionRemoteDelete:
			plan.RemoteDeletes = append(plan.RemoteDeletes, *a)
		case ActionLocalMove, ActionRemoteMove:
			plan.Moves = append(plan.Moves, *a)
		case ActionConflict:
			plan.Conflicts = append(plan.Conflicts, *a)
		case ActionUpdateSynced:
			plan.SyncedUpdates = append(plan.SyncedUpdates, *a)
		case ActionCleanup:
			plan.Cleanups = append(plan.Cleanups, *a)
		}
	}
}

// orderPlan sorts action slices for correct execution order: folder
// creates shallowest-first (top-down), deletes deepest-first (bottom-up).
func orderPlan(plan *ActionPlan) {
	// Folder creates: shallowest first so parent dirs exist before children.
	sort.SliceStable(plan.FolderCreates, func(i, j int) bool {
		return pathDepth(plan.FolderCreates[i].Path) < pathDepth(plan.FolderCreates[j].Path)
	})

	// Local deletes: deepest first so children are removed before parents.
	sort.SliceStable(plan.LocalDeletes, func(i, j int) bool {
		return pathDepth(plan.LocalDeletes[i].Path) > pathDepth(plan.LocalDeletes[j].Path)
	})

	// Remote deletes: same depth-first ordering as local deletes.
	sort.SliceStable(plan.RemoteDeletes, func(i, j int) bool {
		return pathDepth(plan.RemoteDeletes[i].Path) > pathDepth(plan.RemoteDeletes[j].Path)
	})
}

// bigDeleteTriggered returns true if the planned deletions exceed the
// safety thresholds defined in the config.
func bigDeleteTriggered(plan *ActionPlan, baseline *Baseline, config *SafetyConfig) bool {
	deleteCount := len(plan.LocalDeletes) + len(plan.RemoteDeletes)
	baselineCount := len(baseline.ByPath)

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

// pathDepth counts the number of "/" separators in a path.
// Empty string and root-level names return 0.
func pathDepth(p string) int {
	if p == "" {
		return 0
	}

	return strings.Count(p, "/")
}
