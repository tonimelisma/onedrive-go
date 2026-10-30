package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
)

// errFolderTransferPlanned reports a reconciliation row that maps a folder to
// a content transfer. Folders are created, moved or deleted, never uploaded
// or downloaded (D-057); a row that says otherwise is a comparison bug.
var errFolderTransferPlanned = errors.New("sync: folder planned as a content transfer")

// PlanCurrentState builds the current actionable set from SQLite-owned
// comparison and reconciliation rows plus the current durable snapshots. SQLite
// owns structural diff authority; Go maps those rows to runtime actions and
// applies action-level normalization for execution semantics.
func (p *planner) PlanCurrentState(
	comparisons []sQLiteComparisonRow,
	reconciliations []sQLiteReconciliationRow,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	observationIssues []ObservationIssueRow,
	baseline *Baseline,
	mount plannerMountContext,
	mode SyncMode,
) (*actionPlan, error) {
	p.logger.Debug("planning current actionable set from sqlite reconciliation",
		slog.Int("comparison_rows", len(comparisons)),
		slog.Int("reconciliation_rows", len(reconciliations)),
		slog.Int("baseline_entries", baseline.Len()),
		slog.Int("observation_issues", len(observationIssues)),
		slog.String("mode", mode.String()),
	)

	truthPaths := comparisonPaths(comparisons)
	truthIndex := newTruthAvailabilityIndex(observationIssues)
	truthStatusByPath := truthIndex.StatusByPath(truthPaths)

	views, comparisonByPath, err := buildSQLitePathViews(comparisons, localRows, remoteRows, baseline, truthStatusByPath)
	if err != nil {
		return nil, err
	}

	moveInputs := moveCompositionInputs(comparisons, views, remoteRows, localRows)
	composition := composeMoves(moveInputs.nodes, moveInputs.occupancy, moveInputs.frozen)
	logMoveNotices(p.logger, composition.notices)

	var allActions []Action
	for i := range reconciliations {
		rec := reconciliations[i]
		cmp, ok := comparisonByPath[rec.Path]
		if !ok {
			return nil, fmt.Errorf("sync: missing comparison row for reconciliation path %q", rec.Path)
		}

		actions, err := buildActionsForReconciliation(&rec, cmp, views)
		if errors.Is(err, errFolderTransferPlanned) {
			// A comparison bug, not a user state: transferring a directory
			// as file content can only fail and be replanned forever. The
			// rest of the plan stays runnable.
			p.logger.Error("planner dropped an impossible folder transfer",
				slog.String("path", rec.Path),
				slog.String("reconciliation", rec.ReconciliationKind),
				slog.String("error", err.Error()),
			)
			continue
		}
		if err != nil {
			return nil, err
		}

		allActions = append(allActions, actions...)
	}
	allActions = dropReconciliationOfComposedMoves(allActions, &composition)
	allActions = append(allActions, buildComposedMoveActions(&composition, &moveInputs, views, baseline)...)

	normalizedActions, trimmed := normalizeCurrentPlanActions(allActions, remoteToLocalFolderPrefixes(moveInputs.nodes), mode)
	beforeDrop := len(normalizedActions)
	normalizedActions = dropCreatesOntoComposedItems(normalizedActions, &moveInputs, &composition)
	droppedCreates := beforeDrop - len(normalizedActions)
	admitted, deferred := partitionCurrentActionsForMode(normalizedActions, mode)
	bindMountContext(admitted, mount)

	beforeCycles := len(admitted)
	admitted, deps := isolateDependencyCycles(admitted, p.logger)

	plan := &actionPlan{
		Actions:        admitted,
		Deps:           deps,
		DeferredByMode: deferred,
		FollowUp: planFollowUp{
			Renames:  countRenames(admitted),
			Deferred: len(composition.deferred),
			Dropped:  trimmed + droppedCreates + beforeCycles - len(admitted),
		},
	}

	logActionPlanSummary(p.logger, "sqlite actionable-set plan complete", plan)

	return plan, nil
}

// normalizeCurrentPlanActions keeps SQL as structural authority, then applies
// action-only execution semantics that cannot be represented by a single path's
// reconciliation row.
//
// Parent preservation runs first: it matches descendants by the paths SQLite
// reconciled them at, which are consistent with each other; the rebases then
// move whole subtrees, preserved parents included, to where they execute.
//
// It also reports how many actions the keep-both trim left for the next pass.
func normalizeCurrentPlanActions(actions []Action, remoteToLocal []graphFolderMoveBoundary, mode SyncMode) ([]Action, int) {
	before := len(actions)
	actions = trimKeepBothDescendants(actions)
	trimmed := before - len(actions)
	actions = preserveParentFoldersForRunnableDescendants(actions, mode)
	actions = rebaseLocalWritesToLocalFolderPositions(actions, remoteToLocal)
	return rebaseRemoteCreatesThroughLocalFolderMoves(actions), trimmed
}

