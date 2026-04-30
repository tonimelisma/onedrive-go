package sync

import (
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Planner is a pure decision engine that transforms SQLite-owned comparison
// and reconciliation rows plus baseline state into an ordered ActionPlan. It
// performs no I/O.
type Planner struct {
	logger *slog.Logger
}

type plannerMountContext struct {
	DriveID driveid.ID
}

// NewPlanner creates a Planner with the given logger.
func NewPlanner(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
}

func filterActionsForMode(actions []Action, mode SyncMode) []Action {
	if len(actions) == 0 || mode == SyncBidirectional {
		return actions
	}

	filtered := actions[:0]
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			filtered = append(filtered, actions[i])
		}
	}

	return filtered
}

func actionAllowedInMode(action *Action, mode SyncMode) bool {
	switch action.Type {
	case ActionDownload:
		return mode != SyncUploadOnly
	case ActionUpload:
		return mode != SyncDownloadOnly
	case ActionLocalDelete:
		return mode != SyncUploadOnly
	case ActionRemoteDelete:
		return mode != SyncDownloadOnly
	case ActionLocalMove:
		return mode != SyncUploadOnly
	case ActionRemoteMove:
		return mode != SyncDownloadOnly
	case ActionFolderCreate:
		if action.CreateSide == CreateLocal {
			return mode != SyncUploadOnly
		}
		if action.CreateSide == CreateRemote {
			return mode != SyncDownloadOnly
		}
		return true
	case ActionConflictCopy:
		// Conflict-copy is only safe when the paired remote-winner download
		// can also run. In upload-only mode that download is deferred, so keeping
		// the rename would mutate local truth into a fake delete/create sequence.
		return mode != SyncUploadOnly
	case ActionUpdateSynced, ActionCleanup:
		return true
	default:
		return true
	}
}

// classifyFile dispatches to the appropriate file classification function
// based on whether a baseline entry exists.
func classifyFile(view *PathView) []Action {
	if view.Baseline != nil {
		return classifyFileWithBaseline(view)
	}

	return classifyFileNoBaseline(view)
}

// classifyFileWithBaseline handles EF1-EF10: files that have a baseline
// entry (previously synced).
func classifyFileWithBaseline(view *PathView) []Action {
	localChanged := detectLocalChange(view)
	remoteChanged := detectRemoteChange(view)

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
		return []Action{MakeAction(ActionRemoteDelete, view)} // EF6
	case remoteChanged && hasRemote:
		return []Action{MakeAction(ActionDownload, view)} // EF7: remote wins
	case remoteDeleted:
		return []Action{MakeAction(ActionCleanup, view)} // EF10
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
		return []Action{MakeAction(ActionDownload, view)} // EF2
	case localChanged && !remoteChanged:
		return []Action{MakeAction(ActionUpload, view)} // EF3
	case localChanged && remoteChanged && hasRemote:
		if view.Local != nil && view.Local.Hash == view.Remote.Hash {
			return []Action{MakeAction(ActionUpdateSynced, view)} // EF4: convergent edit
		}
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		} // EF5
	case !localChanged && remoteDeleted:
		return []Action{MakeAction(ActionLocalDelete, view)} // EF8
	case localChanged && remoteDeleted:
		return []Action{makeCreateUploadAction(view)} // EF9
	}

	return nil
}

// classifyFileNoBaseline handles EF11-EF14: files that have no baseline
// entry (never synced before).
func classifyFileNoBaseline(view *PathView) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil

	switch {
	case hasLocal && hasRemote:
		if view.Local.Hash == view.Remote.Hash {
			return []Action{MakeAction(ActionUpdateSynced, view)} // EF11: convergent create
		}
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		} // EF12

	case hasLocal && !hasRemote:
		return []Action{MakeAction(ActionUpload, view)} // EF13

	case !hasLocal && hasRemote:
		return []Action{MakeAction(ActionDownload, view)} // EF14
	}

	return nil
}

// classifyFolder handles ED1-ED8: folder decision matrix. Dispatches
// to sub-functions based on baseline presence to keep complexity down.
func classifyFolder(view *PathView) []Action {
	hasBaseline := view.Baseline != nil

	if hasBaseline {
		return classifyFolderWithBaseline(view)
	}

	return classifyFolderNoBaseline(view)
}

// classifyFolderWithBaseline handles ED1, ED4, ED6, ED7, ED8: folders
// that have a baseline entry (previously synced).
func classifyFolderWithBaseline(view *PathView) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted
	localDeleted := !hasLocal // baseline exists (we're in WithBaseline)

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
		return []Action{MakeAction(ActionLocalDelete, view)} // ED6

	case localDeleted && remoteDeleted:
		return []Action{MakeAction(ActionCleanup, view)} // ED7: both deleted

	case localDeleted && !hasRemote && !remoteDeleted:
		return []Action{MakeAction(ActionRemoteDelete, view)} // ED8: propagate delete
	}

	return nil
}