// countRenames counts the actions that put an item at a new path (moves,
// baseline re-keys, conflict copies), whose follow-up the next pass plans.
func countRenames(actions []Action) int {
	n := 0
	for i := range actions {
		switch {
		case isMoveAction(&actions[i]), actions[i].Type == ActionConflictCopy:
			n++
		case actions[i].Type == ActionBaselineUpdate && actions[i].OldPath != "":
			n++
		}
	}

	return n
}

// rebaseRemoteCreatesThroughLocalFolderMoves moves new local content under a
// folder this plan renames locally (OneDrive renamed it) to the folder's new
// path. The upload or remote folder create resolves its OneDrive parent from
// the baseline, which knows the folder only at its new path once the local
// move commits; addressed at the old local path it failed on every pass and
// held the folder's move behind it. It now runs after the move. Its view keeps
// the snapshot path, which is where freshness finds its local truth.
func rebaseRemoteCreatesThroughLocalFolderMoves(actions []Action) []Action {
	var folderMoves []graphFolderMoveBoundary
	for i := range actions {
		action := &actions[i]
		if action.Type != ActionLocalMove || resolveItemType(action.View) != ItemTypeFolder || action.OldPath == "" {
			continue
		}
		folderMoves = append(folderMoves, graphFolderMoveBoundary{
			oldPrefix: action.OldPath + "/",
			newPrefix: action.Path + "/",
		})
	}
	if len(folderMoves) == 0 {
		return actions
	}
	sort.Slice(folderMoves, func(i, j int) bool {
		return len(folderMoves[i].oldPrefix) > len(folderMoves[j].oldPrefix)
	})

	rebased := make([]Action, len(actions))
	copy(rebased, actions)
	for i := range rebased {
		action := &rebased[i]
		isRemoteCreate := action.Type == ActionUpload ||
			(action.Type == ActionFolderCreate && action.CreateSide == CreateRemote)
		if isRemoteCreate {
			action.Path = rebaseThroughFolderMoves(action.Path, folderMoves)
		}
	}

	return rebased
}

// rebaseLocalWritesToLocalFolderPositions makes remote-origin local writes
// address the local tree as it is. They carry remote paths, but a folder that
// sits elsewhere locally (renamed by the user, or renamed online and not yet
// moved here) does not exist at its remote path: OneDrive creating photos/x
// inside docs while docs was renamed locally to renamed-docs must land at
// renamed-docs/x. Applied at the remote path, the write re-created docs/
// locally, and the next pass uploaded the stray folder. Moves are already
// composed (planner_move_composition.go), and a local move waits for the
// writes under its source, so the rebased writes run before it. Remote-side
// writes need no rebasing: they address parents by item ID.
func rebaseLocalWritesToLocalFolderPositions(actions []Action, remoteToLocal []graphFolderMoveBoundary) []Action {
	if len(remoteToLocal) == 0 {
		return actions
	}

	rebased := make([]Action, len(actions))
	copy(rebased, actions)
	for i := range rebased {
		if rebased[i].Type == ActionLocalMove || !writesLocally(&rebased[i]) {
			continue
		}
		rebased[i].Path = rebaseThroughFolderMoves(rebased[i].Path, remoteToLocal)
	}

	return rebased
}

// writesLocally reports remote-origin actions that apply OneDrive's state to
// the local tree, addressed by path.
func writesLocally(action *Action) bool {
	switch action.Type {
	case ActionLocalMove, ActionDownload, ActionLocalDelete, ActionConflictCopy:
		return true
	case ActionFolderCreate:
		return action.CreateSide == createLocal
	case ActionUpload, ActionRemoteMove, ActionRemoteDelete, ActionBaselineUpdate, ActionCleanup:
		return false
	default:
		return false
	}
}

func rebaseThroughFolderMoves(p string, folderMoves []graphFolderMoveBoundary) string {
	if p == "" {
		return p
	}
	for _, move := range folderMoves {
		if strings.HasPrefix(p, move.oldPrefix) {
			return move.newPrefix + strings.TrimPrefix(p, move.oldPrefix)
		}
	}

	return p
}

// graphFolderMoveBoundary maps a folder's subtree from one path prefix to
// another.
type graphFolderMoveBoundary struct {
	oldPrefix string
	newPrefix string
}

// logMoveNotices reports composition decisions a user may want to know about.
func logMoveNotices(logger *slog.Logger, notices []moveNotice) {
	if logger == nil {
		return
	}
	for i := range notices {
		n := &notices[i]
		attrs := []any{
			slog.String("path", n.baselinePath),
			slog.String("local_path", n.localPath),
			slog.String("remote_path", n.remotePath),
		}
		switch n.kind {
		case moveNoticeDivergentRename:
			logger.Info("Renamed differently on both sides; keeping the OneDrive name and moving the local copy", attrs...)
		case moveNoticeCycle:
			logger.Info("Moves on both sides would nest the folders inside each other; keeping the OneDrive move", attrs...)
		case moveNoticeCollision:
			logger.Info("A local move collides with another item's place; keeping the OneDrive location", attrs...)
		case moveNoticeDeferred:
			logger.Debug("move waits for the next pass", append(attrs, slog.String("target_path", n.targetPath))...)
		}
	}
}

func comparisonPaths(rows []sQLiteComparisonRow) []string {
	if len(rows) == 0 {
		return nil
	}

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}

func logActionPlanSummary(logger *slog.Logger, message string, plan *actionPlan) {
	if logger == nil || plan == nil {
		return
	}

	counts := countByType(plan.Actions)
	logger.Info(message,
		slog.Int("total_actions", len(plan.Actions)),
		slog.Int("folder_creates", counts[ActionFolderCreate]),
		slog.Int("moves", counts[ActionLocalMove]+counts[ActionRemoteMove]),
		slog.Int("downloads", counts[ActionDownload]),
		slog.Int("uploads", counts[ActionUpload]),
		slog.Int("local_deletes", counts[ActionLocalDelete]),
		slog.Int("remote_deletes", counts[ActionRemoteDelete]),
		slog.Int("conflict_copies", counts[ActionConflictCopy]),
		slog.Int("baseline_updates", counts[ActionBaselineUpdate]),
		slog.Int("cleanups", counts[ActionCleanup]),
		slog.Int("deferred_folder_creates", plan.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", plan.DeferredByMode.Moves),
		slog.Int("deferred_downloads", plan.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", plan.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", plan.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", plan.DeferredByMode.RemoteDeletes),
	)
}

func buildSQLitePathViews(
	comparisons []sQLiteComparisonRow,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	baseline *Baseline,
	truthStatusByPath map[string]pathTruthStatus,
) (map[string]*pathView, map[string]*sQLiteComparisonRow, error) {
	localByPath := make(map[string]localStateRow, len(localRows))
	for i := range localRows {
		localByPath[localRows[i].Path] = localRows[i]
	}

	remoteByPath := make(map[string]remoteStateRow, len(remoteRows))
	for i := range remoteRows {
		remoteByPath[remoteRows[i].Path] = remoteRows[i]
	}

	views := make(map[string]*pathView, len(comparisons))
	comparisonByPath := make(map[string]*sQLiteComparisonRow, len(comparisons))

	for i := range comparisons {
		row := comparisons[i]
		comparisonByPath[row.Path] = &comparisons[i]

		truthStatus, ok := truthStatusByPath[row.Path]
		if !ok {
			return nil, nil, fmt.Errorf("sync: missing truth status for comparison path %q", row.Path)
		}

		view := &pathView{
			Path:        row.Path,
			TruthStatus: truthStatus,
		}
		if row.BaselinePresent {
			entry, ok := baseline.GetByPath(row.Path)
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing baseline entry for comparison path %q", row.Path)
			}
			view.Baseline = entry
		}
		if row.LocalPresent {
			localRow, ok := localByPath[row.Path]
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing local_state row for comparison path %q", row.Path)
			}
			view.Local = localStateFromSnapshotRow(&localRow)
		}
		if row.RemotePresent {
			remoteRow, ok := remoteByPath[row.Path]
			if !ok {
				return nil, nil, fmt.Errorf("sync: missing remote_state row for comparison path %q", row.Path)
			}
			view.Remote = remoteStateFromSnapshotRow(&remoteRow)
		}
		views[row.Path] = view
	}

	return views, comparisonByPath, nil
}