// classifyFolderNoBaseline handles ED2, ED3, ED5: folders that have
// no baseline entry (never synced before).
func classifyFolderNoBaseline(view *PathView) []Action {
	hasRemote := view.Remote != nil && !view.Remote.IsDeleted
	hasLocal := view.Local != nil
	remoteDeleted := view.Remote != nil && view.Remote.IsDeleted

	switch {
	case hasLocal && hasRemote:
		return []Action{MakeAction(ActionUpdateSynced, view)} // ED2: adopt

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

// localStateFromBaseline derives a LocalState from a baseline entry for
// paths with no local events (item is unchanged on disk).
func localStateFromBaseline(entry *BaselineEntry) *LocalState {
	return &LocalState{
		Name:             path.Base(entry.Path),
		ItemType:         entry.ItemType,
		Size:             entry.LocalSize,
		Hash:             entry.LocalHash,
		Mtime:            entry.LocalMtime,
		LocalDevice:      entry.LocalDevice,
		LocalInode:       entry.LocalInode,
		LocalHasIdentity: entry.LocalHasIdentity,
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

	if localIdentityMismatch(view.Local, view.Baseline) {
		return true
	}

	// Folders have no content hash; existence is the only signal.
	if view.Baseline.ItemType == ItemTypeFolder {
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

func localIdentityMismatch(local *LocalState, baseline *BaselineEntry) bool {
	if local == nil || baseline == nil {
		return false
	}
	if !local.LocalHasIdentity || !baseline.LocalHasIdentity {
		return false
	}

	return local.LocalDevice != baseline.LocalDevice || local.LocalInode != baseline.LocalInode
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
func resolveItemType(view *PathView) ItemType {
	if view == nil {
		return ItemTypeFile
	}

	if view.Remote != nil {
		// When the remote item is deleted, the delta API may omit the folder
		// facet, causing ItemType to default to ItemTypeFile. If a baseline
		// exists with a non-file type, prefer it — the baseline recorded the
		// correct type when the item was still alive.
		if view.Remote.IsDeleted &&
			view.Remote.ItemType == ItemTypeFile &&
			view.Baseline != nil &&
			view.Baseline.ItemType != ItemTypeFile {
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

	return ItemTypeFile
}

// MakeAction constructs an Action with type, path, and IDs populated from
// the PathView.
//
// DriveID propagation contract:
//   - Remote.DriveID is authoritative for cross-drive items (shared folders
//     from Drive A appearing in Drive B's delta carry Drive A's DriveID).
//   - Baseline.DriveID is the fallback for items with no remote observation.
//   - Empty DriveID for new local items (EF13, ED5) — planner fills this from
//     the mounted engine drive before execution.
//   - Empty ItemID for new items — assigned by the API on creation.
func MakeAction(actionType ActionType, view *PathView) Action {
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

func bindMountContext(actions []Action, mount plannerMountContext) {
	for i := range actions {
		if actions[i].DriveID.IsZero() {
			actions[i].DriveID = mount.DriveID
		}
	}
}

func makeConflictCopyAction(view *PathView) Action {
	return MakeAction(ActionConflictCopy, view)
}

func makeDownloadAfterConflictCopyAction(view *PathView) Action {
	action := MakeAction(ActionDownload, view)
	action.RequireMissingLocalTarget = true
	return action
}

func makeCreateUploadAction(view *PathView) Action {
	action := MakeAction(ActionUpload, view)
	action.ItemID = ""
	return action
}

// makeFolderCreate constructs an ActionFolderCreate action with the
// specified creation side (local or remote).
func makeFolderCreate(view *PathView, side FolderCreateSide) Action {
	a := MakeAction(ActionFolderCreate, view)
	a.CreateSide = side

	return a
}

// expandFolderDeleteCascades expands admitted parent-folder delete actions into
// baseline descendants that observation omitted. The omitted side depends on
// the admitted parent action:
//   - ActionLocalDelete: remote folder delete omitted remote descendant deletes
//   - ActionRemoteDelete: local folder delete omitted local descendant deletes
//   - ActionCleanup: both sides deleted the folder, so descendants must clean up
//
// Existing child actions are reclassified through the same file/folder
// decision matrix with a synthetic deleted side so descendant-level
// download/upload/conflict semantics survive parent-folder collapse. When any
// descendant still needs the deleted parent to exist, the parent delete is
// rewritten into a folder create on the required side so child work has a
// target.
//
// Logic:
//  1. Index current actions by path.
//  2. For each folder delete/cleanup action, walk baseline.DescendantsOf.
//  3. Rebuild each descendant view with Remote.IsDeleted=true and run it back
//     through the normal file/folder classifiers.
//  4. Replace an existing descendant action or append a missing one.
//  5. If any descendant action preserves local content, rewrite the parent
//     folder delete into a remote folder create so child work has a target.
func expandFolderDeleteCascades(
	actions []Action,
	baseline *Baseline,
	views map[string]*PathView,
	mode SyncMode,
	logger *slog.Logger,
) []Action {
	// Track the current action index for each path. Initial classification
	// emits at most one action per path; cascade may replace that action when
	// the omitted remote delete changes the descendant semantics.
	existingActionIndex := make(map[string]actionLocation, len(actions))
	for i := range actions {
		existingActionIndex[actions[i].Path] = actionLocation{Index: i}
	}

	var cascaded []Action

	for i := range actions {
		a := &actions[i]

		if !shouldCascadeFolderDelete(a) {
			continue
		}
		cascadeKind, ok := cascadeDeleteKindForAction(a.Type)
		if !ok {
			continue
		}

		descendants := baseline.DescendantsOf(a.Path)
		preserveRemoteDescendant := false
		if len(descendants) > 0 {
			logger.Debug("cascading folder delete to descendants",
				slog.String("folder", a.Path),
				slog.Int("descendant_count", len(descendants)),
			)

			preserveRemoteDescendant = applyFolderDeleteCascade(
				actions,
				existingActionIndex,
				descendants,
				views,
				mode,
				cascadeKind,
				&cascaded,
			)
		}
		if !preserveRemoteDescendant && subtreeActionRequiresParent(actions, cascaded, a.Path) {
			preserveRemoteDescendant = true
		}

		if !preserveRemoteDescendant {
			continue
		}

		switch a.Type {
		case ActionLocalDelete:
			actions[i] = makeFolderCreate(a.View, CreateRemote)
		case ActionRemoteDelete:
			actions[i] = makeFolderCreate(a.View, CreateLocal)
		case ActionCleanup:
		case ActionDownload,
			ActionUpload,
			ActionLocalMove,
			ActionRemoteMove,
			ActionFolderCreate,
			ActionConflictCopy,
			ActionUpdateSynced:
			panic(fmt.Sprintf("unexpected folder cascade action type %s", a.Type.String()))
		}
	}

	if len(cascaded) > 0 {
		logger.Info("folder delete cascade expanded",
			slog.Int("cascaded_actions", len(cascaded)),
		)
	}

	return append(actions, cascaded...)
}

func shouldCascadeFolderDelete(action *Action) bool {
	if action == nil {
		return false
	}

	isDelete := action.Type == ActionLocalDelete ||
		action.Type == ActionRemoteDelete ||
		action.Type == ActionCleanup
	return isDelete && resolveItemType(action.View) == ItemTypeFolder
}

type actionLocation struct {
	InCascaded bool
	Index      int
}

type cascadeDeleteKind uint8

const (
	cascadeRemoteDeleted cascadeDeleteKind = iota
	cascadeLocalDeleted
	cascadeBothDeleted
)

func cascadeDeleteKindForAction(actionType ActionType) (cascadeDeleteKind, bool) {
	switch actionType {
	case ActionLocalDelete:
		return cascadeRemoteDeleted, true
	case ActionRemoteDelete:
		return cascadeLocalDeleted, true
	case ActionCleanup:
		return cascadeBothDeleted, true
	case ActionDownload,
		ActionUpload,
		ActionLocalMove,
		ActionRemoteMove,
		ActionFolderCreate,
		ActionConflictCopy,
		ActionUpdateSynced:
		return 0, false
	}

	panic(fmt.Sprintf("unknown action type %d", actionType))
}

func applyFolderDeleteCascade(
	actions []Action,
	existingActionIndex map[string]actionLocation,
	descendants []*BaselineEntry,
	views map[string]*PathView,
	mode SyncMode,
	cascadeKind cascadeDeleteKind,
	cascaded *[]Action,
) bool {
	preserveRemoteDescendant := false

	for _, desc := range descendants {
		descActions := classifyCascadedDescendant(
			buildCascadedDescendantView(desc, views[desc.Path], cascadeKind),
			mode,
		)
		if len(descActions) == 0 {
			continue
		}

		descAction := descActions[0]
		if actionRequiresParentFolder(descAction.Type) {
			preserveRemoteDescendant = true
		}

		if existingLocation, exists := existingActionIndex[desc.Path]; exists {
			if existingLocation.InCascaded {
				(*cascaded)[existingLocation.Index] = descAction
			} else {
				actions[existingLocation.Index] = descAction
			}
			continue
		}

		*cascaded = append(*cascaded, descAction)
		existingActionIndex[desc.Path] = actionLocation{
			InCascaded: true,
			Index:      len(*cascaded) - 1,
		}
	}

	return preserveRemoteDescendant
}

func subtreeActionRequiresParent(actions []Action, cascaded []Action, parentPath string) bool {
	prefix := parentPath + "/"

	for i := range actions {
		if strings.HasPrefix(actions[i].Path, prefix) && actionRequiresParentFolder(actions[i].Type) {
			return true
		}
	}

	for i := range cascaded {
		if strings.HasPrefix(cascaded[i].Path, prefix) && actionRequiresParentFolder(cascaded[i].Type) {
			return true
		}
	}

	return false
}

func buildCascadedDescendantView(
	desc *BaselineEntry,
	existingView *PathView,
	cascadeKind cascadeDeleteKind,
) *PathView {
	descView := &PathView{
		Path:     desc.Path,
		Baseline: desc,
	}

	switch cascadeKind {
	case cascadeRemoteDeleted:
		descView.Remote = &RemoteState{
			ItemID:    desc.ItemID,
			DriveID:   desc.DriveID,
			ItemType:  desc.ItemType,
			IsDeleted: true,
		}
		if existingView != nil && existingView.Local != nil {
			descView.Local = existingView.Local
		} else {
			descView.Local = localStateFromBaseline(desc)
		}
	case cascadeLocalDeleted:
		if existingView != nil && existingView.Remote != nil {
			descView.Remote = existingView.Remote
		}
	case cascadeBothDeleted:
		descView.Remote = &RemoteState{
			ItemID:    desc.ItemID,
			DriveID:   desc.DriveID,
			ItemType:  desc.ItemType,
			IsDeleted: true,
		}
	default:
		panic("unknown cascade delete kind")
	}

	return descView
}

func classifyCascadedDescendant(view *PathView, mode SyncMode) []Action {
	if view == nil {
		return nil
	}

	if resolveItemType(view) == ItemTypeFolder {
		return filterActionsForMode(classifyFolder(view), mode)
	}

	return filterActionsForMode(classifyFile(view), mode)
}

func actionRequiresParentFolder(actionType ActionType) bool {
	return actionType != ActionLocalDelete &&
		actionType != ActionCleanup &&
		actionType != ActionRemoteDelete
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
	conflictCopyIdx := make(map[string]int)
	// Index all deletes by path for child→parent edges.
	deleteIdx := make(map[string]int)

	for i := range actions {
		if actions[i].Type == ActionFolderCreate {
			folderCreateIdx[actions[i].Path] = i
		}
		if actions[i].Type == ActionConflictCopy {
			conflictCopyIdx[actions[i].Path] = i
		}

		isDelete := actions[i].Type == ActionLocalDelete ||
			actions[i].Type == ActionRemoteDelete ||
			actions[i].Type == ActionCleanup
		if isDelete {
			deleteIdx[actions[i].Path] = i
		}
	}

	for i := range actions {
		deps[i] = addParentFolderDep(deps[i], i, &actions[i], folderCreateIdx)
		deps[i] = addConflictCopyDep(deps[i], i, &actions[i], conflictCopyIdx)
		deps[i] = addChildDeleteDeps(deps[i], i, &actions[i], deleteIdx)
		deps[i] = addRemoteMoveDeps(deps[i], i, &actions[i], actions)
		// Sort dependency indices for reproducible ordering (B-154).
		sort.Ints(deps[i])
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

func addConflictCopyDep(deps []int, idx int, a *Action, conflictCopyIdx map[string]int) []int {
	if a.Type != ActionDownload {
		return deps
	}

	if copyIdx, ok := conflictCopyIdx[a.Path]; ok && copyIdx != idx {
		deps = append(deps, copyIdx)
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

func addRemoteMoveDeps(deps []int, idx int, a *Action, actions []Action) []int {
	if a.Type == ActionRemoteMove || a.Type == ActionLocalDelete || a.Type == ActionRemoteDelete || a.Type == ActionCleanup {
		return deps
	}

	for moveIdx := range actions {
		move := &actions[moveIdx]
		if moveIdx == idx || move.Type != ActionRemoteMove {
			continue
		}
		if move.Path == a.Path {
			deps = append(deps, moveIdx)
			continue
		}
		if resolveItemType(move.View) == ItemTypeFolder && strings.HasPrefix(a.Path, move.Path+"/") {
			deps = append(deps, moveIdx)
		}
	}

	return deps
}

// CountByType counts actions grouped by ActionType. Exported for use by the
// sync engine when building pass reports from plan counts.
func CountByType(actions []Action) map[ActionType]int {
	counts := make(map[ActionType]int)
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
				return ErrDependencyCycle
			}
		}
	}

	return nil
}