//nolint:gocyclo // One switch maps SQL reconciliation kinds to concrete action constructors.
func buildActionsForReconciliation(
	rec *sQLiteReconciliationRow,
	cmp *sQLiteComparisonRow,
	views map[string]*pathView,
) ([]Action, error) {
	if rec == nil || cmp == nil {
		return nil, fmt.Errorf("sync: reconciliation row requires comparison context")
	}

	view := views[rec.Path]
	if view == nil {
		return nil, fmt.Errorf("sync: missing path view for reconciliation path %q", rec.Path)
	}
	if plannerSuppressesUnavailableTruth(&view.TruthStatus) {
		return nil, nil
	}

	switch rec.ReconciliationKind {
	case "noop":
		return nil, nil
	case "baseline_remove":
		return []Action{makeAction(ActionCleanup, view)}, nil
	case "folder_create_local":
		return []Action{makeFolderCreate(view, createLocal)}, nil
	case "folder_create_remote":
		return []Action{makeFolderCreate(view, CreateRemote)}, nil
	case "upload":
		if view.Local != nil && view.Local.ItemType == ItemTypeFolder {
			return nil, fmt.Errorf("%w: upload of local folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []Action{makeAction(ActionUpload, view)}, nil
	case "download":
		if view.Remote != nil && view.Remote.ItemType == ItemTypeFolder {
			return nil, fmt.Errorf("%w: download of remote folder %s", errFolderTransferPlanned, rec.Path)
		}
		return []Action{makeAction(ActionDownload, view)}, nil
	case strLocalDelete:
		return []Action{makeAction(ActionLocalDelete, view)}, nil
	case strRemoteDelete:
		return []Action{makeAction(ActionRemoteDelete, view)}, nil
	case strBaselineUpdate:
		return []Action{makeAction(ActionBaselineUpdate, view)}, nil
	case "conflict_edit_edit":
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case "conflict_edit_delete":
		return []Action{
			makeCreateUploadAction(view),
		}, nil
	case "conflict_create_create":
		if view.Local != nil && view.Remote != nil && view.Local.ItemType != view.Remote.ItemType {
			return buildKeepBothActions(view), nil
		}
		return []Action{
			makeConflictCopyAction(view),
			makeDownloadAfterConflictCopyAction(view),
		}, nil
	case "conflict_keep_both":
		return buildKeepBothActions(view), nil
	case "replace_remote":
		return buildReplaceRemoteActions(view), nil
	case "replace_local":
		return buildReplaceLocalActions(view), nil
	case strLocalMove, strRemoteMove:
		// Moves are composed per item (planner_move_composition.go).
		return nil, nil
	default:
		return nil, fmt.Errorf("sync: unsupported reconciliation kind %q for %s", rec.ReconciliationKind, rec.Path)
	}
}

// buildKeepBothActions resolves a conflict between entries of different types
// at one path (D-059, D-059a): the local entry, file or folder, is renamed to
// a conflict copy (uploaded later as new content), then OneDrive's item takes
// the path as what it is: a folder is created locally, a file downloaded into
// the now-missing target. No content of either side is deleted.
// trimKeepBothDescendants removes the planned work below the path that the
// rename invalidates.
func buildKeepBothActions(view *pathView) []Action {
	actions := []Action{makeConflictCopyAction(view)}
	if view.Remote != nil && view.Remote.ItemType == ItemTypeFolder {
		return append(actions, makeFolderCreate(view, createLocal))
	}

	return append(actions, makeDownloadAfterConflictCopyAction(view))
}

// trimKeepBothDescendants keeps, below a keep-both path, only OneDrive's
// content arriving into the folder that takes the path (downloads and local
// folder creates, plus restores of the children that looked deleted locally)
// and baseline cleanup. Everything else there addressed the local entry the
// conflict copy moves away: uploads and local deletes of its children would run
// against paths that no longer hold them, and remote deletes of children the
// local side "lost" would delete OneDrive content the keep-both keeps. The next
// pass plans the conflict copy's content as new.
func trimKeepBothDescendants(actions []Action) []Action {
	keepBoth := keepBothPaths(actions)
	if len(keepBoth) == 0 {
		return actions
	}

	kept := actions[:0:0]
	for i := range actions {
		action := &actions[i]
		remoteIsFolder, under := keepBothAncestor(action.Path, keepBoth)
		if !under || action.Type == ActionCleanup {
			kept = append(kept, *action)
			continue
		}
		if replacement, keep := keepBothDescendantAction(action, remoteIsFolder); keep {
			kept = append(kept, replacement)
		}
	}

	return kept
}

// keepBothPaths maps each keep-both path (a conflict copy between entries of
// different types) to whether OneDrive's item there is a folder.
func keepBothPaths(actions []Action) map[string]bool {
	keepBoth := map[string]bool{}
	for i := range actions {
		action := &actions[i]
		if action.Type != ActionConflictCopy || action.View == nil || action.View.Local == nil || action.View.Remote == nil {
			continue
		}
		if action.View.Local.ItemType != action.View.Remote.ItemType {
			keepBoth[action.Path] = action.View.Remote.ItemType == ItemTypeFolder
		}
	}

	return keepBoth
}

func keepBothAncestor(p string, keepBoth map[string]bool) (remoteIsFolder bool, under bool) {
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if isFolder, ok := keepBoth[dir]; ok {
			return isFolder, true
		}
	}

	return false, false
}

// keepBothDescendantAction decides one action below a keep-both path: content
// arriving into OneDrive's folder stays, and an unchanged child of that folder
// that looks deleted locally (the local entry at the path was a file) is
// restored, not deleted online. Anything else waits for the next pass.
func keepBothDescendantAction(action *Action, remoteIsFolder bool) (Action, bool) {
	if !remoteIsFolder {
		return Action{}, false
	}
	if action.Type == ActionDownload || (action.Type == ActionFolderCreate && action.CreateSide == createLocal) {
		return *action, true
	}
	if action.Type == ActionRemoteDelete && action.View != nil && action.View.Remote != nil {
		return restoreRemoteChild(action.View), true
	}

	return Action{}, false
}

func restoreRemoteChild(view *pathView) Action {
	if view.Remote.ItemType == ItemTypeFolder {
		return makeFolderCreate(view, createLocal)
	}
	download := makeAction(ActionDownload, view)
	download.RequireMissingLocalTarget = true

	return download
}

// buildReplaceRemoteActions replaces the OneDrive item at a path whose local
// item is now of another type (or whose remote type changed while local
// content also changed): delete the old remote item, then create the local
// item as new content. buildDependencies orders the create after the delete.
//
// The create's view has no baseline or remote: it is a brand-new item, and by
// the time it dispatches the delete outcome has removed the old remote_state
// row, so remote freshness expects absence.
func buildReplaceRemoteActions(view *pathView) []Action {
	deleteAction := makeAction(ActionRemoteDelete, view)

	createView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		TruthStatus: view.TruthStatus,
	}
	if view.Local != nil && view.Local.ItemType == ItemTypeFolder {
		return []Action{deleteAction, makeFolderCreate(createView, CreateRemote)}
	}

	return []Action{deleteAction, makeCreateUploadAction(createView)}
}

// buildReplaceLocalActions replaces the local item at a path whose OneDrive
// item is now of another type while local truth is unchanged: delete the old
// local item, then materialize the remote one.
//
// The delete's view describes the old local item (resolveItemType prefers the
// remote side, which already has the new type). local_state is refreshed only
// by observation, so the create keeps the planned local row for freshness, and
// a download requires its target to be missing, as after a conflict copy.
func buildReplaceLocalActions(view *pathView) []Action {
	deleteView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		Baseline:    view.Baseline,
		TruthStatus: view.TruthStatus,
	}
	deleteAction := makeAction(ActionLocalDelete, deleteView)

	createView := &pathView{
		Path:        view.Path,
		Local:       view.Local,
		Remote:      view.Remote,
		TruthStatus: view.TruthStatus,
	}
	if view.Remote != nil && view.Remote.ItemType == ItemTypeFolder {
		return []Action{deleteAction, makeFolderCreate(createView, createLocal)}
	}

	download := makeAction(ActionDownload, createView)
	download.RequireMissingLocalTarget = true

	return []Action{deleteAction, download}
}

func partitionCurrentActionsForMode(actions []Action, mode SyncMode) ([]Action, DeferredCounts) {
	admitted := make([]Action, 0, len(actions))
	var deferred DeferredCounts
	for i := range actions {
		if actionAllowedInMode(&actions[i], mode) {
			admitted = append(admitted, actions[i])
		} else {
			deferred.AddAction(&actions[i])
		}
	}

	return admitted, deferred
}

func localStateFromSnapshotRow(row *localStateRow) *localState {
	if row == nil {
		return nil
	}

	return &localState{
		Name:             path.Base(row.Path),
		ItemType:         row.ItemType,
		Size:             row.Size,
		Hash:             row.Hash,
		Mtime:            row.Mtime,
		LocalCtime:       row.Ctime,
		LocalDevice:      row.LocalDevice,
		LocalInode:       row.LocalInode,
		LocalHasIdentity: row.LocalHasIdentity,
	}
}

func remoteStateFromSnapshotRow(row *remoteStateRow) *remoteState {
	if row == nil {
		return nil
	}

	return &remoteState{
		ItemID:    row.ItemID,
		DriveID:   row.DriveID,
		Name:      path.Base(row.Path),
		ItemType:  row.ItemType,
		Size:      row.Size,
		SizeKnown: row.SizeKnown,
		Hash:      row.Hash,
		Mtime:     row.Mtime,
		ETag:      row.ETag,
		IsDeleted: false,
	}
}
